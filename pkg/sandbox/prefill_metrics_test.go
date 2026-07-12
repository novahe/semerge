package sandbox

import (
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/prefill"
	"github.com/kuasar-sandbox/sandboxer/pkg/uffd"
)

func TestPrefillMetricsSinkAppliesOutcomeToLateHandler(t *testing.T) {
	sink := &prefillMetricsSink{}
	sink.RecordStats(prefill.PrefillStats{
		BytesRequested:  8 << 20,
		BytesFilled:     4 << 20,
		Duration:        12 * time.Millisecond,
		BudgetExhausted: true,
	})
	sink.RecordResumeWait(uint64((100 * time.Millisecond).Nanoseconds()), true)

	h := &uffd.Handler{}
	sink.Attach(h)
	stats := h.Stats()
	if got := stats["prefill_requested_bytes"]; got != 8<<20 {
		t.Fatalf("prefill_requested_bytes=%d, want %d", got, 8<<20)
	}
	if got := stats["prefill_filled_bytes"]; got != 4<<20 {
		t.Fatalf("prefill_filled_bytes=%d, want %d", got, 4<<20)
	}
	if got := stats["prefill_timed_out"]; got != 1 {
		t.Fatalf("prefill_timed_out=%d, want 1", got)
	}
	if got := stats["prefill_budget_exhausted"]; got != 1 {
		t.Fatalf("prefill_budget_exhausted=%d, want 1", got)
	}
}
