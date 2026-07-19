package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/guestlink"
	"github.com/kuasar-sandbox/sandboxer/pkg/uffd"
	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
)

// statsReport is the JSON layout written by --stats-json. The bucket
// bounds are included once at the top so consumers can interpret each
// backend's lat_buckets array. The last element of lat_buckets is the
// >max-bucket-bound overflow (latency exceeded all named bounds).
type statsReport struct {
	BucketsNs []uint64                `json:"latency_bucket_bounds_ns"`
	Backends  []statsBackendJSON      `json:"backends"`
	Uffd      *uffdStatsJSON          `json:"uffd,omitempty"`
	Runtime   *runtimeStatsJSON       `json:"runtime,omitempty"`
	Ping      *guestlink.PingSnapshot `json:"ping,omitempty"`
	Warming   *warmingStatsJSON       `json:"warming,omitempty"`
	Wallclock wallclockJSON           `json:"wallclock"`
}

// WarmingStatsSnapshot is the concurrency-safe point-in-time contract between
// the restore warmer and the shared sandbox stats writer. It intentionally
// contains raw counters/histograms; rates, amplification, null handling, and
// severity are derived once while rendering JSON.
type WarmingStatsSnapshot struct {
	Workers                int
	P1Started              uint64
	P1Completed            uint64
	P1Errors               uint64
	P1InflightMax          uint64
	P1ReadyBeforeResume    uint64
	P1ReadyCiphertextBytes uint64
	P1WarmLatency          uffd.LatSnapshot

	P0DataFaults        uint64
	P0ConflictedFaults  uint64
	P0SameKeyConflicted uint64
	P0P1OverlapNs       uint64
	P0ServiceTotalNs    uint64
	P0QueueNoP1         uffd.LatSnapshot
	P0QueueWithP1       uffd.LatSnapshot
	P0FetchNoP1         uffd.LatSnapshot
	P0FetchWithP1       uffd.LatSnapshot
	P0FetchSameKeyP1    uffd.LatSnapshot
	P0ServiceNoP1       uffd.LatSnapshot
	P0ServiceWithP1     uffd.LatSnapshot
	P0ServiceSameKeyP1  uffd.LatSnapshot
}

type warmingStatsJSON struct {
	Workers                int                 `json:"workers"`
	P1Started              uint64              `json:"p1_started"`
	P1Completed            uint64              `json:"p1_completed"`
	P1Errors               uint64              `json:"p1_errors"`
	P1InflightMax          uint64              `json:"p1_inflight_max"`
	P1ReadyBeforeResume    uint64              `json:"p1_ready_before_resume"`
	P1ReadyCiphertextBytes uint64              `json:"p1_ready_ciphertext_bytes"`
	P1WarmLatency          latencyStatsJSON    `json:"p1_warm_latency"`
	Conflict               warmingConflictJSON `json:"conflict"`
}

type warmingConflictJSON struct {
	P0DataFaults              uint64           `json:"p0_data_faults"`
	P0ConflictedFaults        uint64           `json:"p0_conflicted_faults"`
	P0SameKeyConflictedFaults uint64           `json:"p0_same_key_conflicted_faults"`
	ConflictRate              float64          `json:"conflict_rate"`
	SameKeyConflictRate       float64          `json:"same_key_conflict_rate"`
	P0P1TimeOverlapRatio      float64          `json:"p0_p1_time_overlap_ratio"`
	FetchP99Amplification     *float64         `json:"fetch_p99_amplification"`
	ServiceP99Amplification   *float64         `json:"service_p99_amplification"`
	QueueP99Amplification     *float64         `json:"queue_p99_amplification"`
	SameKeyP99Amplification   *float64         `json:"same_key_p99_amplification"`
	Severity                  string           `json:"severity"`
	P0QueueNoP1               latencyStatsJSON `json:"p0_queue_no_p1"`
	P0QueueWithP1             latencyStatsJSON `json:"p0_queue_with_p1"`
	P0FetchNoP1               latencyStatsJSON `json:"p0_fetch_no_p1"`
	P0FetchWithP1             latencyStatsJSON `json:"p0_fetch_with_p1"`
	P0FetchSameKeyP1          latencyStatsJSON `json:"p0_fetch_same_key_p1"`
	P0ServiceNoP1             latencyStatsJSON `json:"p0_service_no_p1"`
	P0ServiceWithP1           latencyStatsJSON `json:"p0_service_with_p1"`
	P0ServiceSameKeyP1        latencyStatsJSON `json:"p0_service_same_key_p1"`
}

type latencyStatsJSON struct {
	Count      uint64   `json:"count"`
	SumNs      uint64   `json:"sum_ns"`
	MaxNs      uint64   `json:"max_ns"`
	P50Ns      uint64   `json:"p50_ns"`
	P99Ns      uint64   `json:"p99_ns"`
	LatBuckets []uint64 `json:"lat_buckets"`
}

// uffdStatsJSON captures the page-fault handler counters plus a
// derived "lazy-load ratio" so consumers (perf bench) can read the
// resident-memory share without recomputing. Both file:// and
// manifest:// snapshot paths surface the same shape.
type uffdStatsJSON struct {
	FaultsAbsent        uint64 `json:"faults_absent"`
	FaultsReleased      uint64 `json:"faults_released"`
	FaultsLoaded        uint64 `json:"faults_loaded"`
	ZeropageCalls       uint64 `json:"zeropage_calls"`
	CopyCalls           uint64 `json:"copy_calls"`
	PagesZeroed         uint64 `json:"pages_zeroed"`
	PagesCopied         uint64 `json:"pages_copied"`
	Wakes               uint64 `json:"wakes"`
	RemoveEvents        uint64 `json:"remove_events"`
	RemoveQDropped      uint64 `json:"remove_q_dropped"`
	RemoveEventsBatched uint64 `json:"remove_events_batched"`
	MadviseCalls        uint64 `json:"madvise_calls"`
	MadviseBytes        uint64 `json:"madvise_bytes"`
	BackendLookupMiss   uint64 `json:"backend_lookup_miss"`
	Errors              uint64 `json:"errors"`
	BatchCalls          uint64 `json:"batch_calls"`
	BatchPagesTotal     uint64 `json:"batch_pages_total"`
	BatchAvgPages       uint64 `json:"batch_avg_pages"`
	BatchMaxPages       uint64 `json:"batch_max_pages"`
	// LazyLoadRatio = (pages_zeroed + pages_copied) / total_pages.
	// total_pages comes from RAMSize/PageSize. Cold-start tracks how
	// little of declared RAM the guest actually touches; restore tracks
	// how much of the snapshot was demand-paged before app rebooted.
	TotalPages    uint64  `json:"total_pages"`
	ResidentPages uint64  `json:"resident_pages"`
	LazyLoadRatio float64 `json:"lazy_load_ratio"`
}

// runtimeStatsJSON captures Go runtime memory + GC counters at
// sandbox-ctl shutdown. Useful to spot allocator regressions in the
// hot uffd / vhost paths without external profilers.
type runtimeStatsJSON struct {
	NumGoroutine    int    `json:"num_goroutine"`
	NumGC           uint32 `json:"num_gc"`
	GCPauseTotalNs  uint64 `json:"gc_pause_total_ns"`
	HeapAllocBytes  uint64 `json:"heap_alloc_bytes"`
	HeapInuseBytes  uint64 `json:"heap_inuse_bytes"`
	HeapSysBytes    uint64 `json:"heap_sys_bytes"`
	HeapObjects     uint64 `json:"heap_objects"`
	TotalAllocBytes uint64 `json:"total_alloc_bytes"`
	Mallocs         uint64 `json:"mallocs"`
	Frees           uint64 `json:"frees"`
	StackInuseBytes uint64 `json:"stack_inuse_bytes"`
}

type wallclockJSON struct {
	StartUnixNs int64 `json:"start_unix_ns"`
	EndUnixNs   int64 `json:"end_unix_ns"`
	DurationMs  int64 `json:"duration_ms"`
}

type statsBackendJSON struct {
	Name          string         `json:"name"`
	Path          string         `json:"path"`
	BlockBytes    uint64         `json:"block_bytes"`
	TotalBlocks   uint64         `json:"total_blocks"`
	LoadedBlocks  uint64         `json:"loaded_blocks"`
	WrittenBlocks uint64         `json:"written_blocks"`
	Read          reqJSON        `json:"read"`
	Write         reqJSON        `json:"write"`
	Flush         reqJSON        `json:"flush"`
	Discard       reqJSON        `json:"discard"`
	Extra         map[string]any `json:"extra,omitempty"`
}

type reqJSON struct {
	Count      uint64   `json:"count"`
	Bytes      uint64   `json:"bytes"`
	ErrCount   uint64   `json:"err_count"`
	LatSumNs   uint64   `json:"lat_sum_ns"`
	LatMaxNs   uint64   `json:"lat_max_ns"`
	LatBuckets []uint64 `json:"lat_buckets"`
	P50Ns      uint64   `json:"p50_ns"`
	P99Ns      uint64   `json:"p99_ns"`
}

func toReqJSON(r vhost.ReqSnapshot) reqJSON {
	buckets := make([]uint64, len(r.LatBuckets))
	copy(buckets, r.LatBuckets[:])
	return reqJSON{
		Count:      r.Count,
		Bytes:      r.Bytes,
		ErrCount:   r.ErrCount,
		LatSumNs:   r.LatSumNs,
		LatMaxNs:   r.LatMaxNs,
		LatBuckets: buckets,
		P50Ns:      r.P50(),
		P99Ns:      r.P99(),
	}
}

// statsBundle holds everything writeStatsJSON needs. Beats threading
// 6 separate args through callers — single struct, easy to extend.
type statsBundle struct {
	Servers     []*vhost.Server
	Uffd        map[string]uint64       // counter snapshot from uffd.Handler.Stats()
	UffdRAMSize int64                   // total RAM in bytes; 0 ⇒ skip lazy-ratio
	Ping        *guestlink.PingSnapshot // host→guest health probe stats; nil ⇒ omit
	Warming     *WarmingStatsSnapshot
	StartUnixNs int64 // sandbox.Run T0
	EndUnixNs   int64 // sandbox.Run T_exit
}

// writeStatsJSON renders the bundle as a single JSON document at path.
func writeStatsJSON(path string, b statsBundle) error {
	report := statsReport{
		BucketsNs: vhost.LatencyBucketBoundsNs(),
		Wallclock: wallclockJSON{
			StartUnixNs: b.StartUnixNs,
			EndUnixNs:   b.EndUnixNs,
			DurationMs:  (b.EndUnixNs - b.StartUnixNs) / int64(time.Millisecond),
		},
	}
	for _, srv := range b.Servers {
		if srv == nil || srv.Stats() == nil {
			continue
		}
		snap := srv.SnapshotStats()
		report.Backends = append(report.Backends, statsBackendJSON{
			Name:          snap.Name,
			Path:          snap.Path,
			BlockBytes:    snap.BlockBytes,
			TotalBlocks:   snap.TotalBlocks,
			LoadedBlocks:  snap.LoadedBlocks,
			WrittenBlocks: snap.WrittenBlocks,
			Read:          toReqJSON(snap.Read),
			Write:         toReqJSON(snap.Write),
			Flush:         toReqJSON(snap.Flush),
			Discard:       toReqJSON(snap.Discard),
			Extra:         snap.Extra,
		})
	}
	if b.Uffd != nil {
		report.Uffd = buildUffdJSON(b.Uffd, b.UffdRAMSize)
	}
	if b.Ping != nil {
		report.Ping = b.Ping
	}
	if b.Warming != nil {
		report.Warming = buildWarmingJSON(*b.Warming)
	}
	report.Runtime = collectRuntimeStats()

	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal stats: %w", err)
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func latencyJSON(s uffd.LatSnapshot) latencyStatsJSON {
	buckets := make([]uint64, len(s.Buckets))
	copy(buckets, s.Buckets[:])
	return latencyStatsJSON{
		Count: s.Count, SumNs: s.SumNs, MaxNs: s.MaxNs,
		P50Ns: s.P50(), P99Ns: s.P99(), LatBuckets: buckets,
	}
}

func ratio(n, d uint64) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

func amplification(with, baseline uffd.LatSnapshot) *float64 {
	base := baseline.P99()
	if base == 0 || with.Count == 0 {
		return nil
	}
	v := float64(with.P99()) / float64(base)
	return &v
}

func amplificationAtLeast(v *float64, threshold float64) bool {
	return v != nil && *v >= threshold
}

func buildWarmingJSON(s WarmingStatsSnapshot) *warmingStatsJSON {
	fetchAmp := amplification(s.P0FetchWithP1, s.P0FetchNoP1)
	serviceAmp := amplification(s.P0ServiceWithP1, s.P0ServiceNoP1)
	queueAmp := amplification(s.P0QueueWithP1, s.P0QueueNoP1)
	sameKeyAmp := amplification(s.P0ServiceSameKeyP1, s.P0ServiceNoP1)
	conflictRate := ratio(s.P0ConflictedFaults, s.P0DataFaults)

	severity := "low"
	if s.P0DataFaults < 100 {
		severity = "insufficient_data"
	} else if amplificationAtLeast(serviceAmp, 1.10) ||
		amplificationAtLeast(sameKeyAmp, 1.10) ||
		amplificationAtLeast(queueAmp, 1.10) {
		severity = "high"
	} else if conflictRate >= 0.05 &&
		(amplificationAtLeast(fetchAmp, 1.05) ||
			amplificationAtLeast(serviceAmp, 1.05) ||
			amplificationAtLeast(queueAmp, 1.05) ||
			amplificationAtLeast(sameKeyAmp, 1.05)) {
		severity = "medium"
	}

	return &warmingStatsJSON{
		Workers:                s.Workers,
		P1Started:              s.P1Started,
		P1Completed:            s.P1Completed,
		P1Errors:               s.P1Errors,
		P1InflightMax:          s.P1InflightMax,
		P1ReadyBeforeResume:    s.P1ReadyBeforeResume,
		P1ReadyCiphertextBytes: s.P1ReadyCiphertextBytes,
		P1WarmLatency:          latencyJSON(s.P1WarmLatency),
		Conflict: warmingConflictJSON{
			P0DataFaults:              s.P0DataFaults,
			P0ConflictedFaults:        s.P0ConflictedFaults,
			P0SameKeyConflictedFaults: s.P0SameKeyConflicted,
			ConflictRate:              conflictRate,
			SameKeyConflictRate:       ratio(s.P0SameKeyConflicted, s.P0DataFaults),
			P0P1TimeOverlapRatio:      ratio(s.P0P1OverlapNs, s.P0ServiceTotalNs),
			FetchP99Amplification:     fetchAmp,
			ServiceP99Amplification:   serviceAmp,
			QueueP99Amplification:     queueAmp,
			SameKeyP99Amplification:   sameKeyAmp,
			Severity:                  severity,
			P0QueueNoP1:               latencyJSON(s.P0QueueNoP1),
			P0QueueWithP1:             latencyJSON(s.P0QueueWithP1),
			P0FetchNoP1:               latencyJSON(s.P0FetchNoP1),
			P0FetchWithP1:             latencyJSON(s.P0FetchWithP1),
			P0FetchSameKeyP1:          latencyJSON(s.P0FetchSameKeyP1),
			P0ServiceNoP1:             latencyJSON(s.P0ServiceNoP1),
			P0ServiceWithP1:           latencyJSON(s.P0ServiceWithP1),
			P0ServiceSameKeyP1:        latencyJSON(s.P0ServiceSameKeyP1),
		},
	}
}

// buildUffdJSON converts the uffd.Handler.Stats() counter map plus
// the declared RAM size into the typed JSON shape, computing the
// lazy-load ratio inline.
func buildUffdJSON(counters map[string]uint64, ramBytes int64) *uffdStatsJSON {
	zeroed := counters["pages_zeroed"]
	copied := counters["pages_copied"]
	resident := zeroed + copied
	var totalPages uint64
	var ratio float64
	if ramBytes > 0 {
		const pageSize = 4096
		totalPages = uint64(ramBytes) / pageSize
		if totalPages > 0 {
			ratio = float64(resident) / float64(totalPages)
		}
	}
	return &uffdStatsJSON{
		FaultsAbsent:        counters["faults_absent"],
		FaultsReleased:      counters["faults_released"],
		FaultsLoaded:        counters["faults_loaded"],
		ZeropageCalls:       counters["zeropage_calls"],
		CopyCalls:           counters["copy_calls"],
		PagesZeroed:         zeroed,
		PagesCopied:         copied,
		Wakes:               counters["wakes"],
		RemoveEvents:        counters["remove_events"],
		RemoveQDropped:      counters["remove_q_dropped"],
		RemoveEventsBatched: counters["remove_events_batched"],
		MadviseCalls:        counters["madvise_calls"],
		MadviseBytes:        counters["madvise_bytes"],
		BackendLookupMiss:   counters["backend_lookup_miss"],
		Errors:              counters["errors"],
		BatchCalls:          counters["batch_calls"],
		BatchPagesTotal:     counters["batch_pages_total"],
		BatchAvgPages:       counters["batch_avg_pages"],
		BatchMaxPages:       counters["batch_max_pages"],
		TotalPages:          totalPages,
		ResidentPages:       resident,
		LazyLoadRatio:       ratio,
	}
}

func collectRuntimeStats() *runtimeStatsJSON {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return &runtimeStatsJSON{
		NumGoroutine:    runtime.NumGoroutine(),
		NumGC:           ms.NumGC,
		GCPauseTotalNs:  ms.PauseTotalNs,
		HeapAllocBytes:  ms.HeapAlloc,
		HeapInuseBytes:  ms.HeapInuse,
		HeapSysBytes:    ms.HeapSys,
		HeapObjects:     ms.HeapObjects,
		TotalAllocBytes: ms.TotalAlloc,
		Mallocs:         ms.Mallocs,
		Frees:           ms.Frees,
		StackInuseBytes: ms.StackInuse,
	}
}
