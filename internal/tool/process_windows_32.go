//go:build windows && (386 || arm)

package tool

// jobObjectBasicLimit mirrors JOBOBJECT_BASIC_LIMIT_INFORMATION. The trailing
// padding is part of the Windows 32-bit ABI: the structure is padded to an
// 8-byte boundary before it is embedded in JOBOBJECT_EXTENDED_LIMIT_INFORMATION.
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
	_                       uint32
}

const (
	expectedJobObjectBasicLimitSize    = uintptr(48)
	expectedJobObjectExtendedLimitSize = uintptr(112)
)
