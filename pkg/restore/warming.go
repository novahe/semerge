package restore

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandbox"
	"github.com/kuasar-sandbox/sandboxer/pkg/uffd"
)

type s2WarmConfig struct {
	Workers int
	Queue   int
}

// activityClock accumulates the union of intervals in which at least one P1
// request is active. Its methods are called while s2Warmer.conflictMu is held,
// which makes P1 activity and same-key bookkeeping one atomic observation.
type activityClock struct {
	active      int
	activeSince time.Time
	accumulated time.Duration
	maxActive   int
}

func (c *activityClock) start(now time.Time) {
	if c.active == 0 {
		c.activeSince = now
	}
	c.active++
	if c.active > c.maxActive {
		c.maxActive = c.active
	}
}

func (c *activityClock) stop(now time.Time) {
	if c.active <= 0 {
		return
	}
	c.active--
	if c.active == 0 {
		c.accumulated += now.Sub(c.activeSince)
	}
}

func (c *activityClock) totalAt(now time.Time) time.Duration {
	total := c.accumulated
	if c.active > 0 {
		total += now.Sub(c.activeSince)
	}
	return total
}

type demandToken struct {
	queueDelay          time.Duration
	p1ActiveTimeAtStart time.Duration
	key                 store.ContentKey
	hasKey              bool
	sameKey             bool // protected by conflictMu
}

type warmingCounters struct {
	p1Started              atomic.Uint64
	p1Completed            atomic.Uint64
	p1Errors               atomic.Uint64
	p1ReadyBeforeResume    atomic.Uint64
	p1ReadyCiphertextBytes atomic.Uint64
	p1WarmLatency          uffd.LatencyHistogram

	p0DataFaults        atomic.Uint64
	p0Conflicted        atomic.Uint64
	p0SameKeyConflicted atomic.Uint64
	p0P1OverlapNs       atomic.Uint64
	p0ServiceTotalNs    atomic.Uint64
	p0QueueNoP1         uffd.LatencyHistogram
	p0QueueWithP1       uffd.LatencyHistogram
	p0FetchNoP1         uffd.LatencyHistogram
	p0FetchWithP1       uffd.LatencyHistogram
	p0FetchSameKeyP1    uffd.LatencyHistogram
	p0ServiceNoP1       uffd.LatencyHistogram
	p0ServiceWithP1     uffd.LatencyHistogram
	p0ServiceSameKeyP1  uffd.LatencyHistogram
}

type s2Warmer struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	stream  fetch.WarmableChunkStream
	workers int
	jobs    chan fetch.WarmChunkRef
	logf    func(string, ...any)

	resumeUnixNs atomic.Int64
	stats        warmingCounters

	conflictMu sync.Mutex
	p1Clock    activityClock
	pendingP1  map[store.ContentKey]int
	activeP0   map[store.ContentKey]map[*demandToken]struct{}
}

func startS2Warmer(parent context.Context, stream fetch.WarmableChunkStream, start, end uint64, cfg s2WarmConfig, logf func(string, ...any)) *s2Warmer {
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.Queue <= 0 {
		cfg.Queue = 2 * cfg.Workers
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ctx, cancel := context.WithCancel(parent)
	w := &s2Warmer{
		ctx:       ctx,
		cancel:    cancel,
		stream:    stream,
		workers:   cfg.Workers,
		jobs:      make(chan fetch.WarmChunkRef, cfg.Queue),
		logf:      logf,
		pendingP1: make(map[store.ContentKey]int),
		activeP0:  make(map[store.ContentKey]map[*demandToken]struct{}),
	}

	w.wg.Add(1)
	go w.produce(start, end)
	for i := 0; i < cfg.Workers; i++ {
		w.wg.Add(1)
		go w.worker()
	}
	return w
}

func (w *s2Warmer) produce(start, end uint64) {
	defer w.wg.Done()
	defer close(w.jobs)
	seen := make(map[store.ContentKey]struct{})
	err := w.stream.WalkDataChunks(start, end, func(ref fetch.WarmChunkRef) error {
		if _, ok := seen[ref.Key]; ok {
			return nil
		}
		seen[ref.Key] = struct{}{}
		select {
		case w.jobs <- ref:
			return nil
		case <-w.ctx.Done():
			return w.ctx.Err()
		}
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		w.logf("snapshot warming: enumerate S2 self chunks: %v", err)
	}
}

func (w *s2Warmer) worker() {
	defer w.wg.Done()
	for {
		select {
		case <-w.ctx.Done():
			return
		case ref, ok := <-w.jobs:
			if !ok {
				return
			}
			// If cancellation raced a ready jobs receive, do not start a new
			// speculative request after the lifecycle has begun draining.
			select {
			case <-w.ctx.Done():
				return
			default:
			}
			started := time.Now()
			w.markP1Start(ref.Key)
			w.stats.p1Started.Add(1)

			err := w.stream.WarmChunk(w.ctx, ref)
			ended := time.Now()
			w.markP1Stop(ref.Key)
			w.stats.p1WarmLatency.Record(uint64(ended.Sub(started).Nanoseconds()))
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					w.stats.p1Errors.Add(1)
				}
				continue
			}

			w.stats.p1Completed.Add(1)
			if ref.CiphertextBytes > 0 {
				w.stats.p1ReadyCiphertextBytes.Add(ref.CiphertextBytes)
			}
			resumeNs := w.resumeUnixNs.Load()
			if resumeNs == 0 || ended.UnixNano() <= resumeNs {
				w.stats.p1ReadyBeforeResume.Add(1)
			}
		}
	}
}

func (w *s2Warmer) markP1Start(key store.ContentKey) {
	w.conflictMu.Lock()
	defer w.conflictMu.Unlock()
	now := time.Now()
	w.p1Clock.start(now)
	w.pendingP1[key]++
	for token := range w.activeP0[key] {
		token.sameKey = true
	}
}

func (w *s2Warmer) markP1Stop(key store.ContentKey) {
	w.conflictMu.Lock()
	defer w.conflictMu.Unlock()
	now := time.Now()
	if n := w.pendingP1[key]; n <= 1 {
		delete(w.pendingP1, key)
	} else {
		w.pendingP1[key] = n - 1
	}
	w.p1Clock.stop(now)
}

func (w *s2Warmer) BeginDemand(offset uint64, queueDelay time.Duration) uffd.DemandToken {
	token := &demandToken{queueDelay: queueDelay}
	ref, ok := w.stream.DataChunkAt(offset)
	if ok {
		token.key = ref.Key
		token.hasKey = true
	}

	w.conflictMu.Lock()
	now := time.Now()
	token.p1ActiveTimeAtStart = w.p1Clock.totalAt(now)
	if token.hasKey {
		token.sameKey = w.pendingP1[token.key] > 0
		waiters := w.activeP0[token.key]
		if waiters == nil {
			waiters = make(map[*demandToken]struct{})
			w.activeP0[token.key] = waiters
		}
		waiters[token] = struct{}{}
	}
	w.conflictMu.Unlock()
	return token
}

func (w *s2Warmer) EndDemand(raw uffd.DemandToken, fetchLatency, serviceLatency time.Duration, bytes uint64, err error) {
	token, ok := raw.(*demandToken)
	if !ok || token == nil {
		return
	}
	w.conflictMu.Lock()
	ended := time.Now()
	overlap := w.p1Clock.totalAt(ended) - token.p1ActiveTimeAtStart
	sameKey := token.sameKey
	if token.hasKey {
		if waiters := w.activeP0[token.key]; waiters != nil {
			delete(waiters, token)
			if len(waiters) == 0 {
				delete(w.activeP0, token.key)
			}
		}
	}
	w.conflictMu.Unlock()

	// Zero/hole reads are intentionally absent from conflict metrics. A fatal
	// source error can also have bytes==0, but it is still a P0 Data attempt and
	// must remain visible in the latency/conflict classes. EOF is the handler's
	// normal zero-page fallback, not a failed snapshot fetch.
	if bytes == 0 && (err == nil || errors.Is(err, io.EOF)) {
		return
	}
	if overlap < 0 {
		overlap = 0
	} else if overlap > serviceLatency {
		// EndDemand acquires conflictMu just after the handler measured
		// serviceLatency. Exclude that observer-lock tail so the derived
		// overlap ratio remains bounded by the actual P0 service interval.
		overlap = serviceLatency
	}
	w.stats.p0DataFaults.Add(1)
	w.stats.p0ServiceTotalNs.Add(uint64(max(serviceLatency.Nanoseconds(), 0)))
	w.stats.p0P1OverlapNs.Add(uint64(max(overlap.Nanoseconds(), 0)))

	queueNs := uint64(max(token.queueDelay.Nanoseconds(), 0))
	fetchNs := uint64(max(fetchLatency.Nanoseconds(), 0))
	serviceNs := uint64(max(serviceLatency.Nanoseconds(), 0))
	if sameKey {
		w.stats.p0Conflicted.Add(1)
		w.stats.p0SameKeyConflicted.Add(1)
		w.stats.p0QueueWithP1.Record(queueNs)
		w.stats.p0FetchSameKeyP1.Record(fetchNs)
		w.stats.p0ServiceSameKeyP1.Record(serviceNs)
	} else if overlap > 0 {
		w.stats.p0Conflicted.Add(1)
		w.stats.p0QueueWithP1.Record(queueNs)
		w.stats.p0FetchWithP1.Record(fetchNs)
		w.stats.p0ServiceWithP1.Record(serviceNs)
	} else {
		w.stats.p0QueueNoP1.Record(queueNs)
		w.stats.p0FetchNoP1.Record(fetchNs)
		w.stats.p0ServiceNoP1.Record(serviceNs)
	}
}

func (w *s2Warmer) MarkResume() {
	w.resumeUnixNs.CompareAndSwap(0, time.Now().UnixNano())
}

func (w *s2Warmer) Close() {
	if w == nil {
		return
	}
	w.cancel()
	w.wg.Wait()
	w.conflictMu.Lock()
	if w.p1Clock.active > 0 {
		w.p1Clock.accumulated += time.Since(w.p1Clock.activeSince)
		w.p1Clock.active = 0
	}
	clear(w.pendingP1)
	clear(w.activeP0)
	w.conflictMu.Unlock()
}

func (w *s2Warmer) Snapshot() sandbox.WarmingStatsSnapshot {
	w.conflictMu.Lock()
	maxInflight := w.p1Clock.maxActive
	w.conflictMu.Unlock()
	return sandbox.WarmingStatsSnapshot{
		Workers:                w.workers,
		P1Started:              w.stats.p1Started.Load(),
		P1Completed:            w.stats.p1Completed.Load(),
		P1Errors:               w.stats.p1Errors.Load(),
		P1InflightMax:          uint64(maxInflight),
		P1ReadyBeforeResume:    w.stats.p1ReadyBeforeResume.Load(),
		P1ReadyCiphertextBytes: w.stats.p1ReadyCiphertextBytes.Load(),
		P1WarmLatency:          w.stats.p1WarmLatency.Snapshot(),
		P0DataFaults:           w.stats.p0DataFaults.Load(),
		P0ConflictedFaults:     w.stats.p0Conflicted.Load(),
		P0SameKeyConflicted:    w.stats.p0SameKeyConflicted.Load(),
		P0P1OverlapNs:          w.stats.p0P1OverlapNs.Load(),
		P0ServiceTotalNs:       w.stats.p0ServiceTotalNs.Load(),
		P0QueueNoP1:            w.stats.p0QueueNoP1.Snapshot(),
		P0QueueWithP1:          w.stats.p0QueueWithP1.Snapshot(),
		P0FetchNoP1:            w.stats.p0FetchNoP1.Snapshot(),
		P0FetchWithP1:          w.stats.p0FetchWithP1.Snapshot(),
		P0FetchSameKeyP1:       w.stats.p0FetchSameKeyP1.Snapshot(),
		P0ServiceNoP1:          w.stats.p0ServiceNoP1.Snapshot(),
		P0ServiceWithP1:        w.stats.p0ServiceWithP1.Snapshot(),
		P0ServiceSameKeyP1:     w.stats.p0ServiceSameKeyP1.Snapshot(),
	}
}
