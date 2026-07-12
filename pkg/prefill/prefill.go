// Package prefill loads profiled snapshot pages through UFFDIO_COPY while
// cloud-hypervisor is restoring. Fetch workers overlap CH restore, while the
// caller keeps vCPUs paused until this bounded operation completes.
package prefill

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/profile"
	"github.com/kuasar-sandbox/sandboxer/pkg/uffd"
)

type PrefillOptions struct {
	MaxBytes uint64
	// BufferBytes bounds data fetched before UFFD regions are ready.
	// Zero uses a 64 MiB default; values below MaxBatchBytes are rounded up.
	BufferBytes uint64
	Timeout     time.Duration
	Workers     int
}

type PrefillStats struct {
	RequestedRanges uint64
	CompletedRanges uint64
	SkippedZero     uint64
	BytesRequested  uint64
	BytesFilled     uint64
	BytesExisting   uint64
	FetchErrors     uint64
	BudgetExhausted bool
	Duration        time.Duration
}

type fetchJob struct {
	seq       uint64
	rangeSeq  uint64
	offset    uint64
	length    uint64
	lastRange bool
}

type fetchedSpan struct {
	offset uint64
	length uint64
	zero   bool
}

type fetchResult struct {
	job   fetchJob
	data  []byte
	spans []fetchedSpan
	err   error
}

// Plan is a bounded two-stage startup prefill. Prepare starts remote reads as
// soon as the snapshot source and profile exist; Inject later installs the
// ordered results after CH reports its UFFD regions.
type Plan struct {
	ctx       context.Context
	cancel    context.CancelFunc
	results   chan fetchResult
	slots     chan struct{}
	done      chan struct{}
	closeOnce sync.Once

	requestedRanges uint64
	requestedBytes  uint64
	budgetExhausted bool
	started         time.Time
}

// Prepare begins ordered, bounded profile fetching. Jobs reserve a buffer slot
// before dispatch, in profile order, so later ranges cannot consume all memory
// and starve the earliest range. Each slot is at most MaxBatchBytes.
func Prepare(ctx context.Context, baseSource uffd.SnapshotReader, prof *profile.MemoryProfile, opts PrefillOptions) *Plan {
	if baseSource == nil || prof == nil || opts.MaxBytes == 0 {
		return nil
	}
	target := prof.MemoryTarget()
	if target == nil {
		return nil
	}
	selected, reserved, exhausted := selectRanges(target.Ranges, opts.MaxBytes)
	if len(selected) == 0 {
		return nil
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = 8
	}
	bufferBytes := opts.BufferBytes
	if bufferBytes == 0 {
		bufferBytes = 64 << 20
	}
	if bufferBytes < uffd.MaxBatchBytes {
		bufferBytes = uffd.MaxBatchBytes
	}
	slotCount := int(bufferBytes / uffd.MaxBatchBytes)
	if slotCount < 1 {
		slotCount = 1
	}
	if workers > slotCount {
		workers = slotCount
	}

	planCtx, cancel := context.WithCancel(ctx)
	p := &Plan{
		ctx:             planCtx,
		cancel:          cancel,
		results:         make(chan fetchResult, slotCount),
		slots:           make(chan struct{}, slotCount),
		done:            make(chan struct{}),
		requestedRanges: uint64(len(selected)),
		requestedBytes:  reserved,
		budgetExhausted: exhausted,
		started:         time.Now(),
	}
	jobs := make(chan fetchJob)
	source := baseSource.CloneWithContext(planCtx)
	var workersWG sync.WaitGroup
	for i := 0; i < workers; i++ {
		workersWG.Add(1)
		go func() {
			defer workersWG.Done()
			for job := range jobs {
				result := fetchResult{job: job, data: make([]byte, int(job.length))}
				for cursor := uint64(0); cursor < job.length; {
					n, zero, err := source.ReadAt(result.data[cursor:], job.offset+cursor)
					if err != nil || n == 0 {
						if err == nil {
							err = context.Canceled
						}
						result.err = err
						break
					}
					result.spans = append(result.spans, fetchedSpan{offset: cursor, length: uint64(n), zero: zero})
					cursor += uint64(n)
				}
				select {
				case p.results <- result:
				case <-planCtx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		var seq uint64
		for rangeSeq, r := range selected {
			for off, end := r.Offset, r.Offset+r.Length; off < end; {
				length := end - off
				if length > uffd.MaxBatchBytes {
					length = uffd.MaxBatchBytes
				}
				select {
				case p.slots <- struct{}{}:
				case <-planCtx.Done():
					return
				}
				job := fetchJob{seq: seq, rangeSeq: uint64(rangeSeq), offset: off, length: length, lastRange: off+length == end}
				select {
				case jobs <- job:
				case <-planCtx.Done():
					<-p.slots
					return
				}
				seq++
				off += length
			}
		}
	}()
	go func() {
		workersWG.Wait()
		close(p.results)
		close(p.done)
	}()
	return p
}

// Inject consumes prepared chunks in profile order. Cancelling ctx stops both
// stages; no UFFDIO_COPY can continue after this method returns.
func (p *Plan) Inject(ctx context.Context, h *uffd.Handler) PrefillStats {
	if p == nil || h == nil {
		return PrefillStats{}
	}
	stats := PrefillStats{RequestedRanges: p.requestedRanges, BytesRequested: p.requestedBytes, BudgetExhausted: p.budgetExhausted}
	pending := make(map[uint64]fetchResult)
	failedRanges := make(map[uint64]bool)
	var next uint64
	for {
		if ctx.Err() != nil {
			p.cancel()
			stats.Duration = time.Since(p.started)
			return stats
		}
		result, ok := pending[next]
		if !ok {
			select {
			case result, ok = <-p.results:
				if !ok {
					stats.Duration = time.Since(p.started)
					return stats
				}
				if result.job.seq != next {
					pending[result.job.seq] = result
					continue
				}
			case <-ctx.Done():
				p.cancel()
				stats.Duration = time.Since(p.started)
				return stats
			}
		} else {
			delete(pending, next)
		}

		if result.err != nil {
			stats.FetchErrors++
			failedRanges[result.job.rangeSeq] = true
		} else {
			for _, span := range result.spans {
				if span.zero {
					stats.SkippedZero += span.length
					continue
				}
				start, end := span.offset, span.offset+span.length
				filled, existing, err := h.PrefillCopy(ctx, result.job.offset+start, result.data[start:end])
				stats.BytesFilled += filled
				stats.BytesExisting += existing
				if err != nil {
					stats.FetchErrors++
					failedRanges[result.job.rangeSeq] = true
					break
				}
			}
		}
		if result.job.lastRange && !failedRanges[result.job.rangeSeq] {
			stats.CompletedRanges++
		}
		<-p.slots
		next++
	}
}

// CanceledStats returns the plan metadata when cancellation happens before a
// UFFD handler becomes available. Fetch workers never own UFFD injection, so
// their eventual cleanup is intentionally kept out of the vm.resume gate.
func (p *Plan) CanceledStats() PrefillStats {
	if p == nil {
		return PrefillStats{}
	}
	return PrefillStats{
		RequestedRanges: p.requestedRanges,
		BytesRequested:  p.requestedBytes,
		BudgetExhausted: p.budgetExhausted,
		Duration:        time.Since(p.started),
	}
}

// Close cancels and joins an unused or partially consumed plan.
func (p *Plan) Close() {
	if p == nil {
		return
	}
	p.closeOnce.Do(func() {
		p.cancel()
		<-p.done
		// No producer remains after done. Drop every buffered result so its
		// plaintext payload becomes unreachable immediately instead of being
		// retained by Plan until the restored VM exits.
		for range p.results {
		}
	})
}

// PrefillStartup fetches independent canonical ranges concurrently and injects
// them through the CH-side userfaultfds. UFFDIO_COPY is the ownership arbiter:
// EEXIST means CH already materialized the page, so prefill counts it and
// moves on without touching existing data. EAGAIN is retried by PrefillCopy.
func PrefillStartup(ctx context.Context, h *uffd.Handler, baseSource uffd.SnapshotReader, prof *profile.MemoryProfile, opts PrefillOptions) PrefillStats {
	plan := Prepare(ctx, baseSource, prof, opts)
	if plan == nil {
		return PrefillStats{}
	}
	defer plan.Close()
	prefillCtx := ctx
	cancel := func() {}
	if opts.Timeout > 0 {
		prefillCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
	}
	defer cancel()
	return plan.Inject(prefillCtx, h)
}

func selectRanges(input []profile.Range, maxBytes uint64) ([]profile.Range, uint64, bool) {
	ranges := append([]profile.Range(nil), input...)
	sort.SliceStable(ranges, func(i, j int) bool {
		if ranges[i].Order == ranges[j].Order {
			return ranges[i].Offset < ranges[j].Offset
		}
		return ranges[i].Order < ranges[j].Order
	})
	selected := make([]profile.Range, 0, len(ranges))
	exhausted := false
	var reserved uint64
	for _, r := range ranges {
		remaining := maxBytes - reserved
		if r.Length > remaining {
			exhausted = true
			// Preserve the earliest page-aligned prefix instead of dropping an
			// oversized hot range wholesale.
			remaining -= remaining % profile.PageSize
			if remaining == 0 {
				continue
			}
			r.Length = remaining
		}
		selected = append(selected, r)
		reserved += r.Length
	}
	return selected, reserved, exhausted
}
