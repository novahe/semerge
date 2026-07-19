package fetch

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/cache"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/codec"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
)

func warmingManifest() *codec.Manifest {
	return &codec.Manifest{
		Version:   codec.Version1,
		ImageSize: 20,
		Entries: []codec.ChunkEntry{
			{Offset: 0, Size: 4, CiphertextHash: store.ContentKey{0x11}},
			{Offset: 4, Size: 4, IsZero: true},
			{Offset: 12, Size: 4, CiphertextHash: store.ContentKey{0x11}},
			{Offset: 16, Size: 4, CiphertextHash: store.ContentKey{0x33}},
		},
		Holes: []sparse.Extent{{Offset: 8, Size: 4}},
	}
}

func TestWalkDataChunksRangeAndKinds(t *testing.T) {
	s := NewStream(warmingManifest(), make([][32]byte, 4), nil, nil)
	warmable, ok := s.(WarmableChunkStream)
	if !ok {
		t.Fatal("single manifest stream does not implement WarmableChunkStream")
	}

	var got []WarmChunkRef
	err := warmable.WalkDataChunks(2, 16, func(ref WarmChunkRef) error {
		got = append(got, ref)
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDataChunks: %v", err)
	}
	want := []WarmChunkRef{
		{Offset: 0, End: 4, CiphertextBytes: 5, Key: store.ContentKey{0x11}},
		{Offset: 12, End: 16, CiphertextBytes: 5, Key: store.ContentKey{0x11}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WalkDataChunks = %#v, want %#v", got, want)
	}
}

func TestWalkDataChunksBoundariesAndDuplicateKeys(t *testing.T) {
	s := NewStream(warmingManifest(), make([][32]byte, 4), nil, nil).(WarmableChunkStream)

	var got []WarmChunkRef
	if err := s.WalkDataChunks(4, 100, func(ref WarmChunkRef) error {
		got = append(got, ref)
		return nil
	}); err != nil {
		t.Fatalf("WalkDataChunks: %v", err)
	}
	want := []WarmChunkRef{
		{Offset: 12, End: 16, CiphertextBytes: 5, Key: store.ContentKey{0x11}},
		{Offset: 16, End: 20, CiphertextBytes: 5, Key: store.ContentKey{0x33}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WalkDataChunks = %#v, want %#v", got, want)
	}

	// Equal content keys are separate logical entries. The sandbox warmer owns
	// content-key deduplication so this enumeration preserves both occurrences.
	var duplicateKeys int
	if err := s.WalkDataChunks(0, 20, func(ref WarmChunkRef) error {
		if ref.Key == (store.ContentKey{0x11}) {
			duplicateKeys++
		}
		return nil
	}); err != nil {
		t.Fatalf("WalkDataChunks duplicates: %v", err)
	}
	if duplicateKeys != 2 {
		t.Fatalf("duplicate key count = %d, want 2", duplicateKeys)
	}

	called := false
	if err := s.WalkDataChunks(20, 20, func(WarmChunkRef) error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("empty WalkDataChunks: %v", err)
	}
	if called {
		t.Fatal("empty range invoked callback")
	}
}

func TestDataChunkAtSkipsZeroHoleAndEOF(t *testing.T) {
	s := NewStream(warmingManifest(), make([][32]byte, 4), nil, nil).(WarmableChunkStream)

	for _, off := range []uint64{4, 7, 8, 11, 20} {
		if ref, ok := s.DataChunkAt(off); ok {
			t.Fatalf("DataChunkAt(%d) = %#v, true; want false", off, ref)
		}
	}

	want := WarmChunkRef{Offset: 12, End: 16, CiphertextBytes: 5, Key: store.ContentKey{0x11}}
	for _, off := range []uint64{12, 15} {
		if ref, ok := s.DataChunkAt(off); !ok || ref != want {
			t.Fatalf("DataChunkAt(%d) = (%#v, %v), want (%#v, true)", off, ref, ok, want)
		}
	}
	if ref, ok := s.DataChunkAt(16); !ok || ref.Offset != 16 || ref.End != 20 {
		t.Fatalf("DataChunkAt(16) = (%#v, %v), want next data entry", ref, ok)
	}
}

type recordingWarmer struct {
	result  cache.CacheResult
	err     error
	enabled bool
	parts   []store.Partition
	keys    []store.ContentKey
}

func (w *recordingWarmer) Get(context.Context, store.Partition, store.ContentKey) (cache.CacheResult, cache.Blob, error) {
	return cache.CacheMiss, nil, nil
}

func (w *recordingWarmer) Warm(_ context.Context, p store.Partition, key store.ContentKey) (cache.CacheResult, error) {
	w.parts = append(w.parts, p)
	w.keys = append(w.keys, key)
	return w.result, w.err
}

func (w *recordingWarmer) WarmSupported() bool { return w.enabled }

func TestWarmChunkCapabilityAndResult(t *testing.T) {
	ref := WarmChunkRef{Offset: 12, End: 16, Key: store.ContentKey{0x44}}
	unsupported := NewStream(warmingManifest(), make([][32]byte, 4), &staticGetter{}, nil).(WarmableChunkStream)
	if unsupported.WarmSupported() {
		t.Fatal("plain Getter unexpectedly supports warming")
	}
	if err := unsupported.WarmChunk(context.Background(), ref); !errors.Is(err, cache.ErrWarmUnsupported) {
		t.Fatalf("WarmChunk with plain Getter error = %v, want ErrWarmUnsupported", err)
	}

	w := &recordingWarmer{result: cache.CacheHit, enabled: true}
	supported := NewStream(warmingManifest(), make([][32]byte, 4), w, nil).(WarmableChunkStream)
	if !supported.WarmSupported() {
		t.Fatal("warm-capable Getter not detected")
	}
	w.enabled = false
	if err := supported.WarmChunk(context.Background(), ref); !errors.Is(err, cache.ErrWarmUnsupported) {
		t.Fatalf("WarmChunk disabled capability error = %v, want ErrWarmUnsupported", err)
	}
	w.enabled = true
	if err := supported.WarmChunk(context.Background(), ref); err != nil {
		t.Fatalf("WarmChunk hit: %v", err)
	}
	if !reflect.DeepEqual(w.parts, []store.Partition{store.PartitionChunk}) ||
		!reflect.DeepEqual(w.keys, []store.ContentKey{ref.Key}) {
		t.Fatalf("Warm calls = (%v, %v), want chunk/%x", w.parts, w.keys, ref.Key)
	}

	w.result = cache.CacheMiss
	if err := supported.WarmChunk(context.Background(), ref); err == nil {
		t.Fatal("WarmChunk miss succeeded")
	}
	w.err = errors.New("transport")
	if err := supported.WarmChunk(context.Background(), ref); err == nil || !errors.Is(err, w.err) {
		t.Fatalf("WarmChunk error = %v, want wrapped transport error", err)
	}
}

func TestLayeredStreamDoesNotExposeWarming(t *testing.T) {
	w := &recordingWarmer{result: cache.CacheHit, enabled: true}
	top := NewStream(warmingManifest(), make([][32]byte, 4), w, nil)
	bottom := NewStream(warmingManifest(), make([][32]byte, 4), w, nil)
	if _, ok := NewLayered(top, bottom).(WarmableChunkStream); ok {
		t.Fatal("multi-layer stream unexpectedly exposes WarmableChunkStream")
	}
}
