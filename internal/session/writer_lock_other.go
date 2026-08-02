//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !illumos && !ios && !linux && !netbsd && !openbsd && !solaris && !windows

package session

import (
	"errors"
	"os"
)

func claimProcessPathWriter(string) (func(), error)     { return func() {}, nil }
func claimProcessIdentityWriter(string) (func(), error) { return nil, nil }
func claimProcessIdentityWriterWithInfo(string) (func(), os.FileInfo, error) {
	return nil, nil, errors.New("session identity locking unsupported")
}
