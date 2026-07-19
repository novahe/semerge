package manifest

import (
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
)

func TestCacheConfigEffectiveWarmingWorkers(t *testing.T) {
	tests := []struct {
		name    string
		value   int
		want    int
		wantErr bool
	}{
		{name: "default", value: 0, want: DefaultWarmingWorkers},
		{name: "minimum", value: 1, want: 1},
		{name: "configured", value: 8, want: 8},
		{name: "maximum", value: MaxWarmingWorkers, want: MaxWarmingWorkers},
		{name: "negative", value: -1, wantErr: true},
		{name: "over maximum", value: MaxWarmingWorkers + 1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := (CacheConfig{WarmingWorkers: tt.value}).EffectiveWarmingWorkers()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("EffectiveWarmingWorkers(%d) = %d, want error", tt.value, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("EffectiveWarmingWorkers(%d): %v", tt.value, err)
			}
			if got != tt.want {
				t.Fatalf("EffectiveWarmingWorkers(%d) = %d, want %d", tt.value, got, tt.want)
			}
		})
	}
}

func TestNewFetcherRejectsInvalidWarmingWorkersBeforeDial(t *testing.T) {
	cfg := Config{
		Cache: CacheConfig{
			Endpoint:       "unix:///not-dialed",
			WarmingWorkers: MaxWarmingWorkers + 1,
		},
		Crypto: crypto.Config{Chunk: "aes", Manifest: "aes"},
	}
	_, err := cfg.NewFetcher(func() ([32]byte, error) { return [32]byte{}, nil })
	if err == nil || !strings.Contains(err.Error(), "cache.warming_workers") {
		t.Fatalf("NewFetcher error = %v, want cache.warming_workers validation", err)
	}
}
