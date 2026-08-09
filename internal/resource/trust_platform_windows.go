//go:build windows

package resource

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

func platformCreatePrivateTrustDirectory(path string) error {
	security, err := privateTrustSecurityAttributes(true)
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

func platformCreateExclusivePrivateTrustFile(path string) (*os.File, error) {
	security, err := privateTrustSecurityAttributes(false)
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
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

func platformCreatePrivateTrustTempFile(directory, prefix, suffix string) (*os.File, error) {
	for attempt := 0; attempt < 100; attempt++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, err
		}
		path := filepath.Join(directory, prefix+hex.EncodeToString(random[:])+suffix)
		file, err := platformCreateExclusivePrivateTrustFile(path)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, windows.ERROR_ALREADY_EXISTS) && !errors.Is(err, windows.ERROR_FILE_EXISTS) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("could not allocate a unique private trust file")
}

func privateTrustSecurityAttributes(directory bool) (*windows.SecurityAttributes, error) {
	owner, err := currentTrustWindowsUserSID()
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

func currentTrustWindowsUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid.Copy()
}

// Windows has no portable directory fsync equivalent. The pinned upstream
// writes trust.json without a directory flush; file Sync plus atomic replace
// is therefore the strongest portable contract here and no longer makes every
// Windows trust operation fail after a successful commit.
func syncDirectory(string) error { return nil }
