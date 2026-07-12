// Package profile provides data structures and parsing logic for the startup profile.
// The startup profile is a JSON document generated during a cold start that records
// the memory pages accessed by the guest. During a subsequent restore, this profile
// is used to asynchronously prefill the hot memory pages to reduce UFFD latency.
package profile

import (
	"encoding/json"
	"fmt"
	"sort"
)

const (
	// CurrentVersion is the supported version of the startup profile format.
	CurrentVersion = 1
	// PageSize is the assumed memory page size (4 KiB).
	PageSize        = 4096
	MaxProfileBytes = 1 << 20
	MaxRanges       = 32768
)

// Range represents a continuous block of memory accessed during cold start.
type Range struct {
	Offset uint64 `json:"offset"`
	Length uint64 `json:"length"`
	Order  uint32 `json:"order"`
	Hits   uint32 `json:"hits"`
}

// Target represents a specific profiling target (e.g., "memory").
type Target struct {
	Kind   string  `json:"kind"`
	Size   uint64  `json:"size"`
	Ranges []Range `json:"ranges"`
}

// MemoryProfile is the top-level structure of the startup-profile.json file.
type MemoryProfile struct {
	Version  int      `json:"version"`
	PageSize int      `json:"page_size"`
	WindowMs int      `json:"window_ms"`
	Targets  []Target `json:"targets"`
}

// Parse unmarshals the JSON profile and performs basic validation.
func Parse(data []byte, expectedMemorySize uint64) (*MemoryProfile, error) {
	if len(data) > MaxProfileBytes {
		return nil, fmt.Errorf("profile exceeds %d bytes", MaxProfileBytes)
	}
	var prof MemoryProfile
	if err := json.Unmarshal(data, &prof); err != nil {
		return nil, fmt.Errorf("failed to unmarshal profile: %w", err)
	}

	if prof.Version != CurrentVersion {
		return nil, fmt.Errorf("unsupported profile version: %d", prof.Version)
	}
	if prof.PageSize != PageSize {
		return nil, fmt.Errorf("unsupported page size: %d", prof.PageSize)
	}

	var memoryTarget *Target
	for i := range prof.Targets {
		if prof.Targets[i].Kind == "memory" {
			if memoryTarget != nil {
				return nil, fmt.Errorf("multiple memory targets found in profile")
			}
			memoryTarget = &prof.Targets[i]
		}
	}

	if memoryTarget == nil {
		return nil, fmt.Errorf("no memory target found in profile")
	}

	if memoryTarget.Size != expectedMemorySize {
		return nil, fmt.Errorf("profile memory size (%d) does not match expected size (%d)", memoryTarget.Size, expectedMemorySize)
	}

	if len(memoryTarget.Ranges) > MaxRanges {
		return nil, fmt.Errorf("profile has too many ranges: %d", len(memoryTarget.Ranges))
	}
	// Validate ranges before concurrent workers see them.
	for _, r := range memoryTarget.Ranges {
		if r.Length == 0 {
			return nil, fmt.Errorf("zero-length range at %d", r.Offset)
		}
		if r.Offset%PageSize != 0 {
			return nil, fmt.Errorf("range offset %d is not page-aligned", r.Offset)
		}
		if r.Length%PageSize != 0 {
			return nil, fmt.Errorf("range length %d is not page-aligned", r.Length)
		}
		if r.Offset > memoryTarget.Size || r.Length > memoryTarget.Size-r.Offset {
			return nil, fmt.Errorf("range exceeds memory size")
		}
	}

	// Check overlap on an address-sorted copy, but retain the original order.
	// Adjacent ranges may intentionally encode reverse/interleaved Guest access;
	// merging them here would destroy the priority captured by Recorder.
	byAddress := append([]Range(nil), memoryTarget.Ranges...)
	sort.Slice(byAddress, func(i, j int) bool { return byAddress[i].Offset < byAddress[j].Offset })
	for i := 1; i < len(byAddress); i++ {
		previousEnd := byAddress[i-1].Offset + byAddress[i-1].Length
		if previousEnd > byAddress[i].Offset {
			return nil, fmt.Errorf("overlapping memory ranges at %d", byAddress[i].Offset)
		}
	}

	return &prof, nil
}

func (p *MemoryProfile) MemoryTarget() *Target {
	if p == nil {
		return nil
	}
	for i := range p.Targets {
		if p.Targets[i].Kind == "memory" {
			return &p.Targets[i]
		}
	}
	return nil
}
