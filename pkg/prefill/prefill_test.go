package prefill

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/profile"
	"github.com/kuasar-sandbox/sandboxer/pkg/uffd"
)

type countingSource struct{ reads atomic.Uint64 }

func (s *countingSource) ReadAt(buf []byte, _ uint64) (int, bool, error) {
	s.reads.Add(1)
	for i := range buf {
		buf[i] = 1
	}
	return len(buf), false, nil
}

func (s *countingSource) CloneWithContext(context.Context) uffd.SnapshotReader { return s }

type blockingSource struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingSource) ReadAt(buf []byte, _ uint64) (int, bool, error) {
	s.once.Do(func() { close(s.started) })
	// Deliberately ignore cancellation to model a transport that is slow to
	// abort. Its cleanup must not remain on the vm.resume critical path.
	<-s.release
	return len(buf), false, nil
}

func (s *blockingSource) CloneWithContext(context.Context) uffd.SnapshotReader { return s }

func TestSelectRangesHonorsOrderAndBudget(t *testing.T) {
	in := []profile.Range{{Offset: 0x3000, Length: 0x1000, Order: 3}, {Offset: 0x1000, Length: 0x2000, Order: 1}, {Offset: 0, Length: 0x1000, Order: 0}}
	got, bytes, exhausted := selectRanges(in, 0x3000)
	if !exhausted || bytes != 0x3000 || len(got) != 2 || got[0].Order != 0 || got[1].Order != 1 {
		t.Fatalf("got=%+v bytes=%x exhausted=%v", got, bytes, exhausted)
	}
}

func TestSelectRangesKeepsOversizedHotPrefix(t *testing.T) {
	got, bytes, exhausted := selectRanges([]profile.Range{{Offset: 0, Length: 4 * profile.PageSize, Order: 0}}, 2*profile.PageSize)
	if !exhausted || bytes != 2*profile.PageSize || len(got) != 1 || got[0].Length != 2*profile.PageSize {
		t.Fatalf("got=%+v bytes=%d exhausted=%v", got, bytes, exhausted)
	}
}

func TestPrepareFetchesBeforeInjectAndRespectsBuffer(t *testing.T) {
	source := &countingSource{}
	prof := &profile.MemoryProfile{Version: profile.CurrentVersion, PageSize: profile.PageSize, Targets: []profile.Target{{
		Kind: "memory", Size: 4 * uffd.MaxBatchBytes,
		Ranges: []profile.Range{{Offset: 0, Length: 4 * uffd.MaxBatchBytes, Order: 0}},
	}}}
	plan := Prepare(context.Background(), source, prof, PrefillOptions{
		MaxBytes:    4 * uffd.MaxBatchBytes,
		BufferBytes: 2 * uffd.MaxBatchBytes,
		Workers:     8,
	})
	if plan == nil {
		t.Fatal("Prepare returned nil")
	}

	deadline := time.Now().Add(time.Second)
	for source.reads.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := source.reads.Load(); got != 2 {
		t.Fatalf("early reads=%d want=2", got)
	}
	// Without Inject releasing slots, later jobs must not allocate/fetch.
	time.Sleep(20 * time.Millisecond)
	if got := source.reads.Load(); got != 2 {
		t.Fatalf("buffer bound allowed %d reads, want 2", got)
	}
	plan.Close()
	if got := len(plan.results); got != 0 {
		t.Fatalf("Close retained %d buffered results", got)
	}
}

func TestInjectCancellationDoesNotJoinBlockedFetch(t *testing.T) {
	source := &blockingSource{started: make(chan struct{}), release: make(chan struct{})}
	prof := &profile.MemoryProfile{Version: profile.CurrentVersion, PageSize: profile.PageSize, Targets: []profile.Target{{
		Kind: "memory", Size: uffd.MaxBatchBytes,
		Ranges: []profile.Range{{Offset: 0, Length: uffd.MaxBatchBytes, Order: 0}},
	}}}
	plan := Prepare(context.Background(), source, prof, PrefillOptions{
		MaxBytes:    uffd.MaxBatchBytes,
		BufferBytes: uffd.MaxBatchBytes,
		Workers:     1,
	})
	if plan == nil {
		t.Fatal("Prepare returned nil")
	}

	select {
	case <-source.started:
	case <-time.After(time.Second):
		t.Fatal("fetch did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan PrefillStats, 1)
	go func() {
		returned <- plan.Inject(ctx, &uffd.Handler{})
	}()
	cancel()
	select {
	case stats := <-returned:
		if stats.BytesRequested != uffd.MaxBatchBytes {
			t.Fatalf("BytesRequested=%d, want %d", stats.BytesRequested, uffd.MaxBatchBytes)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Inject waited for blocked fetch worker after cancellation")
	}
	select {
	case <-plan.done:
		t.Fatal("blocked fetch unexpectedly finished; test did not exercise asynchronous cleanup")
	default:
	}

	close(source.release)
	plan.Close()
}
