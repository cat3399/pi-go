//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package app

import (
	"os"
	"syscall"
)

func platformSignalSpecs() []signalSpec {
	return []signalSpec{
		{signal: os.Interrupt, exitCode: 130},
		{signal: syscall.SIGTERM, exitCode: 143},
		{signal: syscall.SIGHUP, exitCode: 129},
	}
}
