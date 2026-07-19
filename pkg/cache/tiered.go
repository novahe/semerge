package cache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/store"
)

// CacheResult distinguishes the three lookup outcomes from a cache layer.
type CacheResult int

const (
	CacheHit     CacheResult = iota // hit, blob valid
	CacheMiss                       // miss, caller should try the next layer
	CacheHitMiss                    // negative cache hit ("known absent"), terminate cascade
)

// Getter is the read-side contract implemented by every cache layer.
//
// Get returns a cache.Blob on hit. The caller owns the returned handle
// and MUST Release it exactly once. If the caller needs to hand the
// blob to a goroutine that outlives the sync path, it must first
// Clone the handle.
type Getter interface {
	Get(ctx context.Context, p store.Partition, key store.ContentKey) (CacheResult, Blob, error)
}

// Warmer fetches an object into a ready cache tier without returning its
// payload. A CacheHit means a later Get can read the object from an upper
// cache tier; it does not merely mean that the origin returned the data.
type Warmer interface {
	Warm(ctx context.Context, p store.Partition, key store.ContentKey) (CacheResult, error)
}

// WarmCapability lets callers avoid starting background work against an old
// wire server that does not implement warm-only requests.
type WarmCapability interface {
	WarmSupported() bool
}

var ErrWarmUnsupported = errors.New("cache: warm-only is not supported")

// Filler is the write-side contract: a tier that supports backfill.
//
// Fill receives raw bytes, not an io.Reader. The `data` slice is valid
// only for the synchronous duration of Fill — implementations that
// need to retain bytes for async work (fire-and-forget fan-out, etc.)
// must copy before returning.
type Filler interface {
	Fill(ctx context.Context, p store.Partition, key store.ContentKey, data []byte) error
}

// Tier is a complete cache layer that supports both lookup and fill.
// It is the object-level read+write contract: Getter + Filler.
type Tier interface {
	Getter
	Filler
}

// ShardGetter is the read-side contract for shard-level storage, where
// a single object is split into N erasure-coded shards.
//
// A shard's (idx, total) is encoded in the first ShardPrefixSize bytes
// of the returned Blob (see shard_value.go); the reader/router does
// NOT tell the peer which idx to fetch. This decouples shard identity
// from cluster membership: a peer serves whatever shard it holds,
// and the EC client accepts any K of N distinct idx.
type ShardGetter interface {
	GetShard(ctx context.Context, p store.Partition, key store.ContentKey) (CacheResult, Blob, error)
}

// ShardFiller is the write-side contract for shard-level storage.
//
// value must be pre-formatted as [idx][total][shard_data] — see
// EncodeShardPrefix. The store writes value verbatim; it does not
// interpret the prefix. This lets the hot path avoid any intermediate
// copy between the RS encoder's output buffer and rocksdb.
type ShardFiller interface {
	FillShard(ctx context.Context, p store.Partition, key store.ContentKey, value []byte) error
}

// ShardTier is the complete shard-level read+write contract.
type ShardTier interface {
	ShardGetter
	ShardFiller
}

// Semaphore is the optional concurrency-control capability.
//
// A tier or origin that implements Semaphore is rate-limited; on Acquire wait
// the caller restarts the search from layer 0 (which may have been filled
// during the wait).
type Semaphore interface {
	Acquire(ctx context.Context) (waited bool, err error)
	Release()
}

// FillMiss is the optional negative-cache filling capability.
//
// When all layers miss, TieredCache calls FillMiss on every tier that
// implements it so the negative result can be remembered.
type FillMiss interface {
	FillMiss(ctx context.Context, p store.Partition, key store.ContentKey) error
}

// Repairable is optionally implemented by tiers that can backfill
// reconstructed data after a partial Get (e.g., EC fills the shards
// that were confirmed missing after a successful reconstruction).
//
// Repair is opt-in: a tier defaults to not repairing until EnableRepair
// is called with a non-nil runner. The runner is invoked once per
// repair operation; implementations MUST ensure the thunk runs exactly
// once.
//
// TieredCache auto-calls EnableRepair on tiers that implement this, so
// repair goroutines participate in graceful shutdown.
type Repairable interface {
	EnableRepair(runner func(fn func()))
}

// fillCtx returns the context for an async fill goroutine: always a
// child of baseCtx (so Close cancels it — no leak on a wedged origin),
// with the optional fillTimeout deadline applied only when > 0.
func (tc *TieredCache) fillCtx() (context.Context, context.CancelFunc) {
	if tc.fillTimeout > 0 {
		return context.WithTimeout(tc.baseCtx, tc.fillTimeout)
	}
	return context.WithCancel(tc.baseCtx)
}

// Close cancels and drains all in-flight async fill/repair goroutines.
// Call it after the serving path has stopped admitting new requests.
func (tc *TieredCache) Close() {
	if tc.baseCancel != nil {
		tc.baseCancel()
	}
	tc.leaders.Wait()
	for i := range tc.fillInflight {
		tc.fillInflight[i].Wait()
	}
}

// TieredCache composes cache tiers and an origin into a layered lookup.
//
// Counters are per-tier parallel slices (hits / misses / fills) plus
// origin-level scalars. The slices are pre-allocated to len(tiers) and
// never grow, so the embedded atomic.Uint64 values are safe to address
// by index — no mutex, no pointer indirection. Counters feed the Info
// gRPC service via Counters() and are the only Go-level stats exposed
// by TieredCache; see pkg/cache/stats.go for the wire shape.
type TieredCache struct {
	tiers        []Tier
	origin       Getter
	fillInflight []sync.WaitGroup // per-tier inflight fill WaitGroups
	fillActive   []atomic.Int64   // observable fill/repair goroutines per tier

	// baseCtx parents every async fill/repair goroutine; baseCancel
	// (called by Close) cancels them all on shutdown. This is what
	// makes "no fill deadline" safe — a wedged origin no longer leaks
	// fill goroutines; they unblock when the cache is torn down.
	baseCtx    context.Context
	baseCancel context.CancelFunc
	// fillTimeout bounds async fills. 0 = no deadline (default): a fill
	// is bounded only by baseCtx (Close) — never an arbitrary number.
	fillTimeout time.Duration

	tierHits   []atomic.Uint64
	tierMisses []atomic.Uint64
	tierFills  []atomic.Uint64
	tierErrors []atomic.Uint64 // per-tier tryTier err-return count

	originHits   atomic.Uint64
	originMisses atomic.Uint64
	originErrors atomic.Uint64

	inflightMu sync.Mutex
	inflight   map[inflightKey]*inflightFetch
	leaders    sync.WaitGroup
}

type inflightKey struct {
	partition store.Partition
	key       store.ContentKey
}

// inflightFetch owns one Blob handle until the leader has finished filling
// and every waiter has stopped using the entry. Get waiters Clone the handle;
// Warm waiters never receive the payload.
type inflightFetch struct {
	key    inflightKey
	ctx    context.Context
	cancel context.CancelFunc

	dataReady chan struct{}
	fillReady chan struct{}

	found   bool
	blob    Blob
	dataErr error
	fillErr error

	waiters   int
	delivered bool
	done      bool
}

// TieredCounters is the snapshot returned by TieredCache.Counters().
// The server package composes these with per-tier type detail (rocks
// properties, EC peers, upstream endpoint) to produce cache.TieredStats.
type TieredCounters struct {
	TierHits          []uint64
	TierMisses        []uint64
	TierFills         []uint64
	TierFillsInflight []uint64
	TierErrors        []uint64
	OriginHits        uint64
	OriginMisses      uint64
	OriginErrors      uint64
}

// NewTieredCache builds a TieredCache from an origin and zero or more cache tiers.
// Tiers are searched in order; backfill flows upward (origin → ... → tier 0).
//
// Tiers that implement Repairable are auto-enabled with a runner that
// tracks repair goroutines in the tier's fillInflight WaitGroup, so
// graceful shutdown correctly accounts for in-flight repairs.
func NewTieredCache(origin Getter, tiers ...Tier) *TieredCache {
	baseCtx, baseCancel := context.WithCancel(context.Background())
	tc := &TieredCache{
		tiers:        tiers,
		origin:       origin,
		baseCtx:      baseCtx,
		baseCancel:   baseCancel,
		fillInflight: make([]sync.WaitGroup, len(tiers)),
		fillActive:   make([]atomic.Int64, len(tiers)),
		tierHits:     make([]atomic.Uint64, len(tiers)),
		tierMisses:   make([]atomic.Uint64, len(tiers)),
		tierFills:    make([]atomic.Uint64, len(tiers)),
		tierErrors:   make([]atomic.Uint64, len(tiers)),
		inflight:     make(map[inflightKey]*inflightFetch),
	}
	for i, t := range tiers {
		r, ok := unwrapTier(t).(Repairable)
		if !ok {
			continue
		}
		idx := i
		r.EnableRepair(func(fn func()) {
			tc.fillInflight[idx].Add(1)
			tc.fillActive[idx].Add(1)
			go func() {
				defer tc.fillInflight[idx].Done()
				defer tc.fillActive[idx].Add(-1)
				fn()
			}()
		})
	}
	return tc
}

// Counters returns a snapshot of per-tier + origin counters. Safe to
// call concurrently with Get/startFill.
func (tc *TieredCache) Counters() TieredCounters {
	c := TieredCounters{
		TierHits:          make([]uint64, len(tc.tiers)),
		TierMisses:        make([]uint64, len(tc.tiers)),
		TierFills:         make([]uint64, len(tc.tiers)),
		TierFillsInflight: make([]uint64, len(tc.tiers)),
		TierErrors:        make([]uint64, len(tc.tiers)),
		OriginHits:        tc.originHits.Load(),
		OriginMisses:      tc.originMisses.Load(),
		OriginErrors:      tc.originErrors.Load(),
	}
	for i := range tc.tiers {
		c.TierHits[i] = tc.tierHits[i].Load()
		c.TierMisses[i] = tc.tierMisses[i].Load()
		c.TierFills[i] = tc.tierFills[i].Load()
		c.TierFillsInflight[i] = uint64(tc.fillActive[i].Load())
		c.TierErrors[i] = tc.tierErrors[i].Load()
	}
	return c
}

// Get attempts to read by key, looking through cache tiers and then the origin.
// On hit, upper cache tiers are filled asynchronously. On terminal miss, FillMiss
// is invoked on every tier that supports it.
//
// Returned Blob ownership: the caller owns the handle and must Release
// it exactly once after use. Fill-aside cloning is handled internally
// by startFill, so the handle returned to the caller is distinct from
// any clones in flight inside fill goroutines.
func (tc *TieredCache) Get(ctx context.Context, p store.Partition, key store.ContentKey) (bool, Blob, error) {
	result, blob, source, err := tc.lookupReadyTiers(ctx, p, key, true)
	if err != nil {
		return false, nil, err
	}
	switch result {
	case CacheHit:
		for j := source - 1; j >= 0; j-- {
			tc.startFill(j, p, key, blob)
		}
		return true, blob, nil
	case CacheHitMiss:
		return false, nil, nil
	}

	// Only an all-tier miss enters the coalescer, keeping ready-tier hits on
	// the original allocation-free synchronous path.
	entry := tc.joinInflight(p, key)

	select {
	case <-entry.dataReady:
		tc.markDelivered(entry)
		defer tc.leaveInflight(entry, false)
		if entry.dataErr != nil {
			return false, nil, entry.dataErr
		}
		if !entry.found {
			return false, nil, nil
		}
		return true, entry.blob.Clone(), nil
	case <-ctx.Done():
		tc.leaveInflight(entry, true)
		return false, nil, ctx.Err()
	}
}

// Warm joins the same content-key lookup as Get, but waits until at least one
// upper tier has synchronously accepted the fetched bytes. It never returns a
// Blob handle to the caller.
func (tc *TieredCache) Warm(ctx context.Context, p store.Partition, key store.ContentKey) (CacheResult, error) {
	if len(tc.tiers) == 0 {
		return CacheMiss, ErrWarmUnsupported
	}
	result, blob, _, err := tc.lookupReadyTiers(ctx, p, key, true)
	if err != nil {
		return CacheMiss, err
	}
	if blob != nil {
		blob.Release()
	}
	if result == CacheHit {
		return CacheHit, nil // already ready in at least one cache tier
	}
	if result == CacheHitMiss {
		return CacheMiss, nil
	}

	entry := tc.joinInflight(p, key)

	select {
	case <-entry.fillReady:
		tc.markDelivered(entry)
		defer tc.leaveInflight(entry, false)
		if entry.dataErr != nil {
			return CacheMiss, entry.dataErr
		}
		if !entry.found {
			return CacheMiss, nil
		}
		if entry.fillErr != nil {
			return CacheMiss, entry.fillErr
		}
		return CacheHit, nil
	case <-ctx.Done():
		tc.leaveInflight(entry, true)
		return CacheMiss, ctx.Err()
	}
}

func (tc *TieredCache) joinInflight(p store.Partition, key store.ContentKey) *inflightFetch {
	k := inflightKey{partition: p, key: key}
	tc.inflightMu.Lock()
	if entry := tc.inflight[k]; entry != nil {
		entry.waiters++
		tc.inflightMu.Unlock()
		return entry
	}
	entryCtx, cancel := context.WithCancel(tc.baseCtx)
	entry := &inflightFetch{
		key:       k,
		ctx:       entryCtx,
		cancel:    cancel,
		dataReady: make(chan struct{}),
		fillReady: make(chan struct{}),
		waiters:   1,
	}
	tc.inflight[k] = entry
	tc.leaders.Add(1)
	tc.inflightMu.Unlock()

	go tc.runInflight(k, entry)
	return entry
}

func (tc *TieredCache) markDelivered(entry *inflightFetch) {
	tc.inflightMu.Lock()
	entry.delivered = true
	tc.inflightMu.Unlock()
}

func (tc *TieredCache) leaveInflight(entry *inflightFetch, canceled bool) {
	tc.inflightMu.Lock()
	entry.waiters--
	if canceled && entry.waiters == 0 && !entry.delivered && !entry.done {
		if tc.inflight[entry.key] == entry {
			delete(tc.inflight, entry.key)
		}
		entry.cancel()
	}
	if entry.done && entry.waiters == 0 && entry.blob != nil {
		entry.blob.Release()
		entry.blob = nil
	}
	tc.inflightMu.Unlock()
}

func (tc *TieredCache) finishInflight(k inflightKey, entry *inflightFetch) {
	tc.inflightMu.Lock()
	if tc.inflight[k] == entry {
		delete(tc.inflight, k)
	}
	entry.done = true
	entry.cancel()
	if entry.waiters == 0 && entry.blob != nil {
		entry.blob.Release()
		entry.blob = nil
	}
	tc.inflightMu.Unlock()
}

func (tc *TieredCache) runInflight(k inflightKey, entry *inflightFetch) {
	defer tc.leaders.Done()
	found, blob, source, err := tc.lookupOrigin(entry.ctx, k.partition, k.key)
	entry.found, entry.blob, entry.dataErr = found, blob, err
	close(entry.dataReady)

	if err == nil && found {
		entry.fillErr = tc.fillUpper(entry.ctx, k.partition, k.key, blob, source)
	}
	close(entry.fillReady)
	tc.finishInflight(k, entry)
}

// lookupReadyTiers never consults origin. A semaphore wait restarts from tier
// zero because another request may have filled an upper tier while we waited.
// count=false is used by the leader's stale-miss probe and deliberately leaves
// user-visible tier counters unchanged.
func (tc *TieredCache) lookupReadyTiers(ctx context.Context, p store.Partition, key store.ContentKey, count bool) (CacheResult, Blob, int, error) {
restart:
	for {
		for i, layer := range tc.tiers {
			result, blob, waited, err := tryTier(ctx, layer, p, key)
			if err != nil {
				if count {
					tc.tierErrors[i].Add(1)
				}
				return CacheMiss, nil, -1, err
			}
			if waited {
				continue restart
			}
			if count {
				switch result {
				case CacheHit, CacheHitMiss:
					tc.tierHits[i].Add(1)
				case CacheMiss:
					tc.tierMisses[i].Add(1)
				}
			}
			switch result {
			case CacheHit, CacheHitMiss:
				return result, blob, i, nil
			case CacheMiss:
				continue
			}
		}
		return CacheMiss, nil, len(tc.tiers), nil
	}
}

// lookupOrigin is run by exactly one singleflight leader. If acquiring the
// origin semaphore waited, it rechecks ready tiers before touching origin.
func (tc *TieredCache) lookupOrigin(ctx context.Context, p store.Partition, key store.ContentKey) (bool, Blob, int, error) {
	// Close the stale-miss window between a caller's counted tier scan and
	// joinInflight: an earlier leader may have completed fill and removed its
	// entry before this leader was installed.
	result, blob, source, err := tc.lookupReadyTiers(ctx, p, key, false)
	if err != nil {
		return false, nil, -1, err
	}
	if result == CacheHit {
		return true, blob, source, nil
	}
	if result == CacheHitMiss {
		return false, nil, source, nil
	}

	for {
		result, blob, waited, err := tryTier(ctx, tc.origin, p, key)
		if err != nil {
			tc.originErrors.Add(1)
			return false, nil, -1, err
		}
		if waited {
			result, blob, source, err := tc.lookupReadyTiers(ctx, p, key, false)
			if err != nil {
				return false, nil, -1, err
			}
			if result == CacheHit {
				return true, blob, source, nil
			}
			if result == CacheHitMiss {
				return false, nil, source, nil
			}
			continue
		}

		switch result {
		case CacheHit:
			tc.originHits.Add(1)
			return true, blob, len(tc.tiers), nil
		case CacheHitMiss:
			tc.originHits.Add(1)
			return false, nil, len(tc.tiers), nil
		case CacheMiss:
			tc.originMisses.Add(1)
			for j := len(tc.tiers) - 1; j >= 0; j-- {
				tc.startFillMiss(j, p, key)
			}
			return false, nil, len(tc.tiers), nil
		}
	}
}

// fillUpper preserves the existing fill-aside cascade, but performs it on the
// singleflight leader so Warm can observe when bytes are actually ready. Get
// waiters are already released through dataReady and never wait here.
func (tc *TieredCache) fillUpper(parent context.Context, p store.Partition, key store.ContentKey, blob Blob, source int) error {
	if source == 0 {
		return nil // already resident in the highest ready tier
	}
	var fillErr error
	// A hit in any cache tier is already warm even if promotion to a still
	// higher tier fails. An origin hit must successfully fill one cache tier.
	succeeded := source < len(tc.tiers)
	for j := source - 1; j >= 0; j-- {
		tc.fillActive[j].Add(1)
		tc.tierFills[j].Add(1)
		var ctx context.Context
		var cancel context.CancelFunc
		if tc.fillTimeout > 0 {
			ctx, cancel = context.WithTimeout(parent, tc.fillTimeout)
		} else {
			ctx, cancel = context.WithCancel(parent)
		}
		err := tc.tiers[j].Fill(ctx, p, key, blob.Bytes())
		cancel()
		tc.fillActive[j].Add(-1)
		if err == nil {
			succeeded = true
		} else {
			fillErr = errors.Join(fillErr, fmt.Errorf("tier %d: %w", j, err))
		}
	}
	if succeeded {
		return nil
	}
	if fillErr != nil {
		return fmt.Errorf("cache: warm fill failed: %w", fillErr)
	}
	return nil // source < len(tiers) would already have made succeeded true.
}

// tryTier acquires the optional sem, then calls Get.
// If acquire would block (waited=true), it releases and returns the restart signal
// without calling Get — the caller will restart from layer 0.
func tryTier(ctx context.Context, c Getter, p store.Partition, key store.ContentKey) (CacheResult, Blob, bool, error) {
	if sem, ok := c.(Semaphore); ok {
		waited, err := sem.Acquire(ctx)
		if err != nil {
			return CacheMiss, nil, false, err
		}
		if waited {
			sem.Release()
			return CacheMiss, nil, true, nil
		}
		defer sem.Release()
	}
	result, blob, err := c.Get(ctx, p, key)
	return result, blob, false, err
}

// startFill clones the blob (refcount++) and hands the clone to a
// fire-and-forget goroutine that writes the bytes into the named tier.
// The caller's original `blob` handle is unaffected — it continues to
// own its own reference and must still be Released independently (the
// wire server does so after writing the response frame). The clone is
// Released inside the goroutine after Fill returns, balancing the
// refcount.
//
// This clone pattern is the whole reason cache.Blob exists: without
// it, a pool-backed blob would be returned to its pool as soon as the
// "main" Release fired, and the fill-goroutine's view of the buffer
// would race with the next caller that pulled the same slice from the
// pool. Refcounting turns the race into safe cooperative ownership.
func (tc *TieredCache) startFill(tierIdx int, p store.Partition, key store.ContentKey, blob Blob) {
	cloned := blob.Clone()
	tc.fillInflight[tierIdx].Add(1)
	tc.fillActive[tierIdx].Add(1)
	tc.tierFills[tierIdx].Add(1)
	go func() {
		defer tc.fillInflight[tierIdx].Done()
		defer tc.fillActive[tierIdx].Add(-1)
		defer cloned.Release()
		ctx, cancel := tc.fillCtx()
		defer cancel()
		_ = tc.tiers[tierIdx].Fill(ctx, p, key, cloned.Bytes())
	}()
}

// startFillMiss invokes FillMiss on the tier if it implements the optional interface.
func (tc *TieredCache) startFillMiss(tierIdx int, p store.Partition, key store.ContentKey) {
	mf, ok := unwrapTier(tc.tiers[tierIdx]).(FillMiss)
	if !ok {
		return
	}
	tc.fillInflight[tierIdx].Add(1)
	tc.fillActive[tierIdx].Add(1)
	go func() {
		defer tc.fillInflight[tierIdx].Done()
		defer tc.fillActive[tierIdx].Add(-1)
		ctx, cancel := tc.fillCtx()
		defer cancel()
		_ = mf.FillMiss(ctx, p, key)
	}()
}

// ───────────────────────────── semaphore (internal) ─────────────────────────────

// semaphore is a context-aware concurrency gate (default Semaphore implementation).
type semaphore struct{ ch chan struct{} }

// newSemaphore returns a semaphore with capacity n. n<=0 returns nil (unlimited).
func newSemaphore(n int) *semaphore {
	if n <= 0 {
		return nil
	}
	return &semaphore{ch: make(chan struct{}, n)}
}

// Acquire returns waited=true if the call had to block on a full channel.
func (s *semaphore) Acquire(ctx context.Context) (waited bool, err error) {
	if s == nil {
		return false, nil
	}
	select {
	case s.ch <- struct{}{}:
		return false, nil
	default:
	}
	select {
	case s.ch <- struct{}{}:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// Release returns one slot.
func (s *semaphore) Release() {
	if s == nil {
		return
	}
	<-s.ch
}

// tierAdapter adds a lookup concurrency budget without hiding optional tier
// capabilities such as Repairable and FillMiss from TieredCache.
type tierAdapter struct {
	Tier
	*semaphore
}

// NewTierAdapter limits synchronous lookups against inner. Fills retain the
// concrete tier's own concurrency controls. A non-positive limit leaves the
// tier unchanged.
func NewTierAdapter(inner Tier, maxInflight int) Tier {
	if maxInflight <= 0 {
		return inner
	}
	return &tierAdapter{Tier: inner, semaphore: newSemaphore(maxInflight)}
}

func unwrapTier(t Tier) Tier {
	for {
		adapter, ok := t.(*tierAdapter)
		if !ok {
			return t
		}
		t = adapter.Tier
	}
}

// ───────────────────────────── origin adapter ─────────────────────────────

// originAdapter composes an inner Getter with a max-inflight semaphore,
// serving as the origin tier of a TieredCache. The inner Getter can be
// any read-capable cache component — rocks, a wire client, a NewStoreOrigin
// bridge over a store-ctl client, etc.
type originAdapter struct {
	inner Getter
	*semaphore
}

// NewOriginAdapter wraps inner with a max-inflight semaphore.
// maxInflight<=0 means unlimited.
func NewOriginAdapter(inner Getter, maxInflight int) Getter {
	return &originAdapter{
		inner:     inner,
		semaphore: newSemaphore(maxInflight),
	}
}

func (oa *originAdapter) Get(ctx context.Context, p store.Partition, key store.ContentKey) (CacheResult, Blob, error) {
	return oa.inner.Get(ctx, p, key)
}
