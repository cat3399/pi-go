//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !illumos && !ios && !linux && !netbsd && !openbsd && !solaris && !windows

package session

func claimProcessWriter(string) (func(), error) { return func() {}, nil }
