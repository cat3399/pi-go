package session

import (
	"encoding/binary"
	"fmt"
)

// Keep the Windows handle contract in platform-neutral code so every build can
// assert the exact CreateFileW arguments required for atomic replacement while
// an identity lock is held.
const (
	windowsGenericRead          uint32 = 0x80000000
	windowsGenericWrite         uint32 = 0x40000000
	windowsDeleteAccess         uint32 = 0x00010000
	windowsFileShareRead        uint32 = 0x00000001
	windowsFileShareWrite       uint32 = 0x00000002
	windowsFileShareDelete      uint32 = 0x00000004
	windowsOpenExisting         uint32 = 3
	windowsFileAttributeNormal  uint32 = 0x00000080
	windowsFileFlagWriteThrough uint32 = 0x80000000
	windowsFileRenameInfoEx     uint32 = 22
	windowsFileRenameReplace    uint32 = 0x00000001
	windowsFileRenamePOSIX      uint32 = 0x00000002
	windowsMoveFileReplace      uint32 = 0x00000001
	windowsMoveFileCopyAllowed  uint32 = 0x00000002
	windowsMoveFileWrite        uint32 = 0x00000008
	windowsMaximumPathCodeUnits        = 32767
	// Windows byte-range locks are mandatory even for another handle in the
	// locking process. Keep the coordination byte far beyond any realistic
	// session or supported Windows filesystem, while remaining below MaxInt64.
	windowsIdentityLockOffset uint64 = 1 << 62
	windowsIdentityLockLength uint64 = 1
)

type windowsIdentityHandleSpec struct {
	DesiredAccess       uint32
	ShareMode           uint32
	CreationDisposition uint32
	FlagsAndAttributes  uint32
}

type windowsReplacementHandleSpec struct {
	DesiredAccess       uint32
	ShareMode           uint32
	CreationDisposition uint32
	FlagsAndAttributes  uint32
}

type windowsRenameInfoLayout struct {
	rootDirectoryOffset  int
	fileNameLengthOffset int
	fileNameOffset       int
	minimumSize          int
}

type windowsIdentityLockRange struct {
	OffsetLow  uint32
	OffsetHigh uint32
	LengthLow  uint32
	LengthHigh uint32
}

func sessionWindowsCreatePublishFlags() uint32 {
	// Create publication is a same-directory move to a name that must not
	// exist. Omitting both REPLACE_EXISTING and COPY_ALLOWED preserves those
	// properties; WRITE_THROUGH is the durability request.
	return windowsMoveFileWrite
}

func sessionWindowsReplacementHandleSpec() windowsReplacementHandleSpec {
	return windowsReplacementHandleSpec{
		// DELETE is required to rename the temporary. GENERIC_WRITE is required
		// for the mandatory post-rename FlushFileBuffers call.
		DesiredAccess:       windowsGenericRead | windowsGenericWrite | windowsDeleteAccess,
		ShareMode:           windowsFileShareRead | windowsFileShareWrite | windowsFileShareDelete,
		CreationDisposition: windowsOpenExisting,
		FlagsAndAttributes:  windowsFileAttributeNormal | windowsFileFlagWriteThrough,
	}
}

func sessionWindowsRenameFlags() uint32 {
	// POSIX semantics is what permits replacement while the old target's
	// identity-lock handle remains open. There is deliberately no fallback to
	// FileRenameInfo or MoveFileExW, neither of which has that property.
	return windowsFileRenameReplace | windowsFileRenamePOSIX
}

func sessionWindowsIdentityHandleSpec() windowsIdentityHandleSpec {
	return windowsIdentityHandleSpec{
		DesiredAccess:       windowsGenericRead,
		ShareMode:           windowsFileShareRead | windowsFileShareWrite | windowsFileShareDelete,
		CreationDisposition: windowsOpenExisting,
		FlagsAndAttributes:  windowsFileAttributeNormal,
	}
}

func sessionWindowsIdentityLockRange() windowsIdentityLockRange {
	return windowsIdentityLockRange{
		OffsetLow:  uint32(windowsIdentityLockOffset & 0xffffffff),
		OffsetHigh: uint32(windowsIdentityLockOffset >> 32),
		LengthLow:  uint32(windowsIdentityLockLength & 0xffffffff),
		LengthHigh: uint32(windowsIdentityLockLength >> 32),
	}
}

func windowsRenameLayout(pointerSize int) (windowsRenameInfoLayout, error) {
	switch pointerSize {
	case 4:
		return windowsRenameInfoLayout{
			rootDirectoryOffset:  4,
			fileNameLengthOffset: 8,
			fileNameOffset:       12,
			minimumSize:          16,
		}, nil
	case 8:
		return windowsRenameInfoLayout{
			rootDirectoryOffset:  8,
			fileNameLengthOffset: 16,
			fileNameOffset:       20,
			minimumSize:          24,
		}, nil
	default:
		return windowsRenameInfoLayout{}, fmt.Errorf("unsupported Windows pointer size %d", pointerSize)
	}
}

// buildWindowsRenameInfoEx builds the ABI buffer consumed by
// SetFileInformationByHandle(FileRenameInfoEx). filename must not contain the
// terminating NUL; FileNameLength, rather than termination, defines its size.
func buildWindowsRenameInfoEx(filename []uint16, pointerSize int) ([]byte, error) {
	if len(filename) == 0 {
		return nil, fmt.Errorf("Windows replacement target is empty")
	}
	layout, err := windowsRenameLayout(pointerSize)
	if err != nil {
		return nil, err
	}
	if len(filename) > windowsMaximumPathCodeUnits {
		return nil, fmt.Errorf("Windows replacement target is too long")
	}
	// Allocate the native minimum structure size plus the complete variable
	// name. This intentionally over-allocates the WCHAR already represented by
	// FILE_RENAME_INFO.FileName[1], matching Microsoft's documented sizing
	// convention and keeping both 32-bit and 64-bit layouts safely padded.
	buffer := make([]byte, layout.minimumSize+len(filename)*2)
	binary.LittleEndian.PutUint32(buffer[0:4], sessionWindowsRenameFlags())
	// RootDirectory remains the zero value because filename is absolute.
	binary.LittleEndian.PutUint32(buffer[layout.fileNameLengthOffset:layout.fileNameLengthOffset+4], uint32(len(filename)*2))
	for index, value := range filename {
		offset := layout.fileNameOffset + index*2
		binary.LittleEndian.PutUint16(buffer[offset:offset+2], value)
	}
	return buffer, nil
}

func isWindowsAtomicRenameUnsupportedCode(code uint32) bool {
	// ERROR_INVALID_FUNCTION, ERROR_NOT_SUPPORTED, ERROR_INVALID_PARAMETER and
	// ERROR_CALL_NOT_IMPLEMENTED are the documented/observed capability
	// failures for an unavailable information class or unsupported POSIX flag.
	switch code {
	case 1, 50, 87, 120:
		return true
	default:
		return false
	}
}
