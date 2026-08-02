//go:build windows

package app

import (
	"os"
	"syscall"
)

func platformSignalSpecs() []signalSpec {
	return []signalSpec{
		{signal: os.Interrupt, exitCode: 130},
		{signal: syscall.SIGTERM, exitCode: 143},
	}
}
