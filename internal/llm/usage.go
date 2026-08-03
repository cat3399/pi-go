package llm

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
)

var (
	ErrUsageOverflow = errors.New("token usage overflow")
	ErrUsageSubset   = errors.New("invalid token usage subset")
)

// UsageSpec contains provider-normalized non-negative integer token counts.
// Nil optional fields mean that the provider did not report that breakdown.
type UsageSpec struct {
	Input        uint64
	Output       uint64
	CacheRead    uint64
	CacheWrite   uint64
	Reasoning    *uint64
	CacheWrite1h *uint64
	// Cost is optional because providers may report token accounting without
	// pricing. Nil means unknown, which is distinct from a real zero-priced
	// request. Rates and arithmetic belong to Model; this is the normalized
	// observed/request cost carried by messages and tool results.
	Cost *Cost
}

// Cost mirrors pi's usage.cost object.  It intentionally uses float64 rather
// than a decimal string because provider pricing is a calculated runtime
// value; callers must keep nil when the provider did not report/calculate it.
type Cost struct{ Input, Output, CacheRead, CacheWrite, Total float64 }

// Usage is immutable normalized token accounting. TotalTokens is always the
// checked sum of Input, Output, CacheRead, and CacheWrite.
type Usage struct {
	input           uint64
	output          uint64
	cacheRead       uint64
	cacheWrite      uint64
	totalTokens     uint64
	reasoning       uint64
	hasReasoning    bool
	cacheWrite1h    uint64
	hasCacheWrite1h bool
	cost            Cost
	hasCost         bool
}

func NewUsage(spec UsageSpec) (Usage, error) {
	if spec.Cost != nil {
		for _, value := range []float64{spec.Cost.Input, spec.Cost.Output, spec.Cost.CacheRead, spec.Cost.CacheWrite, spec.Cost.Total} {
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
				return Usage{}, fmt.Errorf("invalid usage cost")
			}
		}
	}
	if spec.Reasoning != nil && *spec.Reasoning > spec.Output {
		return Usage{}, fmt.Errorf(
			"%w: reasoning (%d) exceeds output (%d)",
			ErrUsageSubset,
			*spec.Reasoning,
			spec.Output,
		)
	}
	if spec.CacheWrite1h != nil && *spec.CacheWrite1h > spec.CacheWrite {
		return Usage{}, fmt.Errorf(
			"%w: cacheWrite1h (%d) exceeds cacheWrite (%d)",
			ErrUsageSubset,
			*spec.CacheWrite1h,
			spec.CacheWrite,
		)
	}

	total, carry := bits.Add64(spec.Input, spec.Output, 0)
	if carry != 0 {
		return Usage{}, ErrUsageOverflow
	}
	total, carry = bits.Add64(total, spec.CacheRead, 0)
	if carry != 0 {
		return Usage{}, ErrUsageOverflow
	}
	total, carry = bits.Add64(total, spec.CacheWrite, 0)
	if carry != 0 {
		return Usage{}, ErrUsageOverflow
	}

	usage := Usage{
		input:       spec.Input,
		output:      spec.Output,
		cacheRead:   spec.CacheRead,
		cacheWrite:  spec.CacheWrite,
		totalTokens: total,
	}
	if spec.Reasoning != nil {
		usage.reasoning = *spec.Reasoning
		usage.hasReasoning = true
	}
	if spec.CacheWrite1h != nil {
		usage.cacheWrite1h = *spec.CacheWrite1h
		usage.hasCacheWrite1h = true
	}
	if spec.Cost != nil {
		usage.cost = *spec.Cost
		usage.hasCost = true
	}

	return usage, nil
}

func (u Usage) Input() uint64 {
	return u.input
}

func (u Usage) Output() uint64 {
	return u.output
}

func (u Usage) CacheRead() uint64 {
	return u.cacheRead
}

func (u Usage) CacheWrite() uint64 {
	return u.cacheWrite
}

func (u Usage) TotalTokens() uint64 {
	return u.totalTokens
}

func (u Usage) Reasoning() (uint64, bool) {
	return u.reasoning, u.hasReasoning
}

func (u Usage) CacheWrite1h() (uint64, bool) {
	return u.cacheWrite1h, u.hasCacheWrite1h
}

func (u Usage) Cost() (Cost, bool) { return u.cost, u.hasCost }
