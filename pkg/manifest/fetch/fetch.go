// Package fetch implements the unified read layer for the container accelerator.
//
// A Stream is the read-base abstraction: it reconstructs a virtual image from
// either a chunked + encrypted manifest, a local file, or an overlay of several
// such streams. Stream is sparse.Source plus lifetime — it strengthens the
// Source baseline contract (monotone single-goroutine reads) to full concurrent
// random access, which the runtime consumers (vhost, uffd) rely on. Three
// implementations satisfy it — manifestStream (also implements ChunkStream),
// the file stream returned by OpenFileStream, and layeredStream — and a sandbox
// disk, snapshot bundle, or manifest-ctl read all sit on the same abstraction.
//
// Run classification uses sparse.RunKind. Only manifest-backed streams ever
// return sparse.Zero (an IsZero chunk in the serving layer — explicit zero
// data that needs no fetch but does NOT fall through an overlay); file streams
// classify allocated zeros as Data and filesystem holes as Hole.
//
// ChunkStream is an optional enhancement a Stream may implement so a caller can
// thread a chunk index between classification (RunChunkAt) and read
// (ReadChunkAt), skipping a re-lookup. Its only in-tree users are manifestStream
// itself (RunAt/ReadAt are built on the chunk primitives) and layeredStream
// (which threads a serving layer's chunk index from resolve to read). External
// consumers use the plain Stream methods.
//
// All methods are safe for concurrent use.
package fetch

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"sync"

	"github.com/kuasar-sandbox/accelerator/pkg/cache"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/codec"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
)

// Stream is a sparse.Source with a lifetime, and a stronger contract: RunAt
// and ReadAt are safe for concurrent use at arbitrary offsets, and ReadAt MAY
// fetch the data chunks in a range concurrently (internal goroutines); see
// ChunkStream.ReadChunkAt for the synchronous counterpart.
type Stream interface {
	sparse.Source

	// Close releases resources owned by this stream (e.g. a file descriptor).
	// manifest streams own none (no-op); a layered stream closes its layers.
	Close() error
}

// ChunkStream is the optional enhancement contract: it lets a caller reuse a
// chunk index between classification and read, avoiding a second offset lookup.
//
// Concurrency convention: ReadChunkAt is ALWAYS synchronous and runs in the
// calling goroutine — it spawns no goroutines. Batch parallelism is Stream.ReadAt's
// job. A caller wanting concurrency invokes ReadChunkAt from its own goroutines.
type ChunkStream interface {
	// RunChunkAt is RunAt that additionally returns the serving chunk index
	// (valid for Data and Zero; meaningless for Hole). It returns a single
	// region (no Hole/Zero extension).
	RunChunkAt(offset, limit uint64) (kind sparse.RunKind, end, chunkIdx uint64, err error)
	// ReadChunkAt synchronously fetches+decrypts chunk chunkIdx and copies its
	// [offset, end) sub-range into buf. Used for Data (and Zero, which yields
	// zeros); never for Hole.
	ReadChunkAt(ctx context.Context, buf []byte, chunkIdx, offset, end uint64) (int, error)
}

// WarmChunkRef identifies one stored ciphertext chunk in a single manifest.
// Offset and End are the full logical extent of the chunk, even when the
// caller's WalkDataChunks range intersects only part of it.
type WarmChunkRef struct {
	Offset          uint64
	End             uint64
	CiphertextBytes uint64
	Key             store.ContentKey
}

// WarmableChunkStream is an optional enhancement implemented only by a
// single-manifest stream backed by a cache client that supports warm-only
// requests. A layered stream deliberately does not expose this contract:
// chunk indices and content membership belong to individual layers.
//
// WalkDataChunks emits every non-zero Data entry intersecting [start, end), in
// logical-offset order. Equal content keys in different entries are emitted
// separately; callers that warm by content key are responsible for deduping.
type WarmableChunkStream interface {
	Stream
	WalkDataChunks(start, end uint64, fn func(WarmChunkRef) error) error
	DataChunkAt(offset uint64) (WarmChunkRef, bool)
	WarmSupported() bool
	WarmChunk(ctx context.Context, ref WarmChunkRef) error
}

// manifestStream is the single-manifest implementation (Stream + ChunkStream).
type manifestStream struct {
	m         *codec.Manifest
	cache     cache.Getter
	encryptor crypto.ChunkEncryptor
	keys      [][32]byte // decrypted per-chunk keys, parallel to m.Entries
}

// NewStream constructs a single-manifest Stream. keys holds one decrypted
// convergent key per chunk entry, in m.Entries order (zero entries hold the
// zero key, never read). Callers holding a hex key but not a Manifest should use
// a Fetcher (NewFetcher).
func NewStream(m *codec.Manifest, keys [][32]byte, c cache.Getter, enc crypto.ChunkEncryptor) Stream {
	return &manifestStream{m: m, cache: c, encryptor: enc, keys: keys}
}

func (s *manifestStream) Size() uint64 { return s.m.ImageSize }
func (s *manifestStream) Close() error { return nil }

// WalkDataChunks enumerates the self manifest's stored Data chunks that
// intersect [start, end). It never resolves parent layers, fetches data, or
// emits explicit zero chunks and holes.
func (s *manifestStream) WalkDataChunks(start, end uint64, fn func(WarmChunkRef) error) error {
	if fn == nil {
		return fmt.Errorf("walk data chunks: nil callback")
	}
	if start >= end || start >= s.m.ImageSize {
		return nil
	}
	if end > s.m.ImageSize {
		end = s.m.ImageSize
	}

	for _, entry := range s.m.Entries {
		entryEnd := entry.Offset + uint64(entry.Size)
		if entryEnd <= start {
			continue
		}
		if entry.Offset >= end {
			break
		}
		if entry.IsZero {
			continue
		}
		if err := fn(WarmChunkRef{
			Offset:          entry.Offset,
			End:             entryEnd,
			CiphertextBytes: uint64(entry.Size) + 1, // AES-CTR flag byte + payload.
			Key:             store.ContentKey(entry.CiphertextHash),
		}); err != nil {
			return err
		}
	}
	return nil
}

// DataChunkAt returns the single-manifest Data chunk containing offset. Holes,
// explicit zero chunks, and out-of-range offsets have no warmable chunk.
func (s *manifestStream) DataChunkAt(offset uint64) (WarmChunkRef, bool) {
	if offset >= s.m.ImageSize {
		return WarmChunkRef{}, false
	}
	i := codec.ChunkIndexForOffset(s.m.Entries, offset)
	if i < 0 {
		return WarmChunkRef{}, false
	}
	entry := s.m.Entries[i]
	if entry.IsZero || offset < entry.Offset || offset >= entry.Offset+uint64(entry.Size) {
		return WarmChunkRef{}, false
	}
	return WarmChunkRef{
		Offset:          entry.Offset,
		End:             entry.Offset + uint64(entry.Size),
		CiphertextBytes: uint64(entry.Size) + 1, // AES-CTR flag byte + payload.
		Key:             store.ContentKey(entry.CiphertextHash),
	}, true
}

// WarmSupported reports whether the backing cache implements the optional
// warm-only operation. Store-origin fallback getters intentionally return
// false because fetching without filling a cache would provide no benefit.
func (s *manifestStream) WarmSupported() bool {
	_, warmerOK := s.cache.(cache.Warmer)
	capability, capabilityOK := s.cache.(cache.WarmCapability)
	return warmerOK && capabilityOK && capability.WarmSupported()
}

// WarmChunk asks the cache to make ref's ciphertext ready without returning
// its payload to the manifest reader. Hash verification and decryption remain
// on the demand ReadAt path.
func (s *manifestStream) WarmChunk(ctx context.Context, ref WarmChunkRef) error {
	if !s.WarmSupported() {
		return fmt.Errorf("warm chunk: %w", cache.ErrWarmUnsupported)
	}
	w := s.cache.(cache.Warmer)
	result, err := w.Warm(ctx, store.PartitionChunk, ref.Key)
	if err != nil {
		return fmt.Errorf("warm chunk %x: %w", ref.Key, err)
	}
	if result != cache.CacheHit {
		return fmt.Errorf("warm chunk %x: not found", ref.Key)
	}
	return nil
}

// RunChunkAt classifies one region starting at offset. By the tiling invariant
// (entries + holes cover [0, ImageSize) with no overlap or gap) a data/zero
// chunk ends exactly where the next hole begins.
func (s *manifestStream) RunChunkAt(offset, limit uint64) (sparse.RunKind, uint64, uint64, error) {
	if offset >= s.m.ImageSize {
		return 0, 0, 0, io.EOF
	}
	limEnd := offset + limit
	if limEnd < offset || limEnd > s.m.ImageSize { // overflow or past EOF
		limEnd = s.m.ImageSize
	}
	if h, in := findHoleAt(s.m.Holes, offset); in {
		end := h.Offset + h.Size
		if end > limEnd {
			end = limEnd
		}
		return sparse.Hole, end, 0, nil
	}
	i := codec.ChunkIndexForOffset(s.m.Entries, offset)
	if i < 0 {
		return sparse.Hole, limEnd, 0, nil // unreachable in bounds (tiling); degrade safely
	}
	e := s.m.Entries[i]
	end := e.Offset + uint64(e.Size)
	if end > limEnd {
		end = limEnd
	}
	if e.IsZero {
		return sparse.Zero, end, uint64(i), nil
	}
	return sparse.Data, end, uint64(i), nil
}

// RunAt extends Hole/Zero runs through contiguous same-kind regions; a Data run
// stays a single chunk.
func (s *manifestStream) RunAt(offset, limit uint64) (sparse.RunKind, uint64, error) {
	kind, end, _, err := s.RunChunkAt(offset, limit)
	if err != nil || kind == sparse.Data {
		return kind, end, err
	}
	limEnd := offset + limit
	if limEnd < offset || limEnd > s.m.ImageSize {
		limEnd = s.m.ImageSize
	}
	for end < limEnd {
		k2, e2, _, err2 := s.RunChunkAt(end, limEnd-end)
		if err2 != nil || k2 != kind {
			break
		}
		end = e2
	}
	return kind, end, nil
}

// readChunkInto is the single synchronous fetch path: fetch chunk chunkIdx,
// decrypt in place, copy its [offset, end) sub-range into dst. It spawns no
// goroutines; ReadChunkAt calls it directly, ReadAt calls it from workers.
func (s *manifestStream) readChunkInto(ctx context.Context, dst []byte, chunkIdx, offset, end uint64) error {
	e := s.m.Entries[chunkIdx]
	if e.IsZero {
		clearSlice(dst)
		return nil
	}
	result, blob, err := s.cache.Get(ctx, store.PartitionChunk, store.ContentKey(e.CiphertextHash))
	if err != nil {
		return fmt.Errorf("chunk %d: %w", chunkIdx, err)
	}
	if result != cache.CacheHit {
		return fmt.Errorf("chunk %d: not found", chunkIdx)
	}
	// The content key is SHA256(ciphertext) and is authenticated by the key
	// table's AAD, so verifying the returned bytes against it rejects a corrupt
	// or tampered chunk before the unauthenticated AES-CTR decrypt would turn
	// attacker-chosen ciphertext into attacker-chosen plaintext. Hash the bytes
	// as received — DecryptInPlace mutates the buffer in place.
	if sha256.Sum256(blob.Bytes()) != e.CiphertextHash {
		blob.Release()
		return fmt.Errorf("chunk %d: ciphertext hash mismatch (corrupt or tampered store/cache)", chunkIdx)
	}
	plain, err := s.encryptor.DecryptInPlace(s.keys[chunkIdx], blob.Bytes())
	if err != nil {
		blob.Release()
		return fmt.Errorf("chunk %d: decrypt: %w", chunkIdx, err)
	}
	copy(dst, plain[offset-e.Offset:end-e.Offset])
	blob.Release()
	return nil
}

// ReadChunkAt implements ChunkStream — synchronous single-chunk read.
func (s *manifestStream) ReadChunkAt(ctx context.Context, buf []byte, chunkIdx, offset, end uint64) (int, error) {
	if err := s.readChunkInto(ctx, buf, chunkIdx, offset, end); err != nil {
		return 0, err
	}
	return int(end - offset), nil
}

// ReadAt walks the runs and fetches data chunks concurrently — one goroutine
// per Data run, each writing directly into its buf slice; Hole/Zero are
// zero-filled in place. ctx cancel on early return releases in-flight blobs.
func (s *manifestStream) ReadAt(ctx context.Context, buf []byte, offset uint64) (int, error) {
	if offset >= s.m.ImageSize {
		return 0, io.EOF
	}
	if len(buf) == 0 {
		return 0, nil
	}
	end := offset + uint64(len(buf))
	var eof error
	if end > s.m.ImageSize {
		end = s.m.ImageSize
		eof = io.EOF
	}

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	var once sync.Once

	for cur := offset; cur < end; {
		kind, runEnd, idx, _ := s.RunChunkAt(cur, end-cur)
		dst := buf[cur-offset : runEnd-offset]
		if kind == sparse.Data {
			wg.Add(1)
			go func(idx, lo, hi uint64, dst []byte) {
				defer wg.Done()
				if err := s.readChunkInto(cctx, dst, idx, lo, hi); err != nil {
					once.Do(func() { errCh <- err; cancel() })
				}
			}(idx, cur, runEnd, dst)
		} else { // Hole | Zero
			clearSlice(dst)
		}
		cur = runEnd
	}

	wg.Wait()
	select {
	case err := <-errCh:
		return 0, err
	default:
	}
	return int(end - offset), eof
}

// findHoleAt returns the hole extent containing offset, or ok=false. The hole
// slice is sorted by Offset and disjoint. O(log M). (manifestStream owns its
// lookup — the manifest's holes live next to chunk entries, not behind a
// sparse source.)
func findHoleAt(holes []sparse.Extent, offset uint64) (sparse.Extent, bool) {
	lo, hi := 0, len(holes)-1
	idx := -1
	for lo <= hi {
		mid := lo + (hi-lo)/2
		if holes[mid].Offset <= offset {
			idx = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	if idx < 0 {
		return sparse.Extent{}, false
	}
	h := holes[idx]
	if offset < h.Offset+h.Size {
		return h, true
	}
	return sparse.Extent{}, false
}

// clearSlice zeros every byte in b (the compiler lowers this to memclr).
func clearSlice(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
