//go:build windows

package session

import (
	"os"
	"syscall"
)

func sessionLinkCount(path string) (uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	var information syscall.ByHandleFileInformation
	infoErr := syscall.GetFileInformationByHandle(syscall.Handle(file.Fd()), &information)
	closeErr := file.Close()
	if infoErr != nil {
		return 0, infoErr
	}
	if closeErr != nil {
		return 0, closeErr
	}
	return uint64(information.NumberOfLinks), nil
}
