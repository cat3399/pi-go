//go:build windows

package tool

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

func platformCreatePrivateTempDirectory(parent, prefix string) (string, error) {
	if parent == "" {
		parent = os.TempDir()
	}
	for attempt := 0; attempt < 100; attempt++ {
		path, err := randomArtifactPath(parent, prefix, "")
		if err != nil {
			return "", err
		}
		if err := platformCreatePrivateDirectory(path); err == nil {
			return path, nil
		} else if !errors.Is(err, windows.ERROR_ALREADY_EXISTS) && !errors.Is(err, windows.ERROR_FILE_EXISTS) {
			return "", err
		}
	}
	return "", fmt.Errorf("could not allocate a unique artifact directory")
}

func platformCreatePrivateDirectory(path string) error {
	security, err := privateArtifactSecurityAttributes(true)
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	err = windows.CreateDirectory(name, security)
	runtime.KeepAlive(security)
	return err
}

func platformCreatePrivateTempFile(root, prefix, suffix string) (*os.File, error) {
	security, err := privateArtifactSecurityAttributes(false)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 100; attempt++ {
		path, err := randomArtifactPath(root, prefix, suffix)
		if err != nil {
			return nil, err
		}
		name, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return nil, err
		}
		handle, err := windows.CreateFile(
			name,
			uint32(windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL),
			windows.FILE_SHARE_READ,
			security,
			windows.CREATE_NEW,
			windows.FILE_ATTRIBUTE_NORMAL,
			0,
		)
		runtime.KeepAlive(security)
		if err == nil {
			return os.NewFile(uintptr(handle), path), nil
		}
		if !errors.Is(err, windows.ERROR_ALREADY_EXISTS) && !errors.Is(err, windows.ERROR_FILE_EXISTS) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("could not allocate a unique artifact file")
}

func randomArtifactPath(parent, prefix, suffix string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return filepath.Join(parent, prefix+hex.EncodeToString(random[:])+suffix), nil
}

func privateArtifactSecurityAttributes(directory bool) (*windows.SecurityAttributes, error) {
	owner, err := currentWindowsUserSID()
	if err != nil {
		return nil, err
	}
	flags := ""
	if directory {
		flags = "OICI"
	}
	descriptor, err := windows.SecurityDescriptorFromString(
		fmt.Sprintf("O:%sD:P(A;%s;FA;;;%s)", owner.String(), flags, owner.String()),
	)
	if err != nil {
		return nil, err
	}
	return &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}, nil
}

func currentWindowsUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid.Copy()
}

func validatePrivateArtifactDirectory(path string, info os.FileInfo) error {
	if !info.IsDir() {
		return fmt.Errorf("artifact directory is not a directory")
	}
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return err
	}
	return validateOwnerOnlyArtifactDescriptor(descriptor)
}

func validatePrivateArtifactFile(_ string, file *os.File, info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("artifact is not a regular file")
	}
	descriptor, err := windows.GetSecurityInfo(
		windows.Handle(file.Fd()),
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return err
	}
	return validateOwnerOnlyArtifactDescriptor(descriptor)
}

func validateOwnerOnlyArtifactDescriptor(descriptor *windows.SECURITY_DESCRIPTOR) error {
	owner, _, err := descriptor.Owner()
	if err != nil {
		return fmt.Errorf("read artifact owner: %w", err)
	}
	current, err := currentWindowsUserSID()
	if err != nil {
		return fmt.Errorf("read current owner: %w", err)
	}
	if owner == nil || !owner.Equals(current) {
		return fmt.Errorf("artifact owner is not the current user")
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return fmt.Errorf("read artifact ACL control: %w", err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("artifact DACL inherits permissions")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return fmt.Errorf("read artifact DACL: %w", err)
	}
	if dacl == nil || dacl.AceCount == 0 {
		return fmt.Errorf("artifact DACL is empty")
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return fmt.Errorf("read artifact ACE %d: %w", index, err)
		}
		if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return fmt.Errorf("artifact ACL contains a non-allow ACE")
		}
		if ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
			return fmt.Errorf("artifact ACL contains an inherited ACE")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() || !sid.Equals(current) {
			return fmt.Errorf("artifact ACL grants access outside the current user")
		}
	}
	return nil
}
