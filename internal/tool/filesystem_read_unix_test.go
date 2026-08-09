//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package tool

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReadRejectsFIFOWithoutWaitingForAWriter(t *testing.T) {
	suite := newTestSuite(t)
	path := filepath.Join(suite.WorkingDir(), "pending.fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	_, err := suite.Read(ctx, ReadInput{Path: "pending.fifo"})
	if !errors.Is(err, ErrUnsupportedFilesystemFeature) {
		t.Fatalf("FIFO read error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("FIFO read blocked for %s", elapsed)
	}
}

func TestReadRejectsDeviceInsteadOfStreamingWithoutBound(t *testing.T) {
	suite := newTestSuite(t)
	if _, err := suite.Read(context.Background(), ReadInput{Path: "/dev/zero"}); !errors.Is(err, ErrUnsupportedFilesystemFeature) {
		t.Fatalf("device read error = %v", err)
	}
}
