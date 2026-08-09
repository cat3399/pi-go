//go:build windows

package resource

import (
	"context"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsTrustStoreAndLeaseAreOwnerOnly(t *testing.T) {
	store, err := NewTrustStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(context.Background(), filepath.Join(t.TempDir(), "project"), true); err != nil {
		t.Fatal(err)
	}
	assertOwnerOnlyTrustPath(t, store.Path())

	lease, err := store.acquirePersistentLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertOwnerOnlyTrustPath(t, lease.path)
	assertOwnerOnlyTrustPath(t, filepath.Join(lease.path, trustLockOwnerFile))
	assertOwnerOnlyTrustPath(t, filepath.Join(lease.path, trustLockHeartbeatFile))
	if err := lease.release(); err != nil {
		t.Fatal(err)
	}
}

func assertOwnerOnlyTrustPath(t *testing.T, path string) {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		t.Fatal(err)
	}
	current, err := currentTrustWindowsUserSID()
	if err != nil {
		t.Fatal(err)
	}
	if owner == nil || !owner.Equals(current) {
		t.Fatalf("%s owner is not current user", path)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("%s inherits its DACL", path)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if dacl == nil || dacl.AceCount == 0 {
		t.Fatalf("%s has no explicit owner ACE", path)
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			t.Fatal(err)
		}
		if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
			t.Fatalf("%s contains a non-owner or inherited ACE", path)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() || !sid.Equals(current) {
			t.Fatalf("%s grants access outside current user", path)
		}
	}
}
