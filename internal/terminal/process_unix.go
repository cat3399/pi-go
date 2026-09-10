//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package terminal

import (
	"errors"
	"os"
	"time"

	pty "github.com/aymanbagabas/go-pty"
	"golang.org/x/sys/unix"
)

// Register a nonblocking duplicate with Go's poller. The PTY library creates
// the master in blocking mode; merely setting O_NONBLOCK afterwards cannot
// make an existing os.File pollable, and Read would return EAGAIN.
func preparePTY(terminal pty.Pty) (pty.Pty, error) {
	p := terminal.(pty.UnixPty)
	var duplicate int
	var duplicateErr error
	err := p.Control(func(fd uintptr) { duplicate, duplicateErr = unix.Dup(int(fd)) })
	if err != nil {
		return nil, err
	}
	if duplicateErr != nil {
		return nil, duplicateErr
	}
	unix.CloseOnExec(duplicate)
	if err := unix.SetNonblock(duplicate, true); err != nil {
		_ = unix.Close(duplicate)
		return nil, err
	}
	return &pollablePTY{UnixPty: p, file: os.NewFile(uintptr(duplicate), p.Name())}, nil
}

type pollablePTY struct {
	pty.UnixPty
	file *os.File
}

func (p *pollablePTY) Read(buf []byte) (int, error)  { return p.file.Read(buf) }
func (p *pollablePTY) Write(buf []byte) (int, error) { return p.file.Write(buf) }
func (p *pollablePTY) Close() error                  { return errors.Join(p.file.Close(), p.UnixPty.Close()) }

func terminate(terminal pty.Pty, process *os.Process) {
	// Interactive job control gives foreground jobs their own process group.
	// Terminating only the shell's group would leave the active job alive.
	if p, ok := terminal.(pty.UnixPty); ok {
		_ = p.Control(func(fd uintptr) {
			if foreground, err := unix.IoctlGetInt(int(fd), unix.TIOCGPGRP); err == nil && foreground > 0 && foreground != process.Pid {
				_ = unix.Kill(-foreground, unix.SIGKILL)
			}
		})
	}
	_ = unix.Kill(-process.Pid, unix.SIGHUP)
	// Give an interactive shell a bounded opportunity to forward HUP to its
	// background jobs before forcibly ending the shell's remaining group.
	time.Sleep(100 * time.Millisecond)
	_ = unix.Kill(-process.Pid, unix.SIGKILL)
}
