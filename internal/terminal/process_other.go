//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package terminal

import (
	pty "github.com/aymanbagabas/go-pty"
	"os"
)

func preparePTY(p pty.Pty) (pty.Pty, error) { return p, nil }

func terminate(_ pty.Pty, process *os.Process) { _ = process.Kill() }
