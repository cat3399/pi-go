//go:build windows

package terminal

import (
	"os"

	pty "github.com/aymanbagabas/go-pty"
)

func preparePTY(p pty.Pty) (pty.Pty, error) { return p, nil }

func terminate(_ pty.Pty, process *os.Process) {
	_ = process.Kill()
	// Closing ConPTY terminates attached console processes. The service closes
	// it concurrently with its output pump, so its final flush cannot deadlock.
}
