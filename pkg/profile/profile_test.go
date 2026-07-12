package profile

import (
	"encoding/json"
	"sort"
	"testing"
)

func TestParsePreservesOrderedAdjacentRanges(t *testing.T) {
	p := MemoryProfile{Version: 1, PageSize: PageSize, Targets: []Target{{Kind: "memory", Size: 8 * PageSize, Ranges: []Range{
		{Offset: PageSize, Length: PageSize, Order: 0, Hits: 1},
		{Offset: 0, Length: PageSize, Order: 1, Hits: 1},
	}}}}
	b, _ := json.Marshal(p)
	got, err := Parse(b, 8*PageSize)
	if err != nil {
		t.Fatal(err)
	}
	r := got.MemoryTarget().Ranges
	if len(r) != 2 || r[0].Offset != PageSize || r[0].Order != 0 || r[1].Offset != 0 || r[1].Order != 1 {
		t.Fatalf("ordered ranges = %+v", r)
	}
}

func TestParseRejectsOverlappingRanges(t *testing.T) {
	p := MemoryProfile{Version: 1, PageSize: PageSize, Targets: []Target{{Kind: "memory", Size: 8 * PageSize, Ranges: []Range{
		{Offset: 0, Length: 2 * PageSize, Order: 0},
		{Offset: PageSize, Length: PageSize, Order: 1},
	}}}}
	b, _ := json.Marshal(p)
	if _, err := Parse(b, 8*PageSize); err == nil {
		t.Fatal("accepted overlapping ranges")
	}
}

func TestParseRejectsInvalidRanges(t *testing.T) {
	for _, r := range []Range{{Offset: 0, Length: 0}, {Offset: 1, Length: PageSize}, {Offset: 0, Length: PageSize + 1}, {Offset: 8 * PageSize, Length: PageSize}} {
		p := MemoryProfile{Version: 1, PageSize: PageSize, Targets: []Target{{Kind: "memory", Size: 8 * PageSize, Ranges: []Range{r}}}}
		b, _ := json.Marshal(p)
		if _, err := Parse(b, 8*PageSize); err == nil {
			t.Fatalf("accepted %+v", r)
		}
	}
}

func TestRecorderFreezeStopsAndDoesNotBridgeGaps(t *testing.T) {
	r := NewRecorder()
	if _, recording, singlePage := r.ReserveFault(); recording || !singlePage {
		t.Fatalf("before start: recording=%v singlePage=%v", recording, singlePage)
	}
	r.Start()
	_, recording, singlePage := r.ReserveFault()
	if !recording || !singlePage {
		t.Fatalf("active: recording=%v singlePage=%v", recording, singlePage)
	}
	r.Discard()
	r.RecordResolved(0)
	r.RecordResolved(PageSize)
	r.RecordResolved(3 * PageSize)
	p := r.Freeze(8 * PageSize)
	if len(p.MemoryTarget().Ranges) != 2 {
		t.Fatalf("ranges = %+v", p.MemoryTarget().Ranges)
	}
	r.RecordResolved(4 * PageSize)
	if got := r.Freeze(8 * PageSize); len(got.MemoryTarget().Ranges) != 2 {
		t.Fatalf("recorded after freeze: %+v", got)
	}
	if _, recording, singlePage := r.ReserveFault(); recording || singlePage {
		t.Fatalf("after freeze: recording=%v singlePage=%v", recording, singlePage)
	}
}

func TestRecorderPreservesFaultArrivalOrder(t *testing.T) {
	r := NewRecorder()
	r.Start()
	// Reverse-adjacent accesses cannot be coalesced because a Range is replayed
	// in ascending address order.
	r.RecordResolved(PageSize)
	r.RecordResolved(0)
	p := r.Freeze(4 * PageSize)
	got := p.MemoryTarget().Ranges
	if len(got) != 2 || got[0].Offset != PageSize || got[0].Order != 0 || got[1].Offset != 0 || got[1].Order != 1 {
		t.Fatalf("ranges=%+v", got)
	}
}

func TestRecorderDoesNotMergeInterleavedAdjacentPages(t *testing.T) {
	r := NewRecorder()
	r.Start()
	for _, page := range []uint64{0, 2, 1, 3, 4} {
		r.RecordResolved(page * PageSize)
	}
	got := r.Freeze(8 * PageSize).MemoryTarget().Ranges
	if len(got) != 4 || got[0].Offset != 0 || got[1].Offset != 2*PageSize || got[2].Offset != PageSize || got[3].Offset != 3*PageSize || got[3].Length != 2*PageSize {
		t.Fatalf("ranges=%+v", got)
	}
}

func TestIncrementalRecorderRetainsParentAndAppendsNewPages(t *testing.T) {
	base := &MemoryProfile{Version: CurrentVersion, PageSize: PageSize, WindowMs: 2000, Targets: []Target{{
		Kind: "memory", Size: 8 * PageSize,
		Ranges: []Range{{Offset: 0, Length: 2 * PageSize, Order: 0, Hits: 2}},
	}}}
	r := NewRecorderWithBase(base)
	r.Start()
	r.RecordResolved(PageSize)     // duplicate parent page
	r.RecordResolved(3 * PageSize) // new generation page
	got := profilePageOffsets(r.Freeze(8 * PageSize))
	want := []uint64{0, PageSize, 3 * PageSize}
	if len(got) != len(want) {
		t.Fatalf("pages=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pages=%v want=%v", got, want)
		}
	}
}

func TestIncrementalRecorderSupportsMultipleSnapshotGenerations(t *testing.T) {
	base := &MemoryProfile{Version: CurrentVersion, PageSize: PageSize, WindowMs: 2000, Targets: []Target{{
		Kind: "memory", Size: 8 * PageSize,
		Ranges: []Range{{Offset: 4 * PageSize, Length: PageSize, Order: 0, Hits: 1}},
	}}}
	gen1 := NewRecorderWithBase(base)
	gen1.Start()
	gen1.RecordResolved(2 * PageSize)
	p1 := gen1.Freeze(8 * PageSize)

	gen2 := NewRecorderWithBase(p1)
	gen2.Start()
	gen2.RecordResolved(6 * PageSize)
	got := profilePageOffsets(gen2.Freeze(8 * PageSize))
	want := []uint64{4 * PageSize, 2 * PageSize, 6 * PageSize}
	if len(got) != len(want) {
		t.Fatalf("pages=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pages=%v want=%v", got, want)
		}
	}
}

func TestIncrementalRecorderMarshalRetainsBaseWithoutNewFaults(t *testing.T) {
	base := &MemoryProfile{Version: CurrentVersion, PageSize: PageSize, WindowMs: 2000, Targets: []Target{{
		Kind: "memory", Size: 8 * PageSize,
		Ranges: []Range{{Offset: PageSize, Length: 2 * PageSize, Order: 0, Hits: 2}},
	}}}
	r := NewRecorderWithBase(base)
	r.Start()
	b, err := r.Marshal(8 * PageSize)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(b, 8*PageSize)
	if err != nil {
		t.Fatal(err)
	}
	pages := profilePageOffsets(got)
	if len(pages) != 2 || pages[0] != PageSize || pages[1] != 2*PageSize {
		t.Fatalf("pages=%v", pages)
	}
}

func TestIncrementalRecorderKeepsTotalPageLimit(t *testing.T) {
	base := &MemoryProfile{Version: CurrentVersion, PageSize: PageSize, WindowMs: 2000, Targets: []Target{{
		Kind: "memory", Size: 8 * PageSize,
		Ranges: []Range{{Offset: 0, Length: 2 * PageSize, Order: 0, Hits: 2}},
	}}}
	r := NewRecorderWithBase(base)
	r.maxPages = 3
	r.Start()
	r.RecordResolved(2 * PageSize)
	r.RecordResolved(3 * PageSize)
	got := profilePageOffsets(r.Freeze(8 * PageSize))
	if len(got) != 3 || got[2] != 2*PageSize {
		t.Fatalf("bounded pages=%v", got)
	}
}

func profilePageOffsets(p *MemoryProfile) []uint64 {
	ranges := append([]Range(nil), p.MemoryTarget().Ranges...)
	sort.SliceStable(ranges, func(i, j int) bool {
		if ranges[i].Order == ranges[j].Order {
			return ranges[i].Offset < ranges[j].Offset
		}
		return ranges[i].Order < ranges[j].Order
	})
	var pages []uint64
	for _, r := range ranges {
		for off, end := r.Offset, r.Offset+r.Length; off < end; off += PageSize {
			pages = append(pages, off)
		}
	}
	return pages
}
