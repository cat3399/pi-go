package session

import "testing"

func TestWindowsIdentityAndReplacementContractOnEveryPlatform(t *testing.T) {
	t.Parallel()
	spec := sessionWindowsIdentityHandleSpec()
	if spec.DesiredAccess != windowsGenericRead {
		t.Fatalf("Windows identity desired access = %#x", spec.DesiredAccess)
	}
	wantShare := windowsFileShareRead | windowsFileShareWrite | windowsFileShareDelete
	if spec.ShareMode != wantShare || spec.ShareMode&windowsFileShareDelete == 0 {
		t.Fatalf("Windows identity share mode = %#x, want %#x", spec.ShareMode, wantShare)
	}
	if spec.CreationDisposition != windowsOpenExisting || spec.FlagsAndAttributes != windowsFileAttributeNormal {
		t.Fatalf("Windows identity open contract = disposition %#x, flags %#x", spec.CreationDisposition, spec.FlagsAndAttributes)
	}
	flags := sessionWindowsReplacementFlags()
	if flags&windowsMoveFileReplace == 0 || flags&windowsMoveFileWrite == 0 {
		t.Fatalf("Windows replacement flags = %#x, require replace + write-through", flags)
	}
	if flags&windowsMoveFileCopyAllowed != 0 {
		t.Fatalf("Windows replacement flags = %#x, cross-volume copy must be refused", flags)
	}
}
