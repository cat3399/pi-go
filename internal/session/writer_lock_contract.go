package session

// Keep the Windows handle contract in platform-neutral code so every build can
// assert the exact CreateFileW arguments required for atomic replacement while
// an identity lock is held.
const (
	windowsGenericRead         uint32 = 0x80000000
	windowsFileShareRead       uint32 = 0x00000001
	windowsFileShareWrite      uint32 = 0x00000002
	windowsFileShareDelete     uint32 = 0x00000004
	windowsOpenExisting        uint32 = 3
	windowsFileAttributeNormal uint32 = 0x00000080
	windowsMoveFileReplace     uint32 = 0x00000001
	windowsMoveFileCopyAllowed uint32 = 0x00000002
	windowsMoveFileWrite       uint32 = 0x00000008
)

type windowsIdentityHandleSpec struct {
	DesiredAccess       uint32
	ShareMode           uint32
	CreationDisposition uint32
	FlagsAndAttributes  uint32
}

func sessionWindowsReplacementFlags() uint32 {
	// The temporary is always created beside the target. Omitting COPY_ALLOWED
	// makes a cross-volume request fail instead of degrading into copy+delete.
	return windowsMoveFileReplace | windowsMoveFileWrite
}

func sessionWindowsIdentityHandleSpec() windowsIdentityHandleSpec {
	return windowsIdentityHandleSpec{
		DesiredAccess:       windowsGenericRead,
		ShareMode:           windowsFileShareRead | windowsFileShareWrite | windowsFileShareDelete,
		CreationDisposition: windowsOpenExisting,
		FlagsAndAttributes:  windowsFileAttributeNormal,
	}
}
