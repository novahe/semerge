package server

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/kuasar-sandbox/accelerator/internal/util/obstat"
	"github.com/kuasar-sandbox/accelerator/pkg/cache"
	"github.com/kuasar-sandbox/accelerator/pkg/cache/wire"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
)

// CacheHandler dispatches wire frames to the appropriate backend.
//
// Sketch frequency tracking lives inside the rocks layer — the handler
// does not see or touch the sketch. Object-level opcodes (ObjectGet /
// ObjectPut) are routed through `tier`; shard-level opcodes (ShardGet /
// ShardPut) through `shard`. Either may be nil when the daemon mode
// doesn't support that opcode family, in which case the handler
// returns StatusError.
//
// Server-level counters (srvHits/srvMisses/srvFills) capture the
// external view: one RPC bumps exactly one counter. Unlike tier-level
// counts which may bump several tiers per RPC.
type CacheHandler struct {
	tier  cache.Tier
	shard cache.ShardTier

	srvHits   atomic.Uint64
	srvMisses atomic.Uint64
	srvFills  atomic.Uint64

	// Extra counters feeding the daemon's periodic adaptive stats line
	// (the Info gRPC path keeps using ServerStats above). getN/putN count
	// object+shard get/put attempts that reached the backend; bytes are the
	// payload moved (out on get hits, in on put); inflight is the gauge of
	// get/put currently executing; the histograms hold wall-clock latency.
	getN     atomic.Uint64
	putN     atomic.Uint64
	errN     atomic.Uint64
	getBytes atomic.Uint64
	putBytes atomic.Uint64
	inflight atomic.Int64
	getHist  obstat.Hist
	putHist  obstat.Hist
}

// WireStats is a richer snapshot of the handler counters for the periodic stats
// printer (a superset of ServerStats). Safe to call concurrently with requests.
type WireStats struct {
	Hits, Misses, Fills uint64
	GetN, PutN, ErrN    uint64
	GetBytes, PutBytes  uint64
	Inflight            int64
	GetHist, PutHist    obstat.HistSnapshot
}

// WireStats returns the current counter snapshot.
func (h *CacheHandler) WireStats() WireStats {
	return WireStats{
		Hits: h.srvHits.Load(), Misses: h.srvMisses.Load(), Fills: h.srvFills.Load(),
		GetN: h.getN.Load(), PutN: h.putN.Load(), ErrN: h.errN.Load(),
		GetBytes: h.getBytes.Load(), PutBytes: h.putBytes.Load(),
		Inflight: h.inflight.Load(),
		GetHist:  h.getHist.Snapshot(),
		PutHist:  h.putHist.Snapshot(),
	}
}

// NewCacheHandler wires the object-level and shard-level backends.
// Pass nil for the unused family in modes that don't support both:
//   - local / tiered: tier non-nil, shard nil
//   - shard:           tier nil, shard non-nil
func NewCacheHandler(tier cache.Tier, shard cache.ShardTier) *CacheHandler {
	return &CacheHandler{tier: tier, shard: shard}
}

// Capabilities reports protocol features that the configured backend can
// actually serve. This prevents non-tiered modes from advertising warm-only.
func (h *CacheHandler) Capabilities() byte {
	if _, ok := h.tier.(cache.Warmer); ok {
		return wire.CapabilityWarmOnly
	}
	return 0
}

// ServerStats returns a snapshot of the wire-handler counters. Safe to
// call concurrently with request handling.
func (h *CacheHandler) ServerStats() cache.ServerStats {
	return cache.ServerStats{
		Hits:   h.srvHits.Load(),
		Misses: h.srvMisses.Load(),
		Fills:  h.srvFills.Load(),
	}
}

// HandleFrame processes one request and returns a response. It never
// returns an error; business errors are encoded as StatusError
// responses.
//
// ctx carries the per-request rpc_timeout set by wire_server.
// Backends that honour context (tier chain remote hops, origin FS
// IO) will be cancelled when ctx fires; backends that don't (rocks
// CGO) run to completion regardless. The caller (wire_server) uses
// the same ctx deadline to release the connection if the response
// write itself stalls.
func (h *CacheHandler) HandleFrame(ctx context.Context, req *wire.Request) *wire.Response {
	switch req.Opcode {
	case wire.OpcodeObjectGet:
		return h.objectGet(ctx, req)
	case wire.OpcodeObjectPut:
		return h.objectPut(ctx, req)
	case wire.OpcodeShardGet:
		return h.shardGet(ctx, req)
	case wire.OpcodeShardPut:
		return h.shardPut(ctx, req)
	case wire.OpcodePing:
		return &wire.Response{Status: wire.StatusHit, Flags: h.Capabilities()}
	default:
		return &wire.Response{Status: wire.StatusError, ErrMsg: "unknown opcode"}
	}
}

// ---------------------------------------------------------------------------
// Object handlers
// ---------------------------------------------------------------------------

func (h *CacheHandler) objectGet(ctx context.Context, req *wire.Request) *wire.Response {
	if h.tier == nil {
		h.errN.Add(1)
		return &wire.Response{Status: wire.StatusError, ErrMsg: "object ops not supported in this mode"}
	}
	h.inflight.Add(1)
	defer h.inflight.Add(-1)
	if req.Flags&wire.FlagWarmOnly != 0 {
		warmer, ok := h.tier.(cache.Warmer)
		if !ok {
			h.errN.Add(1)
			return &wire.Response{Status: wire.StatusError, ErrMsg: cache.ErrWarmUnsupported.Error()}
		}
		result, err := warmer.Warm(ctx, wireNSToPartition(req.Namespace), req.Hash)
		if err != nil {
			h.errN.Add(1)
			return &wire.Response{Status: wire.StatusError, ErrMsg: err.Error()}
		}
		if result != cache.CacheHit {
			h.srvMisses.Add(1)
			return &wire.Response{Status: wire.StatusMiss}
		}
		h.srvHits.Add(1)
		return &wire.Response{Status: wire.StatusHit}
	}
	start := time.Now()
	defer func() {
		h.getN.Add(1)
		h.getHist.Record(time.Since(start))
	}()

	result, blob, err := h.tier.Get(ctx, wireNSToPartition(req.Namespace), req.Hash)
	if err != nil {
		h.errN.Add(1)
		return &wire.Response{Status: wire.StatusError, ErrMsg: err.Error()}
	}
	if result != cache.CacheHit {
		h.srvMisses.Add(1)
		return &wire.Response{Status: wire.StatusMiss}
	}
	h.srvHits.Add(1)
	h.getBytes.Add(uint64(len(blob.Bytes())))
	// Zero-copy handoff: the blob owns its own backing bytes. The
	// wire-server drops the reference via resp.Value.Release() after
	// the frame has been written. TieredCache fill-aside clones hold
	// their own refcounts and outlive this handle.
	return &wire.Response{
		Status: wire.StatusHit,
		Value:  blob,
	}
}

func (h *CacheHandler) objectPut(ctx context.Context, req *wire.Request) *wire.Response {
	if h.tier == nil {
		return &wire.Response{Status: wire.StatusError, ErrMsg: "writes not supported"}
	}
	// Non-writable tiers (e.g. tiered-mode adapter) report their
	// rejection via a marker interface before we spend cycles on the
	// empty-value guard. This preserves the legacy error message
	// tests assert on: "writes not supported" vs "empty value".
	if rw, ok := h.tier.(interface{ RejectsWrites() bool }); ok && rw.RejectsWrites() {
		h.errN.Add(1)
		return &wire.Response{Status: wire.StatusError, ErrMsg: "writes not supported"}
	}
	if len(req.Value) == 0 {
		h.errN.Add(1)
		return &wire.Response{Status: wire.StatusError, ErrMsg: "empty value"}
	}
	h.inflight.Add(1)
	start := time.Now()
	defer func() {
		h.inflight.Add(-1)
		h.putN.Add(1)
		h.putHist.Record(time.Since(start))
	}()
	if err := h.tier.Fill(ctx, wireNSToPartition(req.Namespace), req.Hash, req.Value); err != nil {
		h.errN.Add(1)
		return &wire.Response{Status: wire.StatusError, ErrMsg: err.Error()}
	}
	h.srvFills.Add(1)
	h.putBytes.Add(uint64(len(req.Value)))
	return &wire.Response{Status: wire.StatusHit}
}

// ---------------------------------------------------------------------------
// Shard handlers
// ---------------------------------------------------------------------------

func (h *CacheHandler) shardGet(ctx context.Context, req *wire.Request) *wire.Response {
	if h.shard == nil {
		h.errN.Add(1)
		return &wire.Response{Status: wire.StatusError, ErrMsg: "shard ops not supported in this mode"}
	}
	h.inflight.Add(1)
	start := time.Now()
	defer func() {
		h.inflight.Add(-1)
		h.getN.Add(1)
		h.getHist.Record(time.Since(start))
	}()
	// The shard handler is oblivious to shard idx / total — they live
	// inside the stored value and travel back untouched to the client,
	// which does the unpacking for RS decode.
	result, blob, err := h.shard.GetShard(ctx, wireNSToPartition(req.Namespace), req.Hash)
	if err != nil {
		h.errN.Add(1)
		return &wire.Response{Status: wire.StatusError, ErrMsg: err.Error()}
	}
	if result != cache.CacheHit {
		h.srvMisses.Add(1)
		return &wire.Response{Status: wire.StatusMiss}
	}
	h.srvHits.Add(1)
	h.getBytes.Add(uint64(len(blob.Bytes())))
	return &wire.Response{
		Status: wire.StatusHit,
		Value:  blob,
	}
}

func (h *CacheHandler) shardPut(ctx context.Context, req *wire.Request) *wire.Response {
	if h.shard == nil {
		h.errN.Add(1)
		return &wire.Response{Status: wire.StatusError, ErrMsg: "writes not supported"}
	}
	// Minimum value length is ShardPrefixSize (client must include the
	// [idx][total] header even for a degenerate empty shard body).
	if len(req.Value) < cache.ShardPrefixSize {
		h.errN.Add(1)
		return &wire.Response{Status: wire.StatusError, ErrMsg: "shard value missing idx/total prefix"}
	}
	h.inflight.Add(1)
	start := time.Now()
	defer func() {
		h.inflight.Add(-1)
		h.putN.Add(1)
		h.putHist.Record(time.Since(start))
	}()
	if err := h.shard.FillShard(ctx, wireNSToPartition(req.Namespace), req.Hash, req.Value); err != nil {
		h.errN.Add(1)
		return &wire.Response{Status: wire.StatusError, ErrMsg: err.Error()}
	}
	h.srvFills.Add(1)
	h.putBytes.Add(uint64(len(req.Value)))
	return &wire.Response{Status: wire.StatusHit}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// wireNSToPartition maps a wire namespace byte to its store.Partition.
// Unknown namespaces fall back to PartitionChunk.
func wireNSToPartition(ns byte) store.Partition {
	switch ns {
	case wire.NSManifest:
		return store.PartitionManifest
	case wire.NSBlob:
		return store.PartitionBlob
	default:
		return store.PartitionChunk
	}
}
