//go:build windows && (amd64 || arm64)

package tool

// jobObjectBasicLimit mirrors JOBOBJECT_BASIC_LIMIT_INFORMATION on 64-bit
// Windows. Its natural field alignment already supplies the required padding.
type jobObjectBasicLimit struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

const (
	expectedJobObjectBasicLimitSize    = uintptr(64)
	expectedJobObjectExtendedLimitSize = uintptr(144)
)
