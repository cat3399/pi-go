//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package tool

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

func openRegularReadFile(path string) (*os.File, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: read only supports regular files (%s)", ErrUnsupportedFilesystemFeature, info.Mode().Type())
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	syscall.CloseOnExec(fd)
	file := os.NewFile(uintptr(fd), path)
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		_ = file.Close()
		return nil, fmt.Errorf("%w: read target changed or is not a regular file", fs.ErrInvalid)
	}
	return file, nil
}
