//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package session

import (
	"fmt"
	"os"
	"syscall"
)

func sessionLinkCount(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("filesystem did not expose link identity")
	}
	return uint64(stat.Nlink), nil
}
