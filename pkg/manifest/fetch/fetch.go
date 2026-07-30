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
// Prefetcher is a separate optional enhancement. Keeping prefetch out of Stream
// and ChunkStream lets non-cache-backed streams implement either contract
// without acquiring a meaningless background-I/O method.
//
// RunAt, ReadAt, and Prefetch are safe for concurrent use. Close is a lifetime
// boundary: callers cancel and wait for active operations before closing.
package fetch

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"github.com/kuasar-sandbox/accelerator/pkg/cache"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/codec"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
)

// Stream is a sparse.Source with a lifetime, and a stronger contract: RunAt
// and ReadAt are safe for concurrent use at arbitrary offsets, RunAt requires
// a non-zero limit, and ReadAt MAY fetch the data chunks in a range concurrently
// (internal goroutines); see ChunkStream.ReadChunkAt for the synchronous
// counterpart.
type Stream interface {
	sparse.Source

	// Close releases resources owned by this stream (e.g. a file descriptor).
	// manifest streams own none (no-op); a layered stream closes its layers.
	// It must not run concurrently with RunAt, ReadAt, Prefetch, BorrowAt, or
	// use of a view previously returned by BorrowAt.
	Close() error
}

// BorrowingStream is an optional local-stream fast path. BorrowAt returns a
// read-only view of a contiguous Data range without copying it into a caller
// buffer. The view remains valid until Stream.Close; callers must not retain it
// beyond that lifetime, mutate it, or use it concurrently with Stream.Close.
//
// ok is false when the stream cannot borrow the requested range. Callers must
// fall back to Stream.ReadAt in that case.
type BorrowingStream interface {
	BorrowAt(offset, limit uint64) (data []byte, ok bool, err error)
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
	// region (no Hole/Zero extension) and requires a non-zero limit.
	RunChunkAt(offset, limit uint64) (kind sparse.RunKind, end, chunkIdx uint64, err error)
	// ReadChunkAt synchronously fetches+decrypts chunk chunkIdx and copies its
	// [offset, end) sub-range into buf. Used for Data (and Zero, which yields
	// zeros); never for Hole.
	ReadChunkAt(ctx context.Context, buf []byte, chunkIdx, offset, end uint64) (int, error)
}

// Prefetcher optionally warms the cache for the visible Data runs of an entire
// logical Stream. With no keys it selects every keyed and unkeyed leaf. With
// keys it selects only visible leaves carrying one of those manifest keys.
type Prefetcher interface {
	Prefetch(ctx context.Context, keys ...store.ContentKey) error
}

// PrefetchChunkStream is the optional physical-chunk prefetch capability.
// PrefetchChunkAt is synchronous and only waits for the existing cache Get/fill
// path; it does not verify, decrypt, materialize, pin, or retain the chunk.
type PrefetchChunkStream interface {
	ChunkStream
	PrefetchChunkAt(ctx context.Context, chunkIdx uint64) error
}

// ErrUnknownPrefetchLayer reports a selector key that is not structurally
// present in the Stream, including when the Stream has only unkeyed leaves.
var ErrUnknownPrefetchLayer = errors.New("fetch: unknown prefetch layer")

var errInvalidRun = errors.New("fetch: invalid run")

// manifestStream is the single-manifest implementation (Stream +
// PrefetchChunkStream + Prefetcher).
type manifestStream struct {
	m              *codec.Manifest
	onDemandGetter cache.Getter
	prefetchGetter cache.Getter
	encryptor      crypto.ChunkEncryptor
	keys           [][32]byte // decrypted per-chunk keys, parallel to m.Entries
	ownKey         store.ContentKey
	hasKey         bool
}

// NewStream constructs an unkeyed single-manifest Stream with its own request
// scheduler. keys holds one decrypted convergent key per chunk entry, in
// m.Entries order (zero entries hold the zero key, never read). Prefetch with no
// selector is supported; any non-empty selector is unknown because NewStream
// has no manifest content key. Callers holding such a key should use a Fetcher.
func NewStream(m *codec.Manifest, keys [][32]byte, c cache.Getter, enc crypto.ChunkEncryptor) Stream {
	var onDemand, prefetch cache.Getter
	if c != nil {
		client := newScheduledCacheClient(c)
		onDemand = client.OnDemandGetter()
		prefetch = client.PrefetchGetter()
	}
	return newManifestStream(m, keys, onDemand, prefetch, enc, store.ContentKey{}, false)
}

func newManifestStream(
	m *codec.Manifest,
	keys [][32]byte,
	onDemand, prefetch cache.Getter,
	enc crypto.ChunkEncryptor,
	ownKey store.ContentKey,
	hasKey bool,
) *manifestStream {
	return &manifestStream{
		m:              m,
		onDemandGetter: onDemand,
		prefetchGetter: prefetch,
		encryptor:      enc,
		keys:           keys,
		ownKey:         ownKey,
		hasKey:         hasKey,
	}
}

func (s *manifestStream) Size() uint64 { return s.m.ImageSize }
func (s *manifestStream) Close() error { return nil }

// RunChunkAt classifies one region starting at offset. By the tiling invariant
// (entries + holes cover [0, ImageSize) with no overlap or gap) a data/zero
// chunk ends exactly where the next hole begins.
func (s *manifestStream) RunChunkAt(offset, limit uint64) (sparse.RunKind, uint64, uint64, error) {
	limEnd, err := boundedRunEnd(s.m.ImageSize, offset, limit)
	if err != nil {
		return 0, 0, 0, err
	}
	if h, in := findHoleAt(s.m.Holes, offset); in {
		end := h.Offset + h.Size
		if end <= offset {
			return 0, 0, 0, fmt.Errorf("%w: hole at %d ends at %d", errInvalidRun, offset, end)
		}
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
	if end <= offset {
		return 0, 0, 0, fmt.Errorf("%w: entry %d at %d ends at %d", errInvalidRun, i, offset, end)
	}
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
	limEnd, err := boundedRunEnd(s.m.ImageSize, offset, limit)
	if err != nil {
		return 0, 0, err
	}
	for end < limEnd {
		k2, e2, _, err2 := s.RunChunkAt(end, limEnd-end)
		if err2 != nil {
			return 0, 0, err2
		}
		if k2 != kind {
			break
		}
		end = e2
	}
	return kind, end, nil
}

type loadKind uint8

const (
	loadOnDemand loadKind = iota
	loadPrefetch
)

// loadChunkAt owns all cache-result validation. A successful non-nil Blob is
// transferred to the caller, which must Release it exactly once. Every Blob
// returned on an error or non-hit path is released here.
func (s *manifestStream) loadChunkAt(ctx context.Context, chunkIdx uint64, kind loadKind) (cache.Blob, error) {
	if chunkIdx >= uint64(len(s.m.Entries)) {
		return nil, fmt.Errorf("fetch: chunk index %d out of range", chunkIdx)
	}
	e := s.m.Entries[chunkIdx]
	if e.IsZero {
		return nil, nil
	}
	var getter cache.Getter
	switch kind {
	case loadOnDemand:
		getter = s.onDemandGetter
	case loadPrefetch:
		getter = s.prefetchGetter
	default:
		return nil, fmt.Errorf("fetch: chunk %d: invalid load kind %d", chunkIdx, kind)
	}
	if getter == nil {
		return nil, fmt.Errorf("fetch: chunk %d: cache getter is nil", chunkIdx)
	}
	result, blob, err := getter.Get(ctx, store.PartitionChunk, store.ContentKey(e.CiphertextHash))
	if err != nil {
		if blob != nil {
			blob.Release()
		}
		return nil, fmt.Errorf("chunk %d: %w", chunkIdx, err)
	}
	if result != cache.CacheHit {
		if blob != nil {
			blob.Release()
		}
		return nil, fmt.Errorf("chunk %d: not found", chunkIdx)
	}
	if blob == nil {
		return nil, fmt.Errorf("chunk %d: cache hit returned nil blob", chunkIdx)
	}
	return blob, nil
}

// ReadChunkAt implements ChunkStream — synchronous single-chunk read.
func (s *manifestStream) ReadChunkAt(ctx context.Context, buf []byte, chunkIdx, offset, end uint64) (int, error) {
	if chunkIdx >= uint64(len(s.m.Entries)) {
		return 0, fmt.Errorf("fetch: chunk index %d out of range", chunkIdx)
	}
	e := s.m.Entries[chunkIdx]
	entryEnd := e.Offset + uint64(e.Size)
	if entryEnd < e.Offset {
		return 0, fmt.Errorf("fetch: chunk %d range overflows", chunkIdx)
	}
	if end < offset {
		return 0, fmt.Errorf("fetch: chunk %d: end %d before offset %d", chunkIdx, end, offset)
	}
	if offset < e.Offset || end > entryEnd {
		return 0, fmt.Errorf("fetch: chunk %d: range [%d,%d) outside entry [%d,%d)", chunkIdx, offset, end, e.Offset, entryEnd)
	}
	want := end - offset
	if uint64(len(buf)) < want {
		return 0, fmt.Errorf("fetch: chunk %d: buffer has %d bytes, need %d", chunkIdx, len(buf), want)
	}
	if want == 0 {
		return 0, nil
	}
	if e.IsZero {
		clearSlice(buf[:int(want)])
		return int(want), nil
	}
	if chunkIdx >= uint64(len(s.keys)) {
		return 0, fmt.Errorf("fetch: chunk %d: missing decryption key", chunkIdx)
	}
	if s.encryptor == nil {
		return 0, fmt.Errorf("fetch: chunk %d: decryptor is nil", chunkIdx)
	}

	blob, err := s.loadChunkAt(ctx, chunkIdx, loadOnDemand)
	if err != nil {
		return 0, err
	}
	defer blob.Release()
	ciphertext := blob.Bytes()
	// The content key is SHA256(ciphertext) and is authenticated by the key
	// table's AAD, so verifying the returned bytes against it rejects a corrupt
	// or tampered chunk before the unauthenticated AES-CTR decrypt would turn
	// attacker-chosen ciphertext into attacker-chosen plaintext. Hash the bytes
	// as received — DecryptInPlace mutates the buffer in place.
	if sha256.Sum256(ciphertext) != e.CiphertextHash {
		return 0, fmt.Errorf("chunk %d: ciphertext hash mismatch (corrupt or tampered store/cache)", chunkIdx)
	}
	plain, err := s.encryptor.DecryptInPlace(s.keys[chunkIdx], ciphertext)
	if err != nil {
		return 0, fmt.Errorf("chunk %d: decrypt: %w", chunkIdx, err)
	}
	if uint64(len(plain)) < uint64(e.Size) {
		return 0, fmt.Errorf("chunk %d: decrypted data has %d bytes, need %d", chunkIdx, len(plain), e.Size)
	}
	lo := int(offset - e.Offset)
	hi := int(end - e.Offset)
	copy(buf[:int(want)], plain[lo:hi])
	return int(want), nil
}

// PrefetchChunkAt implements PrefetchChunkStream. It intentionally does no
// Blob inspection: a successful Get followed by exactly one Release is the
// complete physical prefetch operation.
func (s *manifestStream) PrefetchChunkAt(ctx context.Context, chunkIdx uint64) error {
	blob, err := s.loadChunkAt(ctx, chunkIdx, loadPrefetch)
	if err != nil {
		return err
	}
	if blob != nil {
		blob.Release()
	}
	return nil
}

func (s *manifestStream) Prefetch(ctx context.Context, keys ...store.ContentKey) error {
	return prefetchStream(ctx, s, keys)
}

// ReadAt walks the runs and fetches data chunks concurrently — one goroutine
// per Data run, each writing directly into its buf slice; Hole/Zero are
// zero-filled in place. ctx cancel on early return releases in-flight blobs.
func (s *manifestStream) ReadAt(ctx context.Context, buf []byte, offset uint64) (int, error) {
	return readResolvedAt(ctx, s, buf, offset)
}

func boundedRunEnd(size, offset, limit uint64) (uint64, error) {
	if offset >= size {
		return 0, io.EOF
	}
	if limit == 0 {
		return 0, fmt.Errorf("%w: zero limit at offset %d", errInvalidRun, offset)
	}
	if limit >= size-offset {
		return size, nil
	}
	return offset + limit, nil
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
