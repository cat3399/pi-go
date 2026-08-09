//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package tool

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestGrepSkipsFIFOWithoutWaitingForWriter(t *testing.T) {
	suite := newTestSuite(t)
	path := filepath.Join(suite.WorkingDir(), "pending.fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	result, err := suite.Grep(ctx, GrepInput{Pattern: "needle", Path: textPointer("pending.fifo")})
	if err != nil || result.Text != "No matches found" {
		t.Fatalf("FIFO grep = %#v, %v", result, err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("FIFO grep blocked for %s", elapsed)
	}
}

func TestGrepSkipsUnboundedDevice(t *testing.T) {
	suite := newTestSuite(t)
	result, err := suite.Grep(context.Background(), GrepInput{Pattern: "needle", Path: textPointer("/dev/zero")})
	if err != nil || result.Text != "No matches found" {
		t.Fatalf("device grep = %#v, %v", result, err)
	}
}

func TestGrepRejectsFIFOIgnoreFileWithoutWaitingForWriter(t *testing.T) {
	suite := newTestSuite(t)
	path := filepath.Join(suite.WorkingDir(), ".rgignore")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	_, err := suite.Grep(ctx, GrepInput{Pattern: "needle"})
	if err == nil {
		t.Fatal("FIFO ignore file unexpectedly accepted")
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("FIFO ignore file blocked grep for %s", elapsed)
	}
}
