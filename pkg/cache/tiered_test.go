package cache

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/store"
)

type gatedOrigin struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (o *gatedOrigin) Get(ctx context.Context, _ store.Partition, _ store.ContentKey) (CacheResult, Blob, error) {
	o.calls.Add(1)
	select {
	case o.started <- struct{}{}:
	default:
	}
	select {
	case <-o.release:
		return CacheHit, NewMemBlob([]byte("ciphertext")), nil
	case <-ctx.Done():
		return CacheMiss, nil, ctx.Err()
	}
}

type gatedFillTier struct {
	started chan struct{}
	release chan struct{}
}

type staleMissTier struct {
	calls        atomic.Int32
	ready        atomic.Bool
	firstStarted chan struct{}
	releaseFirst chan struct{}
	filled       chan struct{}
}

func (t *staleMissTier) Get(context.Context, store.Partition, store.ContentKey) (CacheResult, Blob, error) {
	call := t.calls.Add(1)
	readyAtStart := t.ready.Load()
	if call == 1 {
		close(t.firstStarted)
		<-t.releaseFirst
	}
	if readyAtStart {
		return CacheHit, NewMemBlob([]byte("ciphertext")), nil
	}
	return CacheMiss, nil, nil
}

func (t *staleMissTier) Fill(context.Context, store.Partition, store.ContentKey, []byte) error {
	t.ready.Store(true)
	select {
	case <-t.filled:
	default:
		close(t.filled)
	}
	return nil
}

func (t *gatedFillTier) Get(context.Context, store.Partition, store.ContentKey) (CacheResult, Blob, error) {
	return CacheMiss, nil, nil
}

func (t *gatedFillTier) Fill(ctx context.Context, _ store.Partition, _ store.ContentKey, _ []byte) error {
	select {
	case t.started <- struct{}{}:
	default:
	}
	select {
	case <-t.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestTieredCacheGetAndWarmShareLookupButWaitForDifferentReadiness(t *testing.T) {
	origin := &gatedOrigin{started: make(chan struct{}, 1), release: make(chan struct{})}
	tier := &gatedFillTier{started: make(chan struct{}, 1), release: make(chan struct{})}
	tc := NewTieredCache(origin, tier)
	defer tc.Close()
	key := store.ContentKey{1}

	warmDone := make(chan error, 1)
	go func() {
		result, err := tc.Warm(context.Background(), store.PartitionChunk, key)
		if err == nil && result != CacheHit {
			err = errors.New("warm did not hit")
		}
		warmDone <- err
	}()
	select {
	case <-origin.started:
	case <-time.After(time.Second):
		t.Fatal("origin lookup did not start")
	}

	getDone := make(chan error, 1)
	go func() {
		found, blob, err := tc.Get(context.Background(), store.PartitionChunk, key)
		if err == nil && !found {
			err = errors.New("get did not hit")
		}
		if blob != nil {
			blob.Release()
		}
		getDone <- err
	}()
	close(origin.release)

	select {
	case err := <-getDone:
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Get waited for cache fill instead of dataReady")
	}
	select {
	case <-tier.started:
	case <-time.After(time.Second):
		t.Fatal("upper tier fill did not start")
	}
	select {
	case err := <-warmDone:
		t.Fatalf("Warm returned before fillReady: %v", err)
	default:
	}
	close(tier.release)
	select {
	case err := <-warmDone:
		if err != nil {
			t.Fatalf("Warm: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Warm did not return after fill")
	}
	if got := origin.calls.Load(); got != 1 {
		t.Fatalf("origin calls=%d, want one shared lookup", got)
	}
}

func TestTieredCacheCanceledSoleWaiterCancelsLeader(t *testing.T) {
	origin := &gatedOrigin{started: make(chan struct{}, 1), release: make(chan struct{})}
	tc := NewTieredCache(origin, newMockTier())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := tc.Get(ctx, store.PartitionChunk, store.ContentKey{2})
		done <- err
	}()
	<-origin.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Get error=%v, want context canceled", err)
	}
	tc.Close()
}

func TestTieredCacheLeaderRechecksTiersAfterStaleMiss(t *testing.T) {
	origin := &gatedOrigin{started: make(chan struct{}, 2), release: make(chan struct{})}
	tier := &staleMissTier{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
		filled:       make(chan struct{}),
	}
	tc := NewTieredCache(origin, tier)
	defer tc.Close()
	key := store.ContentKey{3}

	// B captures a tier miss, then pauses before it can join singleflight.
	bDone := make(chan error, 1)
	go func() {
		found, blob, err := tc.Get(context.Background(), store.PartitionChunk, key)
		if err == nil && !found {
			err = errors.New("B did not hit")
		}
		if blob != nil {
			blob.Release()
		}
		bDone <- err
	}()
	<-tier.firstStarted

	// A proceeds through origin and fills the tier while B still holds its
	// stale miss result.
	aDone := make(chan error, 1)
	go func() {
		found, blob, err := tc.Get(context.Background(), store.PartitionChunk, key)
		if err == nil && !found {
			err = errors.New("A did not hit")
		}
		if blob != nil {
			blob.Release()
		}
		aDone <- err
	}()
	<-origin.started
	close(origin.release)
	if err := <-aDone; err != nil {
		t.Fatalf("A Get: %v", err)
	}
	select {
	case <-tier.filled:
	case <-time.After(time.Second):
		t.Fatal("A did not fill ready tier")
	}
	deadline := time.Now().Add(time.Second)
	for {
		tc.inflightMu.Lock()
		remaining := len(tc.inflight)
		tc.inflightMu.Unlock()
		if remaining == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("A singleflight entry was not retired")
		}
		time.Sleep(time.Millisecond)
	}

	close(tier.releaseFirst)
	if err := <-bDone; err != nil {
		t.Fatalf("B Get: %v", err)
	}
	if got := origin.calls.Load(); got != 1 {
		t.Fatalf("origin calls=%d, want 1 after stale-miss recheck", got)
	}
	// B's uncounted leader probe must not add another hit/miss sample.
	c := tc.Counters()
	if got := c.TierMisses[0] + c.TierHits[0]; got != 2 {
		t.Fatalf("counted tier probes=%d, want exactly A+B initial scans", got)
	}
}

// countingBlob is a cache.Blob mock that tracks Clone and Release
// calls through a shared refcount, so tests can assert that fill-
// aside handles the lifecycle correctly.
type countingBlob struct {
	data    []byte
	state   *blobState
	handles *atomic.Int32
}

type blobState struct {
	clones   atomic.Int32
	releases atomic.Int32
}

func newCountingBlob(data []byte) *countingBlob {
	b := &countingBlob{
		data:    data,
		state:   &blobState{},
		handles: new(atomic.Int32),
	}
	b.handles.Store(1)
	return b
}

func (b *countingBlob) Bytes() []byte { return b.data }

func (b *countingBlob) Clone() Blob {
	b.state.clones.Add(1)
	b.handles.Add(1)
	return &countingBlob{data: b.data, state: b.state, handles: b.handles}
}

func (b *countingBlob) Release() {
	b.state.releases.Add(1)
	b.handles.Add(-1)
}

// mockTier is a Tier that never hits on Get and records the
// bytes it was asked to Fill. It also serialises Fill behind a
// channel so the test can observe the moment the goroutine sees the
// data.
type mockTier struct {
	fillCalled atomic.Int32
	fillBytes  chan []byte
}

type cancelFillTier struct {
	started  chan struct{}
	finished chan struct{}
}

func (t *cancelFillTier) Get(context.Context, store.Partition, store.ContentKey) (CacheResult, Blob, error) {
	return CacheMiss, nil, nil
}

func (t *cancelFillTier) Fill(ctx context.Context, _ store.Partition, _ store.ContentKey, _ []byte) error {
	close(t.started)
	<-ctx.Done()
	close(t.finished)
	return ctx.Err()
}

type optionalTier struct {
	*mockTier
	repairEnabled atomic.Bool
	missFills     atomic.Int32
}

func (t *optionalTier) EnableRepair(runner func(fn func())) {
	t.repairEnabled.Store(runner != nil)
}

func (t *optionalTier) FillMiss(ctx context.Context, p store.Partition, key store.ContentKey) error {
	t.missFills.Add(1)
	return nil
}

func newMockTier() *mockTier {
	return &mockTier{fillBytes: make(chan []byte, 4)}
}

func (m *mockTier) Get(ctx context.Context, p store.Partition, key store.ContentKey) (CacheResult, Blob, error) {
	return CacheMiss, nil, nil
}

func (m *mockTier) Fill(ctx context.Context, p store.Partition, key store.ContentKey, data []byte) error {
	m.fillCalled.Add(1)
	// Copy data — we're validating startFill clone semantics, not
	// Fill's own contract (which explicitly allows data to go away
	// after Fill returns).
	m.fillBytes <- append([]byte(nil), data...)
	return nil
}

func TestTierAdapterLimitsLookupsAndPreservesOptionalCapabilities(t *testing.T) {
	inner := &optionalTier{mockTier: newMockTier()}
	limited := NewTierAdapter(inner, 1)
	sem, ok := limited.(Semaphore)
	if !ok {
		t.Fatalf("limited tier type %T does not implement Semaphore", limited)
	}
	if waited, err := sem.Acquire(context.Background()); err != nil || waited {
		t.Fatalf("first acquire waited=%v err=%v", waited, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sem.Acquire(canceled); err == nil {
		t.Fatal("second acquire should block until its context is canceled")
	}
	sem.Release()

	tc := NewTieredCache(missGetter{}, limited)
	if !inner.repairEnabled.Load() {
		t.Fatal("tier adapter hid Repairable from TieredCache")
	}
	tc.startFillMiss(0, store.PartitionChunk, store.ContentKey{1})
	tc.Close()
	if got := inner.missFills.Load(); got != 1 {
		t.Fatalf("FillMiss calls=%d, want 1", got)
	}
	if got := NewTierAdapter(inner, 0); got != inner {
		t.Fatalf("unlimited tier was wrapped: %T", got)
	}
}

// TestTieredCache_StartFillClonesBlob asserts the contract baked into
// startFill:
//
//   - Every call to startFill produces exactly one Clone of the
//     supplied blob (for the fill goroutine's own handle) and exactly
//     one Release (after Fill returns).
//   - The caller's original handle is neither cloned nor released by
//     startFill itself.
//   - The bytes received by Fill match the blob's contents verbatim.
//
// This is the load-bearing invariant of the refcounted Blob design —
// without Clone-before-goroutine, a pool-backed blob could be returned
// to its pool while the fill goroutine still references it.
func TestTieredCache_StartFillClonesBlob(t *testing.T) {
	tier := newMockTier()
	tc := NewTieredCache(NewOriginAdapter(nil, 0), tier)

	blob := newCountingBlob([]byte("payload"))
	key := store.ContentKey{1, 2, 3}

	tc.startFill(0, store.PartitionChunk, key, blob)

	// Wait for the goroutine to do its work.
	select {
	case got := <-tier.fillBytes:
		if string(got) != "payload" {
			t.Errorf("Fill received wrong data: got %q, want %q", got, "payload")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Fill was not called within 2s")
	}

	// Close joins the fill goroutine and ensures Release has happened.
	tc.Close()

	// Validate the clone accounting. The original blob still has
	// refcount = 1 (caller owns it); the fill-goroutine's clone has
	// been Released.
	if got := blob.state.clones.Load(); got != 1 {
		t.Errorf("Clone count: got %d, want 1", got)
	}
	if got := blob.state.releases.Load(); got != 1 {
		t.Errorf("Release count (clone side): got %d, want 1", got)
	}
	if got := blob.handles.Load(); got != 1 {
		t.Errorf("live handle count: got %d, want 1 (the original)", got)
	}
	if got := tier.fillCalled.Load(); got != 1 {
		t.Errorf("Fill call count: got %d, want 1", got)
	}

	// The caller's original blob is still usable.
	if string(blob.Bytes()) != "payload" {
		t.Errorf("original blob corrupted after startFill")
	}
	blob.Release()
}

func TestTieredCacheCloseCancelsAndDrainsFill(t *testing.T) {
	tier := &cancelFillTier{started: make(chan struct{}), finished: make(chan struct{})}
	tc := NewTieredCache(missGetter{}, tier)
	blob := newCountingBlob([]byte("payload"))
	tc.startFill(0, store.PartitionChunk, store.ContentKey{1}, blob)

	select {
	case <-tier.started:
	case <-time.After(time.Second):
		t.Fatal("fill did not start")
	}
	if got := tc.Counters().TierFillsInflight[0]; got != 1 {
		t.Fatalf("fills in flight=%d, want 1", got)
	}
	tc.Close()
	select {
	case <-tier.finished:
	default:
		t.Fatal("Close returned before the canceled fill exited")
	}
	if got := blob.handles.Load(); got != 1 {
		t.Fatalf("live handle count after Close=%d, want caller's original handle", got)
	}
	if got := tc.Counters().TierFillsInflight[0]; got != 0 {
		t.Fatalf("fills in flight after Close=%d, want 0", got)
	}
	blob.Release()

	// Cancellation and waiting are both safe to repeat after the drain.
	tc.Close()
}

// hitTier is a Tier that always reports CacheHit and returns a fresh
// blob; used to walk TieredCache counters through the cascade.
type hitTier struct{ data []byte }

func (h *hitTier) Get(ctx context.Context, p store.Partition, key store.ContentKey) (CacheResult, Blob, error) {
	return CacheHit, NewMemBlob(h.data), nil
}
func (h *hitTier) Fill(ctx context.Context, p store.Partition, key store.ContentKey, data []byte) error {
	return nil
}

// missGetter is a trivial Getter that always misses (for the origin
// slot) so the cascade runs through without a hit.
type missGetter struct{}

func (missGetter) Get(ctx context.Context, p store.Partition, key store.ContentKey) (CacheResult, Blob, error) {
	return CacheMiss, nil, nil
}

// TestTieredCache_CountersCascade exercises the Get cascade with a
// miss-then-hit pattern and verifies the parallel hit/miss/fill
// counters track the layers correctly. It also verifies the origin
// counters bump when the cascade falls through to origin.
func TestTieredCache_CountersCascade(t *testing.T) {
	miss := newMockTier() // tier 0 — miss
	hit := &hitTier{data: []byte("payload")}

	tc := NewTieredCache(missGetter{}, miss, hit)
	key := store.ContentKey{1, 2, 3}

	found, blob, err := tc.Get(context.Background(), store.PartitionChunk, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatal("expected hit (tier 1)")
	}
	blob.Release()

	// Stop accepting work and drain the backfill before reading counters.
	tc.Close()

	c := tc.Counters()
	if c.TierHits[0] != 0 || c.TierMisses[0] != 1 {
		t.Errorf("tier 0: hits=%d misses=%d (want 0/1)", c.TierHits[0], c.TierMisses[0])
	}
	if c.TierHits[1] != 1 || c.TierMisses[1] != 0 {
		t.Errorf("tier 1: hits=%d misses=%d (want 1/0)", c.TierHits[1], c.TierMisses[1])
	}
	// tier 0 is upstream of the hit; one backfill attempt flows up.
	if c.TierFills[0] != 1 {
		t.Errorf("tier 0 fills=%d (want 1 — backfilled from hit)", c.TierFills[0])
	}
	if c.TierFills[1] != 0 {
		t.Errorf("tier 1 fills=%d (want 0 — it was the hit)", c.TierFills[1])
	}
	if c.OriginHits != 0 || c.OriginMisses != 0 {
		t.Errorf("origin: hits=%d misses=%d (want both 0 — hit at tier 1)", c.OriginHits, c.OriginMisses)
	}

	// Now drive a full miss so origin gets bumped.
	tc2 := NewTieredCache(missGetter{}, newMockTier())
	_, _, _ = tc2.Get(context.Background(), store.PartitionChunk, key)
	c2 := tc2.Counters()
	if c2.TierMisses[0] != 1 {
		t.Errorf("tier 0 missed: got %d", c2.TierMisses[0])
	}
	if c2.OriginMisses != 1 {
		t.Errorf("origin missed: got %d", c2.OriginMisses)
	}
}
