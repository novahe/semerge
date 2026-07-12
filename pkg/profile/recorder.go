package profile

import (
	"encoding/json"
	"sort"
	"sync"
)

// maxPages avoids unbound memory usage during cold start recording
const defaultMaxPages = 16384 // 64 MiB

// MaxCoalesceGap is the maximum gap between pages that can be coalesced into a single range
const MaxCoalesceGap = 0

// Recorder safely tracks memory page access during the cold start window.
// It is designed to be called concurrently from the UFFD handler's workers.
type Recorder struct {
	mu           sync.Mutex
	active       bool
	started      bool
	maxPages     uint64
	orderCnt     uint32
	droppedPages uint64
	pages        map[uint64]uint32 // pageIdx -> order
	seedPages    map[uint64]uint32 // inherited profile pages retained across Start
	seedOrderCnt uint32
	inflight     sync.WaitGroup
}

type recordedPage struct {
	pageIdx uint64
	order   uint32
}

// NewRecorder initializes a new Recorder with default limits.
func NewRecorder() *Recorder {
	return &Recorder{
		maxPages:  defaultMaxPages,
		pages:     make(map[uint64]uint32),
		seedPages: make(map[uint64]uint32),
	}
}

// NewRecorderWithBase creates an incremental recorder seeded by a parent
// snapshot profile. Parent pages keep their priority; pages faulted during the
// new generation are deduplicated and appended until the same 64 MiB cap is
// reached. The base is not revalidated here; callers must pass a profile
// returned by Parse.
func NewRecorderWithBase(base *MemoryProfile) *Recorder {
	r := NewRecorder()
	target := base.MemoryTarget()
	if target == nil {
		return r
	}
	ranges := append([]Range(nil), target.Ranges...)
	sort.SliceStable(ranges, func(i, j int) bool {
		if ranges[i].Order == ranges[j].Order {
			return ranges[i].Offset < ranges[j].Offset
		}
		return ranges[i].Order < ranges[j].Order
	})
	for _, rg := range ranges {
		for off, end := rg.Offset, rg.Offset+rg.Length; off < end; off += PageSize {
			if uint64(len(r.seedPages)) >= r.maxPages {
				break
			}
			pageIdx := off / PageSize
			if _, exists := r.seedPages[pageIdx]; exists {
				continue
			}
			r.seedPages[pageIdx] = r.seedOrderCnt
			r.seedOrderCnt++
		}
		if uint64(len(r.seedPages)) >= r.maxPages {
			break
		}
	}
	r.resetPagesLocked()
	return r
}

func (r *Recorder) resetPagesLocked() {
	r.pages = make(map[uint64]uint32, len(r.seedPages))
	for pageIdx, order := range r.seedPages {
		r.pages[pageIdx] = order
	}
	r.orderCnt = r.seedOrderCnt
}

// Start begins the recording session, clearing any previous state.
func (r *Recorder) Start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active = true
	r.started = true
	r.resetPagesLocked()
	r.droppedPages = 0
}

// RecordResolved marks a page as accessed. It is called by the UFFD handler
// after successfully loading a page (e.g., via COPY or ZEROPAGE) when the
// recorder is active.
func (r *Recorder) Reserve() (uint32, bool) {
	seq, recording, _ := r.ReserveFault()
	return seq, recording
}

// ReserveFault atomically snapshots both recorder decisions for one PAGEFAULT.
// singlePage is true before Start and while active, preventing speculative
// bootstrap batches from hiding application pages. It becomes false after
// Freeze so the normal 256-page batching path resumes.
func (r *Recorder) ReserveFault() (seq uint32, recording, singlePage bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	singlePage = !r.started || r.active
	if !r.active {
		return 0, false, singlePage
	}
	seq = r.orderCnt
	r.orderCnt++
	r.inflight.Add(1)
	return seq, true, singlePage
}

func (r *Recorder) Commit(pageOffset uint64, seq uint32) {
	defer r.inflight.Done()
	r.mu.Lock()
	defer r.mu.Unlock()

	pageIdx := pageOffset / PageSize
	if old, exists := r.pages[pageIdx]; !exists || seq < old {
		if !exists && uint64(len(r.pages)) >= r.maxPages {
			r.droppedPages++
			return
		}
		r.pages[pageIdx] = seq
	}
}

func (r *Recorder) Discard() { r.inflight.Done() }

// RecordResolved remains a test/helper path; production reserves order when
// the PAGEFAULT event arrives and commits it only after successful resolution.
func (r *Recorder) RecordResolved(pageOffset uint64) {
	if seq, ok := r.Reserve(); ok {
		r.Commit(pageOffset, seq)
	}
}

// Freeze stops the recorder and generates the MemoryProfile based on recorded pages.
func (r *Recorder) Freeze(memorySize uint64) *MemoryProfile {
	r.mu.Lock()
	r.active = false
	r.mu.Unlock()
	r.inflight.Wait()
	r.mu.Lock()
	pagesMap := r.pages
	r.mu.Unlock()

	if len(pagesMap) == 0 {
		return nil
	}

	// The profile drives a priority-ordered prefill. Sort by observed Guest
	// access order, not address; address sorting would turn a reverse access
	// such as page 100 -> 99 into a range that incorrectly fetches 99 first.
	pages := make([]recordedPage, 0, len(pagesMap))
	for idx, order := range pagesMap {
		pages = append(pages, recordedPage{pageIdx: idx, order: order})
	}
	sort.Slice(pages, func(i, j int) bool {
		if pages[i].order == pages[j].order {
			return pages[i].pageIdx < pages[j].pageIdx
		}
		return pages[i].order < pages[j].order
	})

	var ranges []Range

	// Coalesce continuous or nearby pages
	startIdx := pages[0].pageIdx
	endIdx := startIdx + 1
	firstOrder := pages[0].order
	previousOrder := firstOrder
	hits := uint32(1)

	for i := 1; i < len(pages); i++ {
		idx := pages[i].pageIdx
		order := pages[i].order

		// A Range is replayed in ascending address order. Coalesce only a run
		// that was also observed in exactly that order; reverse/interleaved
		// accesses must stay as separate ranges to preserve priority.
		if idx == endIdx && order == previousOrder+1 {
			endIdx = idx + 1
			previousOrder = order
			hits++
		} else {
			// Commit current range
			ranges = append(ranges, Range{
				Offset: startIdx * PageSize,
				Length: (endIdx - startIdx) * PageSize,
				Order:  firstOrder,
				Hits:   hits,
			})

			// Start new range
			startIdx = idx
			endIdx = idx + 1
			firstOrder = order
			previousOrder = order
			hits = 1
		}
	}

	// Commit last range
	ranges = append(ranges, Range{
		Offset: startIdx * PageSize,
		Length: (endIdx - startIdx) * PageSize,
		Order:  firstOrder,
		Hits:   hits,
	})

	return &MemoryProfile{
		Version:  CurrentVersion,
		PageSize: PageSize,
		WindowMs: 2000,
		Targets: []Target{
			{
				Kind:   "memory",
				Size:   memorySize,
				Ranges: ranges,
			},
		},
	}
}

// Marshal is a helper to directly get the JSON representation
func (r *Recorder) Marshal(memorySize uint64) ([]byte, error) {
	prof := r.Freeze(memorySize)
	if prof == nil {
		return nil, nil
	}
	return json.Marshal(prof)
}

// IsActive returns whether the recorder is currently capturing faults.
func (r *Recorder) IsActive() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active
}
