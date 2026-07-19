package restore

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
)

type fakeWarmStream struct {
	refs []fetch.WarmChunkRef

	mu        sync.Mutex
	active    int
	maxActive int
	started   int
	release   <-chan struct{}
}

func (s *fakeWarmStream) Size() uint64 { return 1 << 20 }
func (s *fakeWarmStream) Close() error { return nil }
func (s *fakeWarmStream) RunAt(offset, limit uint64) (sparse.RunKind, uint64, error) {
	if offset >= s.Size() {
		return sparse.Hole, offset, io.EOF
	}
	return sparse.Data, min(offset+limit, s.Size()), nil
}
func (s *fakeWarmStream) ReadAt(context.Context, []byte, uint64) (int, error) { return 0, io.EOF }
func (s *fakeWarmStream) WalkDataChunks(start, end uint64, fn func(fetch.WarmChunkRef) error) error {
	for _, ref := range s.refs {
		if ref.End > start && ref.Offset < end {
			if err := fn(ref); err != nil {
				return err
			}
		}
	}
	return nil
}
func (s *fakeWarmStream) DataChunkAt(offset uint64) (fetch.WarmChunkRef, bool) {
	for _, ref := range s.refs {
		if offset >= ref.Offset && offset < ref.End {
			return ref, true
		}
	}
	return fetch.WarmChunkRef{}, false
}
func (s *fakeWarmStream) WarmSupported() bool { return true }
func (s *fakeWarmStream) WarmChunk(ctx context.Context, _ fetch.WarmChunkRef) error {
	s.mu.Lock()
	s.started++
	s.active++
	if s.active > s.maxActive {
		s.maxActive = s.active
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active--
		s.mu.Unlock()
	}()
	if s.release == nil {
		return nil
	}
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func testKey(v byte) store.ContentKey {
	var key store.ContentKey
	key[0] = v
	return key
}

func TestS2WarmerDedupesAndBoundsConcurrency(t *testing.T) {
	release := make(chan struct{})
	s := &fakeWarmStream{
		refs: []fetch.WarmChunkRef{
			{Key: testKey(1), Offset: 0, End: 4096, CiphertextBytes: 4097},
			{Key: testKey(1), Offset: 4096, End: 8192, CiphertextBytes: 4097}, // duplicate content
			{Key: testKey(2), Offset: 8192, End: 12288, CiphertextBytes: 4097},
			{Key: testKey(3), Offset: 12288, End: 16384, CiphertextBytes: 4097},
		},
		release: release,
	}
	w := startS2Warmer(context.Background(), s, 0, 16384, s2WarmConfig{Workers: 2, Queue: 4}, nil)

	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		started := s.started
		maxActive := s.maxActive
		s.mu.Unlock()
		if started >= 2 {
			if maxActive != 2 {
				t.Fatalf("max active = %d, want 2", maxActive)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for workers; started=%d", started)
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	deadline = time.Now().Add(time.Second)
	for w.Snapshot().P1Completed != 3 {
		if time.Now().After(deadline) {
			snap := w.Snapshot()
			t.Fatalf("timed out waiting for deduped jobs; started/completed=%d/%d", snap.P1Started, snap.P1Completed)
		}
		time.Sleep(time.Millisecond)
	}
	w.Close()

	snap := w.Snapshot()
	if snap.P1Started != 3 || snap.P1Completed != 3 {
		t.Fatalf("started/completed = %d/%d, want 3/3", snap.P1Started, snap.P1Completed)
	}
	if snap.P1InflightMax != 2 {
		t.Fatalf("inflight max = %d, want 2", snap.P1InflightMax)
	}
	if snap.P1ReadyCiphertextBytes != 3*4097 {
		t.Fatalf("ready ciphertext bytes = %d, want %d", snap.P1ReadyCiphertextBytes, 3*4097)
	}
}

func TestS2WarmerTracksSameKeyBothArrivalOrders(t *testing.T) {
	ref := fetch.WarmChunkRef{Key: testKey(7), Offset: 0, End: 4096}
	w := newTestWarmer(&fakeWarmStream{refs: []fetch.WarmChunkRef{ref}})

	// P0 arrives first; P1 later marks the already-active demand token.
	tok1 := w.BeginDemand(0, time.Millisecond).(*demandToken)
	w.markP1Start(ref.Key)
	w.markP1Stop(ref.Key)
	w.EndDemand(tok1, 2*time.Millisecond, 5*time.Millisecond, 4096, nil)

	// P1 arrives first; BeginDemand observes pendingP1 immediately.
	w.markP1Start(ref.Key)
	tok2 := w.BeginDemand(0, time.Millisecond).(*demandToken)
	w.markP1Stop(ref.Key)
	w.EndDemand(tok2, time.Millisecond, 3*time.Millisecond, 4096, nil)

	snap := w.Snapshot()
	if snap.P0DataFaults != 2 || snap.P0ConflictedFaults != 2 || snap.P0SameKeyConflicted != 2 {
		t.Fatalf("P0 total/conflicted/same = %d/%d/%d, want 2/2/2",
			snap.P0DataFaults, snap.P0ConflictedFaults, snap.P0SameKeyConflicted)
	}
	if snap.P0ServiceSameKeyP1.Count != 2 {
		t.Fatalf("same-key histogram count = %d, want 2", snap.P0ServiceSameKeyP1.Count)
	}
}

func TestS2WarmerOverlapUsesP1IntervalUnion(t *testing.T) {
	// First prove the union math exactly with overlapping synthetic intervals.
	var clock activityClock
	t0 := time.Unix(0, 0)
	clock.start(t0.Add(time.Millisecond))
	clock.start(t0.Add(2 * time.Millisecond))
	clock.stop(t0.Add(4 * time.Millisecond))
	clock.stop(t0.Add(6 * time.Millisecond))
	if got, want := clock.totalAt(t0.Add(10*time.Millisecond)), 5*time.Millisecond; got != want {
		t.Fatalf("activity union = %v, want %v", got, want)
	}

	// Then verify the observer classifies a demand that overlaps two P1s once.
	w := newTestWarmer(&fakeWarmStream{})
	token := w.BeginDemand(100, 0).(*demandToken) // no self key; still tracks any-P1 contention
	w.markP1Start(testKey(1))
	w.markP1Start(testKey(2))
	time.Sleep(time.Millisecond)
	w.markP1Stop(testKey(1))
	w.markP1Stop(testKey(2))
	w.EndDemand(token, time.Millisecond, 10*time.Millisecond, 4096, nil)

	snap := w.Snapshot()
	if snap.P0P1OverlapNs == 0 || snap.P0P1OverlapNs > snap.P0ServiceTotalNs {
		t.Fatalf("overlap/service = %d/%d, want 0 < overlap <= service", snap.P0P1OverlapNs, snap.P0ServiceTotalNs)
	}
	if snap.P0ServiceWithP1.Count != 1 || snap.P0ServiceNoP1.Count != 0 {
		t.Fatalf("with/no P1 counts = %d/%d, want 1/0", snap.P0ServiceWithP1.Count, snap.P0ServiceNoP1.Count)
	}
}

func TestS2WarmerZeroReadRetiresDemandWithoutRecording(t *testing.T) {
	ref := fetch.WarmChunkRef{Key: testKey(9), Offset: 0, End: 4096}
	w := newTestWarmer(&fakeWarmStream{refs: []fetch.WarmChunkRef{ref}})
	token := w.BeginDemand(0, 0).(*demandToken)
	w.EndDemand(token, time.Microsecond, time.Microsecond, 0, nil)

	w.conflictMu.Lock()
	active := len(w.activeP0)
	w.conflictMu.Unlock()
	if active != 0 || w.Snapshot().P0DataFaults != 0 {
		t.Fatalf("zero read left active=%d data_faults=%d", active, w.Snapshot().P0DataFaults)
	}
}

func TestS2WarmerRecordsZeroByteFetchError(t *testing.T) {
	ref := fetch.WarmChunkRef{Key: testKey(10), Offset: 0, End: 4096}
	w := newTestWarmer(&fakeWarmStream{refs: []fetch.WarmChunkRef{ref}})
	token := w.BeginDemand(0, 0)
	w.markP1Start(ref.Key)
	w.markP1Stop(ref.Key)
	w.EndDemand(token, time.Millisecond, 2*time.Millisecond, 0, errors.New("cache unavailable"))

	s := w.Snapshot()
	if s.P0DataFaults != 1 || s.P0ConflictedFaults != 1 || s.P0SameKeyConflicted != 1 {
		t.Fatalf("errored P0 total/conflicted/same=%d/%d/%d, want 1/1/1",
			s.P0DataFaults, s.P0ConflictedFaults, s.P0SameKeyConflicted)
	}
	if s.P0ServiceSameKeyP1.Count != 1 {
		t.Fatalf("errored P0 missing from same-key service histogram")
	}
}

func TestS2WarmerLatencyClassesAreMutuallyExclusive(t *testing.T) {
	ref := fetch.WarmChunkRef{Key: testKey(4), Offset: 0, End: 4096}
	w := newTestWarmer(&fakeWarmStream{refs: []fetch.WarmChunkRef{ref}})

	noP1 := w.BeginDemand(0, 0).(*demandToken)
	w.EndDemand(noP1, time.Millisecond, 2*time.Millisecond, 4096, nil)

	anyP1 := w.BeginDemand(8192, 0).(*demandToken)
	w.markP1Start(testKey(8))
	w.markP1Stop(testKey(8))
	w.EndDemand(anyP1, time.Millisecond, 2*time.Millisecond, 4096, nil)

	sameP1 := w.BeginDemand(0, 0).(*demandToken)
	w.markP1Start(ref.Key)
	w.markP1Stop(ref.Key)
	w.EndDemand(sameP1, time.Millisecond, 2*time.Millisecond, 4096, nil)

	s := w.Snapshot()
	if got := s.P0ServiceNoP1.Count + s.P0ServiceWithP1.Count + s.P0ServiceSameKeyP1.Count; got != s.P0DataFaults {
		t.Fatalf("service class count = %d, data faults = %d", got, s.P0DataFaults)
	}
	if s.P0ServiceNoP1.Count != 1 || s.P0ServiceWithP1.Count != 1 || s.P0ServiceSameKeyP1.Count != 1 {
		t.Fatalf("class counts no/any/same = %d/%d/%d, want 1/1/1",
			s.P0ServiceNoP1.Count, s.P0ServiceWithP1.Count, s.P0ServiceSameKeyP1.Count)
	}
}

func TestS2WarmerClockLinearizationNeverUnderflows(t *testing.T) {
	w := newTestWarmer(&fakeWarmStream{})
	for i := 0; i < 200; i++ {
		token := w.BeginDemand(8192, 0)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			w.markP1Start(testKey(3))
			w.markP1Stop(testKey(3))
		}()
		go func() {
			defer wg.Done()
			w.EndDemand(token, time.Microsecond, time.Microsecond, 4096, nil)
		}()
		wg.Wait()
	}
	s := w.Snapshot()
	if s.P0P1OverlapNs > s.P0ServiceTotalNs {
		t.Fatalf("overlap underflow/overflow: overlap=%d service=%d", s.P0P1OverlapNs, s.P0ServiceTotalNs)
	}
}

func TestS2WarmerSnapshotConcurrentWithWorkerAndCloseCleansState(t *testing.T) {
	release := make(chan struct{})
	ref := fetch.WarmChunkRef{Key: testKey(6), Offset: 0, End: 4096}
	s := &fakeWarmStream{refs: []fetch.WarmChunkRef{ref}, release: release}
	w := startS2Warmer(context.Background(), s, 0, 4096, s2WarmConfig{Workers: 1, Queue: 2}, nil)

	deadline := time.Now().Add(time.Second)
	for w.Snapshot().P1Started == 0 {
		if time.Now().After(deadline) {
			t.Fatal("P1 did not start")
		}
		time.Sleep(time.Millisecond)
	}

	var snapshots sync.WaitGroup
	snapshots.Add(1)
	go func() {
		defer snapshots.Done()
		for i := 0; i < 1000; i++ {
			_ = w.Snapshot()
		}
	}()
	close(release)
	w.Close()
	snapshots.Wait()

	w.conflictMu.Lock()
	defer w.conflictMu.Unlock()
	if w.p1Clock.active != 0 || len(w.pendingP1) != 0 || len(w.activeP0) != 0 {
		t.Fatalf("residual state active=%d pending=%d p0=%d",
			w.p1Clock.active, len(w.pendingP1), len(w.activeP0))
	}
}

func newTestWarmer(stream fetch.WarmableChunkStream) *s2Warmer {
	ctx, cancel := context.WithCancel(context.Background())
	return &s2Warmer{
		ctx:       ctx,
		cancel:    cancel,
		stream:    stream,
		workers:   1,
		pendingP1: make(map[store.ContentKey]int),
		activeP0:  make(map[store.ContentKey]map[*demandToken]struct{}),
	}
}
