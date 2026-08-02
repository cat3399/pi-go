//go:build windows

package session

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWindowsFailedIdentityLockClosesHandle(t *testing.T) {
	closed := 0
	_, err := claimWindowsWriterHandle(syscall.InvalidHandle, func() error {
		closed++
		return nil
	})
	if err == nil || closed != 1 {
		t.Fatalf("failed identity lock = error %v, close calls %d", err, closed)
	}
	if !errors.Is(err, syscall.Errno(6)) { // ERROR_INVALID_HANDLE
		t.Fatalf("failed identity lock cause = %v", err)
	}
}

func TestWindowsFileRenameInfoExReplacesOpenDestinationUnderIdentityLock(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target.jsonl")
	temporary := filepath.Join(directory, "temporary.jsonl")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temporary, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	targetPointer, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		t.Fatal(err)
	}
	identity := sessionWindowsIdentityHandleSpec()
	oldHandle, err := syscall.CreateFile(
		targetPointer,
		identity.DesiredAccess,
		identity.ShareMode,
		nil,
		identity.CreationDisposition,
		identity.FlagsAndAttributes,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(oldHandle)
	unlock, err := claimProcessIdentityWriter(target)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	replaced, err := replaceTemporary(temporary, target)
	if err != nil || !replaced {
		t.Fatalf("replace under identity lock = (%t, %v)", replaced, err)
	}
	data, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(data, []byte("new")) {
		t.Fatalf("replacement target = %q, %v", data, err)
	}
	oldData := make([]byte, 3)
	var oldRead uint32
	if err := syscall.ReadFile(oldHandle, oldData, &oldRead, nil); err != nil {
		t.Fatalf("read replaced destination handle: %v", err)
	}
	if !bytes.Equal(oldData[:oldRead], []byte("old")) {
		t.Fatalf("open destination handle changed identity: %q", oldData[:oldRead])
	}
	if _, err := os.Stat(temporary); !os.IsNotExist(err) {
		t.Fatalf("replacement temporary still exists: %v", err)
	}
}

func TestWindowsReplacementRejectsNonSiblingTemporaryBeforePublication(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target.jsonl")
	temporary := filepath.Join(t.TempDir(), "temporary.jsonl")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temporary, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	replaced, err := replaceTemporary(temporary, target)
	if err == nil || replaced {
		t.Fatalf("non-sibling replacement = (%t, %v)", replaced, err)
	}
	for path, want := range map[string][]byte{target: []byte("old"), temporary: []byte("new")} {
		data, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(data, want) {
			t.Fatalf("non-sibling replacement changed %s: %q, %v", path, data, readErr)
		}
	}
}

func TestWindowsOpenMigratesLegacyWhileIdentityLockIsHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.jsonl")
	legacy := []byte(`{"type":"session","id":"old","timestamp":"2026-08-01T00:00:00Z","cwd":"C:\\workspace"}` + "\n" +
		`{"type":"message","timestamp":"2026-08-01T00:00:01Z","message":{"role":"user","content":"hello","timestamp":1}}` + "\n")
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := Open(path, OpenOptions{NewEntryID: sequenceIDs("root")})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(data, []byte(`"version":3`)) {
		t.Fatalf("migrated Windows session = %q, %v", data, err)
	}
}

func TestWindowsRecoveryReplacesPartialWhileIdentityLockIsHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial.jsonl")
	prefix := []byte(testHeader + "\n" + userEntryJSON("root", "entry-1", "null", 1) + "\n")
	original := append(append([]byte(nil), prefix...), '{')
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := RecoverTrailingPartial(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, prefix) {
		t.Fatalf("recovered Windows session = %q, %v", data, err)
	}
	backup, err := os.ReadFile(result.BackupPath)
	if err != nil || !bytes.Equal(backup, original) {
		t.Fatalf("Windows recovery backup = %q, %v", backup, err)
	}
}
