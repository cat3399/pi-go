//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package tool

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestAtomicWriteRejectsEffectivelyUnwritableOwnerMode0002(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may bypass owner-class permission checks")
	}
	suite := newTestSuite(t)
	target := writeTestFile(t, suite.WorkingDir(), "owner-mode.txt", "old")
	if err := os.Chmod(target, 0o002); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(target, 0o600) }()
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	_, err = suite.Write(context.Background(), WriteInput{Path: "owner-mode.txt", Content: "new"})
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("write error = %v, want effective permission failure", err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "old" {
		t.Fatalf("target changed to %q", data)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("failed write changed mtime from %v to %v", before.ModTime(), after.ModTime())
	}
}
