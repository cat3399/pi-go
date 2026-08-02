//go:build windows

package tool

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
	"unsafe"
)

const windowsHelperRoleEnvironment = "PI_GO_TOOL_WINDOWS_HELPER_ROLE"

func TestWindowsJobObjectLimitLayout(t *testing.T) {
	if got := unsafe.Sizeof(jobObjectBasicLimit{}); got != expectedJobObjectBasicLimitSize {
		t.Fatalf("JOBOBJECT_BASIC_LIMIT_INFORMATION size = %d, want %d", got, expectedJobObjectBasicLimitSize)
	}
	if got := unsafe.Sizeof(jobObjectExtendedLimit{}); got != expectedJobObjectExtendedLimitSize {
		t.Fatalf("JOBOBJECT_EXTENDED_LIMIT_INFORMATION size = %d, want %d", got, expectedJobObjectExtendedLimitSize)
	}
}

func TestWindowsJobObjectOwnsDescendants(t *testing.T) {
	t.Run("termination waits for the whole job", func(t *testing.T) {
		workingDir := t.TempDir()
		ready := filepath.Join(workingDir, "child-started")
		marker := filepath.Join(workingDir, "must-not-exist")
		command := windowsHelperCommand("parent-wait", ready, marker)
		tree, err := startLocalProcess(command)
		if err != nil {
			t.Fatal(err)
		}
		waitForWindowsPath(t, ready, 5*time.Second)
		if err := tree.terminate(); err != nil {
			t.Fatal(err)
		}
		if err := command.Wait(); err == nil {
			t.Fatal("terminated helper unexpectedly exited successfully")
		}
		if err := tree.settleTermination(); err != nil {
			t.Fatal(err)
		}
		if err := tree.release(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(700 * time.Millisecond)
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("job descendant survived termination: %v", err)
		}
	})

	t.Run("normal release leaves a quiet descendant alive", func(t *testing.T) {
		workingDir := t.TempDir()
		ready := filepath.Join(workingDir, "child-started")
		marker := filepath.Join(workingDir, "child-finished")
		command := windowsHelperCommand("parent-exit", ready, marker)
		tree, err := startLocalProcess(command)
		if err != nil {
			t.Fatal(err)
		}
		waitForWindowsPath(t, ready, 5*time.Second)
		if err := command.Wait(); err != nil {
			t.Fatal(err)
		}
		if err := tree.release(); err != nil {
			t.Fatal(err)
		}
		waitForWindowsPath(t, marker, 5*time.Second)
	})
}

func windowsHelperCommand(role, ready, marker string) *exec.Cmd {
	command := exec.Command(os.Args[0], "-test.run=^TestWindowsProcessTreeHelper$")
	command.Env = append(
		os.Environ(),
		windowsHelperRoleEnvironment+"="+role,
		"PI_GO_TOOL_WINDOWS_HELPER_READY="+ready,
		"PI_GO_TOOL_WINDOWS_HELPER_MARKER="+marker,
	)
	return command
}

func TestWindowsProcessTreeHelper(t *testing.T) {
	role := os.Getenv(windowsHelperRoleEnvironment)
	if role == "" {
		t.Skip("subprocess helper")
	}
	marker := os.Getenv("PI_GO_TOOL_WINDOWS_HELPER_MARKER")
	if role == "child" {
		time.Sleep(500 * time.Millisecond)
		if err := os.WriteFile(marker, []byte("finished"), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}

	child := exec.Command(os.Args[0], "-test.run=^TestWindowsProcessTreeHelper$")
	child.Env = append(
		os.Environ(),
		windowsHelperRoleEnvironment+"=child",
		"PI_GO_TOOL_WINDOWS_HELPER_MARKER="+marker,
	)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("PI_GO_TOOL_WINDOWS_HELPER_READY"), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	if role == "parent-wait" {
		if err := child.Wait(); err != nil {
			t.Fatal(err)
		}
	}
}

func waitForWindowsPath(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("inspect readiness path %s: %v", path, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
