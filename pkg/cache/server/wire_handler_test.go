package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/cache"
	"github.com/kuasar-sandbox/accelerator/pkg/cache/wire"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
)

// roTier presents an in-memory Getter as a read-only cache.Tier,
// mirroring how store-ctl serves the wire protocol over its backend:
// Get passes through, Fill is rejected via the RejectsWrites marker.
type roTier struct{ cache.Getter }

func (roTier) Fill(context.Context, store.Partition, store.ContentKey, []byte) error {
	return errors.New("read-only")
}
func (roTier) RejectsWrites() bool { return true }

// mapGetter is a partition-keyed in-memory Getter for handler tests.
type mapGetter map[store.Partition]map[store.ContentKey][]byte

func (m mapGetter) Get(_ context.Context, p store.Partition, k store.ContentKey) (cache.CacheResult, cache.Blob, error) {
	if byKey, ok := m[p]; ok {
		if v, ok := byKey[k]; ok {
			return cache.CacheHit, cache.NewMemBlob(v), nil
		}
	}
	return cache.CacheMiss, nil, nil
}

type warmTier struct {
	result cache.CacheResult
	err    error
	calls  atomic.Int32
}

func (t *warmTier) Get(context.Context, store.Partition, store.ContentKey) (cache.CacheResult, cache.Blob, error) {
	return cache.CacheMiss, nil, nil
}
func (t *warmTier) Fill(context.Context, store.Partition, store.ContentKey, []byte) error { return nil }
func (t *warmTier) Warm(context.Context, store.Partition, store.ContentKey) (cache.CacheResult, error) {
	t.calls.Add(1)
	return t.result, t.err
}

func TestWireHandlerWarmOnlyReturnsNoPayloadAndAdvertisesCapability(t *testing.T) {
	tier := &warmTier{result: cache.CacheHit}
	h := NewCacheHandler(tier, nil)
	if got := h.Capabilities(); got&wire.CapabilityWarmOnly == 0 {
		t.Fatalf("capabilities=%#x, want warm-only", got)
	}
	resp := h.HandleFrame(context.Background(), &wire.Request{
		Opcode: wire.OpcodeObjectGet, Namespace: wire.NSChunk,
		Flags: wire.FlagWarmOnly, Hash: mkKey("warm"),
	})
	if resp.Status != wire.StatusHit || resp.Value != nil {
		t.Fatalf("warm response status=%d value=%v", resp.Status, resp.Value)
	}
	if tier.calls.Load() != 1 {
		t.Fatalf("warm calls=%d, want 1", tier.calls.Load())
	}
	stats := h.WireStats()
	if stats.GetN != 0 || stats.GetHist.Count != 0 {
		t.Fatalf("warm-only polluted demand Get stats: count=%d hist=%d", stats.GetN, stats.GetHist.Count)
	}

	plain := NewCacheHandler(roTier{mapGetter{}}, nil)
	if got := plain.Capabilities(); got != 0 {
		t.Fatalf("plain backend advertised capabilities %#x", got)
	}
	unsupported := plain.HandleFrame(context.Background(), &wire.Request{
		Opcode: wire.OpcodeObjectGet, Flags: wire.FlagWarmOnly,
	})
	if unsupported.Status != wire.StatusError {
		t.Fatalf("unsupported warm status=%d, want error", unsupported.Status)
	}
}

func mkKey(s string) store.ContentKey { return store.ContentKey(sha256.Sum256([]byte(s))) }

// TestWireHandlerNamespaceRouting proves the wire handler routes each
// wire namespace to the matching store.Partition (including blob =
// 0x03), keeps namespaces isolated, and that a read-only tier rejects
// ObjectPut with the canonical error status.
func TestWireHandlerNamespaceRouting(t *testing.T) {
	kChunk, kManifest, kBlob := mkKey("c"), mkKey("m"), mkKey("b")
	backend := mapGetter{
		store.PartitionChunk:    {kChunk: []byte("chunk-bytes")},
		store.PartitionManifest: {kManifest: []byte("manifest-bytes")},
		store.PartitionBlob:     {kBlob: []byte("blob-bytes")},
	}
	h := NewCacheHandler(roTier{backend}, nil)
	ctx := context.Background()

	cases := []struct {
		name string
		ns   byte
		key  store.ContentKey
		want string
	}{
		{"chunk", wire.NSChunk, kChunk, "chunk-bytes"},
		{"manifest", wire.NSManifest, kManifest, "manifest-bytes"},
		{"blob", wire.NSBlob, kBlob, "blob-bytes"},
	}
	for _, c := range cases {
		resp := h.HandleFrame(ctx, &wire.Request{Opcode: wire.OpcodeObjectGet, Namespace: c.ns, Hash: c.key})
		if resp.Status != wire.StatusHit {
			t.Fatalf("%s: ObjectGet status=%d want hit", c.name, resp.Status)
		}
		if string(resp.Value.Bytes()) != c.want {
			t.Fatalf("%s: got %q want %q", c.name, resp.Value.Bytes(), c.want)
		}
		resp.Value.Release()
	}

	// Namespace isolation: a blob key requested under the chunk namespace
	// must miss (proves NSBlob != NSChunk routing).
	miss := h.HandleFrame(ctx, &wire.Request{Opcode: wire.OpcodeObjectGet, Namespace: wire.NSChunk, Hash: kBlob})
	if miss.Status != wire.StatusMiss {
		t.Fatalf("cross-namespace lookup: status=%d want miss", miss.Status)
	}

	// Read-only: ObjectPut is rejected (writes go through the store gRPC).
	put := h.HandleFrame(ctx, &wire.Request{Opcode: wire.OpcodeObjectPut, Namespace: wire.NSBlob, Hash: kBlob, Value: []byte("x")})
	if put.Status != wire.StatusError {
		t.Fatalf("ObjectPut on read-only tier: status=%d want error", put.Status)
	}
}
