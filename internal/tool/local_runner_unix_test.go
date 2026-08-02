//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package tool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLocalBashExecutesInWorkingDirectoryWithEnvironment(t *testing.T) {
	t.Parallel()
	workingDir := t.TempDir()
	bash, err := NewBash(BashOptions{
		WorkingDir: workingDir,
		Environment: []string{
			"PATH=/usr/bin:/bin",
			"VISIBLE=present",
			"PI_SESSION_ID=must-not-leak",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	command := "printf 'OUT:%s\\n' \"$VISIBLE\"; printf 'ERR:%s\\n' \"$PI_SESSION_ID\" >&2; pwd"
	result, err := bash.Execute(context.Background(), testBashInput(t, command, nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"OUT:present", "ERR:", workingDir} {
		if !strings.Contains(result.Text(), fragment) {
			t.Fatalf("output %q does not contain %q", result.Text(), fragment)
		}
	}
	if strings.Contains(result.Text(), "must-not-leak") {
		t.Fatalf("output leaked stripped environment: %q", result.Text())
	}
}

func TestLocalBashShellSetupAndSpawnFailuresAreDistinct(t *testing.T) {
	t.Parallel()
	t.Run("missing explicit shell is setup failure", func(t *testing.T) {
		t.Parallel()
		bash, err := NewBash(BashOptions{
			WorkingDir:  t.TempDir(),
			Environment: []string{"PATH=/usr/bin:/bin"},
			ShellPath:   filepath.Join(t.TempDir(), "missing-shell"),
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = bash.Execute(context.Background(), testBashInput(t, "echo no", nil))
		var failure *BashFailure
		if !errors.As(err, &failure) || failure.Kind() != FailureSetup {
			t.Fatalf("error = %v, want setup failure", err)
		}
	})

	t.Run("existing non-executable shell is spawn failure", func(t *testing.T) {
		t.Parallel()
		shell := filepath.Join(t.TempDir(), "shell")
		if err := os.WriteFile(shell, []byte("#!/bin/sh\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		bash, err := NewBash(BashOptions{
			WorkingDir:  t.TempDir(),
			Environment: []string{"PATH=/usr/bin:/bin"},
			ShellPath:   shell,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = bash.Execute(context.Background(), testBashInput(t, "echo no", nil))
		var failure *BashFailure
		if !errors.As(err, &failure) || failure.Kind() != FailureSpawn {
			t.Fatalf("error = %v, want spawn failure", err)
		}
	})

	t.Run("relative explicit shell is resolved against the fixed working directory", func(t *testing.T) {
		t.Parallel()
		workingDir := t.TempDir()
		shell := filepath.Join(workingDir, "relative-shell")
		if err := os.WriteFile(shell, []byte("#!/bin/sh\nexec /bin/sh \"$@\"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		bash, err := NewBash(BashOptions{
			WorkingDir:  workingDir,
			Environment: []string{"PATH=/usr/bin:/bin"},
			ShellPath:   "relative-shell",
		})
		if err != nil {
			t.Fatal(err)
		}
		result, err := bash.Execute(context.Background(), testBashInput(t, "printf relative-ok", nil))
		if err != nil || result.Text() != "relative-ok" {
			t.Fatalf("relative shell result = (%q, %v)", result.Text(), err)
		}
	})
}

func TestLocalBashTimeoutKillsBackgroundProcessGroup(t *testing.T) {
	workingDir := t.TempDir()
	ready := filepath.Join(workingDir, "descendant-started")
	marker := filepath.Join(workingDir, "should-not-exist")
	timeout := 250 * time.Millisecond
	bash := newLocalTestBash(t, workingDir)
	command := "(touch " + shellQuote(ready) + "; sleep 0.6; touch " + shellQuote(marker) + ") & wait"
	_, err := bash.Execute(context.Background(), testBashInput(t, command, &timeout))
	var failure *BashFailure
	if !errors.As(err, &failure) || failure.Kind() != FailureTimeout {
		t.Fatalf("error = %v, want timeout", err)
	}
	if _, err := os.Stat(ready); err != nil {
		t.Fatalf("timeout fired before descendant startup was proven: %v", err)
	}
	time.Sleep(700 * time.Millisecond)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("background descendant survived timeout; stat error = %v", err)
	}
}

func TestLocalBashCancellationKillsBackgroundProcessGroup(t *testing.T) {
	workingDir := t.TempDir()
	ready := filepath.Join(workingDir, "descendant-started")
	marker := filepath.Join(workingDir, "should-not-exist")
	bash := newLocalTestBash(t, workingDir)
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("fixture cancellation")
	command := "(touch " + shellQuote(ready) + "; sleep 0.6; touch " + shellQuote(marker) + ") & wait"
	done := make(chan error, 1)
	go func() {
		_, err := bash.Execute(ctx, testBashInput(t, command, nil))
		done <- err
	}()
	waitForPath(t, ready, 2*time.Second)
	cancel(cause)
	err := <-done
	var failure *BashFailure
	if !errors.As(err, &failure) || failure.Kind() != FailureCancelled {
		t.Fatalf("error = %v, want cancellation", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("error does not retain cancellation cause: %v", err)
	}
	time.Sleep(700 * time.Millisecond)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("background descendant survived cancellation; stat error = %v", err)
	}
}

func TestLocalRunnerCancellationWinsExitAndIdleSettlement(t *testing.T) {
	tests := []struct {
		name    string
		command func(string, string) string
	}{
		{
			name: "direct exit and pipe EOF",
			command: func(_, _ string) string {
				return "printf READY"
			},
		},
		{
			name: "post-exit idle grace",
			command: func(ready, marker string) string {
				return "(touch " + shellQuote(ready) + "; sleep 0.4; touch " + shellQuote(marker) + ") & " +
					"while [ ! -f " + shellQuote(ready) + " ]; do sleep 0.01; done; printf READY"
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			workingDir := t.TempDir()
			ready := filepath.Join(workingDir, "descendant-ready")
			marker := filepath.Join(workingDir, "should-not-exist")
			ctx, cancel := context.WithCancelCause(context.Background())
			cause := errors.New("cancelled while settling")
			var cancelOnce sync.Once
			runner := NewLocalRunner(LocalRunnerOptions{})
			_, err := runner.Run(
				ctx,
				newRunRequest(test.command(ready, marker), workingDir, []string{"PATH=/usr/bin:/bin"}),
				func(data []byte) error {
					if strings.Contains(string(data), "READY") {
						cancelOnce.Do(func() { cancel(cause) })
					}
					return nil
				},
			)
			var interrupted *RunInterruptedError
			if !errors.As(err, &interrupted) || !errors.Is(err, cause) {
				t.Fatalf("Run() error = %v, want cancellation cause", err)
			}
			if test.name == "post-exit idle grace" {
				if _, err := os.Stat(ready); err != nil {
					t.Fatalf("background descendant never started: %v", err)
				}
				time.Sleep(500 * time.Millisecond)
				if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("background descendant survived cancellation race: %v", err)
				}
			}
		})
	}
}

func TestLocalBashArtifactFailureKillsAndReapsProcessGroup(t *testing.T) {
	workingDir := t.TempDir()
	marker := filepath.Join(workingDir, "should-not-exist")
	artifactRoot := filepath.Join(t.TempDir(), "broad")
	if err := os.Mkdir(artifactRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	bash, err := NewBash(BashOptions{
		WorkingDir:        workingDir,
		Environment:       []string{"PATH=/usr/bin:/bin"},
		ArtifactDirectory: artifactRoot,
		MaxOutputBytes:    4,
	})
	if err != nil {
		t.Fatal(err)
	}
	command := "printf 'abcdef'; (sleep 0.25; touch " + shellQuote(marker) + ") & wait"
	_, err = bash.Execute(context.Background(), testBashInput(t, command, nil))
	var failure *BashFailure
	if !errors.As(err, &failure) || failure.Kind() != FailureArtifact {
		t.Fatalf("error = %v, want artifact failure", err)
	}
	if !errors.Is(err, ErrArtifactSecurity) {
		t.Fatalf("artifact security cause was not retained: %v", err)
	}
	time.Sleep(350 * time.Millisecond)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("background descendant survived artifact failure; stat error = %v", err)
	}
}

func TestLocalRunnerPostExitIdleGrace(t *testing.T) {
	t.Run("active descendant extends capture", func(t *testing.T) {
		bash := newLocalTestBash(t, t.TempDir())
		command := "printf 'HEAD\\n'; (for i in 1 2 3 4 5 6; do sleep 0.05; printf 'TICK%s\\n' \"$i\"; done) &"
		start := time.Now()
		result, err := bash.Execute(context.Background(), testBashInput(t, command, nil))
		elapsed := time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(result.Text(), "HEAD") || !strings.Contains(result.Text(), "TICK6") {
			t.Fatalf("late active output was truncated: %q", result.Text())
		}
		if elapsed < 200*time.Millisecond || elapsed > 2*time.Second {
			t.Fatalf("active capture elapsed = %s", elapsed)
		}
	})

	t.Run("quiet descendant is left alive after reader settles", func(t *testing.T) {
		workingDir := t.TempDir()
		marker := filepath.Join(workingDir, "background-finished")
		bash := newLocalTestBash(t, workingDir)
		command := "printf 'DONE\\n'; (sleep 0.35; touch " + shellQuote(marker) + ") &"
		start := time.Now()
		result, err := bash.Execute(context.Background(), testBashInput(t, command, nil))
		elapsed := time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(result.Text(), "DONE") {
			t.Fatalf("output = %q", result.Text())
		}
		if elapsed > 300*time.Millisecond {
			t.Fatalf("runner waited too long for quiet descendant: %s", elapsed)
		}
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("marker already exists at settle; stat error = %v", err)
		}
		deadline := time.Now().Add(time.Second)
		for {
			if _, err := os.Stat(marker); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("quiet background child was killed instead of being left alive")
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
}

func newLocalTestBash(t *testing.T, workingDir string) *Bash {
	t.Helper()
	bash, err := NewBash(BashOptions{
		WorkingDir:  workingDir,
		Environment: []string{"PATH=/usr/bin:/bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return bash
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func waitForPath(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("inspect readiness path %s: %v", path, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for readiness path %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
