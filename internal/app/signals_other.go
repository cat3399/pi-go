//go:build plan9 || js || wasip1

package app

import "os"

func platformSignalSpecs() []signalSpec {
	return []signalSpec{{signal: os.Interrupt, exitCode: 130}}
}
