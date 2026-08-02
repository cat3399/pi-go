//go:build !windows

package tool

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLookPathUsesInjectedEnvironmentAndWorkingDirectory(t *testing.T) {
	t.Parallel()
	workingDir := t.TempDir()
	binDir := filepath.Join(workingDir, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	shell := filepath.Join(binDir, "bash")
	if err := os.WriteFile(shell, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	path, ok := lookPathInEnvironment(
		"bash",
		[]string{"PATH=/definitely/missing", "PATH=bin"},
		workingDir,
		false,
	)
	if !ok || path != shell {
		t.Fatalf("lookPathInEnvironment() = (%q, %v), want (%q, true)", path, ok, shell)
	}
	if value, ok := environmentValue(
		[]string{"PATH=first", "=C:=hidden", "PATH=last"},
		"PATH",
		false,
	); !ok || value != "last" {
		t.Fatalf("environmentValue() = (%q, %v), want last", value, ok)
	}
}

func TestExplicitShellPathExistsWithoutPrematureExecutableCheck(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "shell")
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := resolveShell(path, newRunRequest("x", t.TempDir(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if config.path != path || len(config.arguments) != 1 || config.arguments[0] != "-c" {
		t.Fatalf("config = %#v", config)
	}
}
