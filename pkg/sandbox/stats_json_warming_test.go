package sandbox

import (
	"encoding/json"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/uffd"
)

func latencySamples(values ...uint64) uffd.LatSnapshot {
	var h uffd.LatencyHistogram
	for _, v := range values {
		h.Record(v)
	}
	return h.Snapshot()
}

func TestBuildWarmingJSONNullAmplificationAndInsufficientData(t *testing.T) {
	got := buildWarmingJSON(WarmingStatsSnapshot{P0DataFaults: 99})
	if got.Conflict.Severity != "insufficient_data" {
		t.Fatalf("severity = %q, want insufficient_data", got.Conflict.Severity)
	}
	if got.Conflict.FetchP99Amplification != nil || got.Conflict.ServiceP99Amplification != nil ||
		got.Conflict.QueueP99Amplification != nil || got.Conflict.SameKeyP99Amplification != nil {
		t.Fatal("amplification without baseline/samples must be nil")
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	conflict := decoded["conflict"].(map[string]any)
	if value, ok := conflict["service_p99_amplification"]; !ok || value != nil {
		t.Fatalf("service amplification JSON = %#v, want explicit null", value)
	}
}

func TestBuildWarmingJSONSeverity(t *testing.T) {
	base := latencySamples(100, 100, 100)
	high := latencySamples(120, 120, 120)
	got := buildWarmingJSON(WarmingStatsSnapshot{
		P0DataFaults:       100,
		P0ConflictedFaults: 10,
		P0ServiceNoP1:      base,
		P0ServiceWithP1:    high,
	})
	if got.Conflict.Severity != "high" {
		t.Fatalf("severity = %q, want high", got.Conflict.Severity)
	}
	if got.Conflict.ServiceP99Amplification == nil || *got.Conflict.ServiceP99Amplification < 1.10 {
		t.Fatalf("service amplification = %v, want >=1.10", got.Conflict.ServiceP99Amplification)
	}
}

func TestBuildWarmingJSONClassRates(t *testing.T) {
	got := buildWarmingJSON(WarmingStatsSnapshot{
		P0DataFaults:        20,
		P0ConflictedFaults:  5,
		P0SameKeyConflicted: 2,
		P0P1OverlapNs:       25,
		P0ServiceTotalNs:    100,
	})
	if got.Conflict.ConflictRate != .25 || got.Conflict.SameKeyConflictRate != .10 || got.Conflict.P0P1TimeOverlapRatio != .25 {
		t.Fatalf("rates = conflict %.2f same %.2f overlap %.2f", got.Conflict.ConflictRate,
			got.Conflict.SameKeyConflictRate, got.Conflict.P0P1TimeOverlapRatio)
	}
}
