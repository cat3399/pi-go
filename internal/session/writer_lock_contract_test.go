package session

import (
	"encoding/binary"
	"slices"
	"testing"
	"unicode/utf16"
)

// This is intentionally an ABI/argument contract test, not evidence that the
// Windows runtime accepts FileRenameInfoEx. legacy_windows_test.go owns that
// runtime coverage and must execute on a Windows runner.
func TestWindowsIdentityAndReplacementContractOnly(t *testing.T) {
	t.Parallel()
	identity := sessionWindowsIdentityHandleSpec()
	if identity.DesiredAccess != windowsGenericRead {
		t.Fatalf("Windows identity desired access = %#x", identity.DesiredAccess)
	}
	wantShare := windowsFileShareRead | windowsFileShareWrite | windowsFileShareDelete
	if identity.ShareMode != wantShare || identity.ShareMode&windowsFileShareDelete == 0 {
		t.Fatalf("Windows identity share mode = %#x, want %#x", identity.ShareMode, wantShare)
	}
	if identity.CreationDisposition != windowsOpenExisting || identity.FlagsAndAttributes != windowsFileAttributeNormal {
		t.Fatalf("Windows identity open contract = disposition %#x, flags %#x", identity.CreationDisposition, identity.FlagsAndAttributes)
	}
	lockRange := sessionWindowsIdentityLockRange()
	gotOffset := uint64(lockRange.OffsetHigh)<<32 | uint64(lockRange.OffsetLow)
	gotLength := uint64(lockRange.LengthHigh)<<32 | uint64(lockRange.LengthLow)
	if gotOffset != windowsIdentityLockOffset || gotOffset < 1<<60 {
		t.Fatalf("Windows identity lock offset = %#x, want stable high offset %#x", gotOffset, windowsIdentityLockOffset)
	}
	if gotLength != 1 {
		t.Fatalf("Windows identity lock length = %d, want 1", gotLength)
	}

	replacement := sessionWindowsReplacementHandleSpec()
	wantAccess := windowsGenericRead | windowsGenericWrite | windowsDeleteAccess
	if replacement.DesiredAccess != wantAccess {
		t.Fatalf("Windows replacement desired access = %#x, want %#x", replacement.DesiredAccess, wantAccess)
	}
	if replacement.ShareMode != wantShare {
		t.Fatalf("Windows replacement share mode = %#x, want %#x", replacement.ShareMode, wantShare)
	}
	if replacement.CreationDisposition != windowsOpenExisting {
		t.Fatalf("Windows replacement disposition = %#x", replacement.CreationDisposition)
	}
	wantAttributes := windowsFileAttributeNormal | windowsFileFlagWriteThrough
	if replacement.FlagsAndAttributes != wantAttributes {
		t.Fatalf("Windows replacement attributes = %#x, want %#x", replacement.FlagsAndAttributes, wantAttributes)
	}
	if windowsFileRenameInfoEx != 22 {
		t.Fatalf("Windows replacement information class = %d", windowsFileRenameInfoEx)
	}
	renameFlags := sessionWindowsRenameFlags()
	if renameFlags != windowsFileRenameReplace|windowsFileRenamePOSIX {
		t.Fatalf("Windows rename flags = %#x, require replace + POSIX", renameFlags)
	}

	createFlags := sessionWindowsCreatePublishFlags()
	if createFlags&windowsMoveFileWrite == 0 {
		t.Fatalf("Windows create publication flags = %#x, require write-through", createFlags)
	}
	if createFlags&(windowsMoveFileReplace|windowsMoveFileCopyAllowed) != 0 {
		t.Fatalf("Windows create publication flags = %#x, clobber/copy must be refused", createFlags)
	}
}

func TestWindowsRenameInfoExBufferContractOnly(t *testing.T) {
	t.Parallel()
	name := utf16.Encode([]rune(`C:\sessions\会话.jsonl`))
	for _, pointerSize := range []int{4, 8} {
		pointerSize := pointerSize
		t.Run(string(rune('0'+pointerSize))+"-byte pointer", func(t *testing.T) {
			layout, err := windowsRenameLayout(pointerSize)
			if err != nil {
				t.Fatal(err)
			}
			buffer, err := buildWindowsRenameInfoEx(name, pointerSize)
			if err != nil {
				t.Fatal(err)
			}
			if got := binary.LittleEndian.Uint32(buffer[:4]); got != sessionWindowsRenameFlags() {
				t.Fatalf("rename flags = %#x", got)
			}
			for _, value := range buffer[layout.rootDirectoryOffset:layout.fileNameLengthOffset] {
				if value != 0 {
					t.Fatalf("RootDirectory is not NULL: %x", buffer[layout.rootDirectoryOffset:layout.fileNameLengthOffset])
				}
			}
			if got := binary.LittleEndian.Uint32(buffer[layout.fileNameLengthOffset:]); got != uint32(len(name)*2) {
				t.Fatalf("FileNameLength = %d, want %d", got, len(name)*2)
			}
			decoded := make([]uint16, len(name))
			for index := range decoded {
				offset := layout.fileNameOffset + index*2
				decoded[index] = binary.LittleEndian.Uint16(buffer[offset : offset+2])
			}
			if !slices.Equal(decoded, name) {
				t.Fatalf("FileName = %x, want %x", decoded, name)
			}
			if got, want := len(buffer), layout.minimumSize+len(name)*2; got != want {
				t.Fatalf("rename buffer size = %d, want %d", got, want)
			}
		})
	}
	if _, err := windowsRenameLayout(16); err == nil {
		t.Fatal("unsupported pointer size was accepted")
	}
	if _, err := buildWindowsRenameInfoEx(nil, 8); err == nil {
		t.Fatal("empty replacement name was accepted")
	}
	if _, err := buildWindowsRenameInfoEx(make([]uint16, windowsMaximumPathCodeUnits+1), 8); err == nil {
		t.Fatal("oversized replacement name was accepted")
	}
	for _, code := range []uint32{1, 50, 87, 120} {
		if !isWindowsAtomicRenameUnsupportedCode(code) {
			t.Fatalf("Windows capability error %d was not classified unsupported", code)
		}
	}
	if isWindowsAtomicRenameUnsupportedCode(5) {
		t.Fatal("ERROR_ACCESS_DENIED was misclassified as an unsupported runtime")
	}
}
