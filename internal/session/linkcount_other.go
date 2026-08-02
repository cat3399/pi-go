//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !illumos && !ios && !linux && !netbsd && !openbsd && !solaris && !windows

package session

import "fmt"

func sessionLinkCount(string) (uint64, error) {
	return 0, fmt.Errorf("platform does not expose hard-link count")
}
