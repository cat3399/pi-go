package llm_test

import (
	"errors"
	"math"
	"testing"

	"github.com/cat3399/pi-go/internal/llm"
)

func TestNewUsage(t *testing.T) {
	t.Parallel()

	reasoning := uint64(7)
	cacheWrite1h := uint64(3)
	usage, err := llm.NewUsage(llm.UsageSpec{
		Input:        11,
		Output:       13,
		CacheRead:    5,
		CacheWrite:   4,
		Reasoning:    &reasoning,
		CacheWrite1h: &cacheWrite1h,
	})
	if err != nil {
		t.Fatalf("NewUsage() error = %v", err)
	}

	if got := usage.Input(); got != 11 {
		t.Fatalf("Input() = %d, want 11", got)
	}
	if got := usage.Output(); got != 13 {
		t.Fatalf("Output() = %d, want 13", got)
	}
	if got := usage.CacheRead(); got != 5 {
		t.Fatalf("CacheRead() = %d, want 5", got)
	}
	if got := usage.CacheWrite(); got != 4 {
		t.Fatalf("CacheWrite() = %d, want 4", got)
	}
	if got := usage.TotalTokens(); got != 33 {
		t.Fatalf("TotalTokens() = %d, want 33", got)
	}
	if got, ok := usage.Reasoning(); !ok || got != 7 {
		t.Fatalf("Reasoning() = (%d, %t), want (7, true)", got, ok)
	}
	if got, ok := usage.CacheWrite1h(); !ok || got != 3 {
		t.Fatalf("CacheWrite1h() = (%d, %t), want (3, true)", got, ok)
	}

	// NewUsage copies optional values instead of retaining caller-owned pointers.
	reasoning = 0
	cacheWrite1h = 0
	if got, _ := usage.Reasoning(); got != 7 {
		t.Fatalf("Reasoning() changed through input pointer: got %d", got)
	}
	if got, _ := usage.CacheWrite1h(); got != 3 {
		t.Fatalf("CacheWrite1h() changed through input pointer: got %d", got)
	}
}

func TestNewUsageOptionalBreakdownAbsent(t *testing.T) {
	t.Parallel()

	usage, err := llm.NewUsage(llm.UsageSpec{})
	if err != nil {
		t.Fatalf("NewUsage() error = %v", err)
	}
	if got := usage.TotalTokens(); got != 0 {
		t.Fatalf("TotalTokens() = %d, want 0", got)
	}
	if got, ok := usage.Reasoning(); ok || got != 0 {
		t.Fatalf("Reasoning() = (%d, %t), want (0, false)", got, ok)
	}
	if got, ok := usage.CacheWrite1h(); ok || got != 0 {
		t.Fatalf("CacheWrite1h() = (%d, %t), want (0, false)", got, ok)
	}
}

func TestNewUsageRejectsInvalidSubset(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		spec llm.UsageSpec
	}{
		{
			name: "reasoning exceeds output",
			spec: llm.UsageSpec{Output: 2, Reasoning: uint64Pointer(3)},
		},
		{
			name: "one-hour cache exceeds cache write",
			spec: llm.UsageSpec{CacheWrite: 2, CacheWrite1h: uint64Pointer(3)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := llm.NewUsage(tt.spec)
			if !errors.Is(err, llm.ErrUsageSubset) {
				t.Fatalf("NewUsage() error = %v, want ErrUsageSubset", err)
			}
		})
	}
}

func TestNewUsageRejectsTotalOverflow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		spec llm.UsageSpec
	}{
		{name: "output", spec: llm.UsageSpec{Input: math.MaxUint64, Output: 1}},
		{name: "cache read", spec: llm.UsageSpec{Input: math.MaxUint64 - 1, Output: 1, CacheRead: 1}},
		{name: "cache write", spec: llm.UsageSpec{Input: math.MaxUint64 - 2, Output: 1, CacheRead: 1, CacheWrite: 1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := llm.NewUsage(tt.spec)
			if !errors.Is(err, llm.ErrUsageOverflow) {
				t.Fatalf("NewUsage() error = %v, want ErrUsageOverflow", err)
			}
		})
	}
}

func TestNewUsageAcceptsEqualAndPresentZeroSubsets(t *testing.T) {
	t.Parallel()

	zero := uint64(0)
	usage, err := llm.NewUsage(llm.UsageSpec{
		Output:       7,
		CacheWrite:   5,
		Reasoning:    uint64Pointer(7),
		CacheWrite1h: &zero,
	})
	if err != nil {
		t.Fatalf("NewUsage() error = %v", err)
	}
	if got, ok := usage.Reasoning(); !ok || got != 7 {
		t.Fatalf("Reasoning() = (%d, %t), want (7, true)", got, ok)
	}
	if got, ok := usage.CacheWrite1h(); !ok || got != 0 {
		t.Fatalf("CacheWrite1h() = (%d, %t), want (0, true)", got, ok)
	}
}

func uint64Pointer(value uint64) *uint64 {
	return &value
}
