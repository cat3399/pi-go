//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package app_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/session"
)

func TestProcessSignalsSettleCancellationBeforeExit(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		signal   os.Signal
		exitCode int
	}{
		{name: "interrupt", signal: os.Interrupt, exitCode: 130},
		{name: "terminate", signal: syscall.SIGTERM, exitCode: 143},
		{name: "hangup", signal: syscall.SIGHUP, exitCode: 129},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			workingDir := t.TempDir()
			sessionPath := filepath.Join(workingDir, "signal.jsonl")
			markerPath := filepath.Join(workingDir, "tool-ready")
			command, stdout, stderr := newHelperCommand(t, workingDir, "signal", []string{
				"-p", "wait for signal", "--session", sessionPath,
			}, markerPath)
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			waitForFile(t, markerPath, command)
			if err := command.Process.Signal(testCase.signal); err != nil {
				_ = command.Process.Kill()
				_ = command.Wait()
				t.Fatalf("Signal() error = %v", err)
			}
			waited := make(chan error, 1)
			go func() { waited <- command.Wait() }()
			var waitErr error
			select {
			case waitErr = <-waited:
			case <-time.After(10 * time.Second):
				_ = command.Process.Kill()
				<-waited
				t.Fatal("signal helper did not settle")
			}
			if got := processExitCode(waitErr); got != testCase.exitCode {
				t.Fatalf("exit = %d, want %d; stderr %q", got, testCase.exitCode, stderr.String())
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("signal output = stdout %q, stderr %q", stdout.String(), stderr.String())
			}

			reopened, err := session.Open(sessionPath, session.OpenOptions{})
			if err != nil {
				t.Fatalf("signal session did not close: %v", err)
			}
			messages := reopened.Context().Messages()
			_ = reopened.Close()
			if roles := messageRoles(messages); !reflectRolesEqual(roles, []llm.Role{
				llm.RoleUser,
				llm.RoleAssistant,
				llm.RoleToolResult,
				llm.RoleAssistant,
			}) {
				t.Fatalf("signal durable roles = %v", roles)
			}
			result, ok := messages[2].(llm.ToolResultMessage)
			if !ok || !result.IsError() || textBlocks(result.Content()) != "Tool execution cancelled" {
				t.Fatalf("signal tool result = %T", messages[2])
			}
			failure, ok := messages[3].(llm.AssistantFailureMessage)
			if !ok || failure.FinishReason() != llm.FinishAborted || failure.ErrorMessage() != "Run cancelled during tool execution" {
				t.Fatalf("signal final = %T", messages[3])
			}
		})
	}
}

func TestProcessRepeatedSignalWaitsForFirstCancellationSettlement(t *testing.T) {
	workingDir := t.TempDir()
	sessionPath := filepath.Join(workingDir, "signal-repeat.jsonl")
	toolReadyPath := filepath.Join(workingDir, "tool-ready")
	cancelObservedPath := filepath.Join(workingDir, repeatCancelObservedName)
	cancelReleasePath := filepath.Join(workingDir, repeatCancelReleaseName)
	command, stdout, stderr := newHelperCommand(t, workingDir, "signal-repeat", []string{
		"-p", "wait for repeated signals", "--session", sessionPath,
	}, toolReadyPath)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, toolReadyPath, command)
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("first Signal() error = %v", err)
	}
	waitForFile(t, cancelObservedPath, command)
	if err := command.Process.Signal(os.Interrupt); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("second Signal() error = %v", err)
	}

	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	select {
	case err := <-waited:
		t.Fatalf("process exited before cancellation settlement was released: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := os.WriteFile(cancelReleasePath, []byte("release"), 0o600); err != nil {
		_ = command.Process.Kill()
		<-waited
		t.Fatalf("release cancellation settlement: %v", err)
	}

	var waitErr error
	select {
	case waitErr = <-waited:
	case <-time.After(10 * time.Second):
		_ = command.Process.Kill()
		<-waited
		t.Fatal("repeated-signal helper did not settle")
	}
	if got := processExitCode(waitErr); got != 143 {
		t.Fatalf("exit = %d, want first-signal code 143; stderr %q", got, stderr.String())
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("repeated signal output = stdout %q, stderr %q", stdout.String(), stderr.String())
	}

	reopened, err := session.Open(sessionPath, session.OpenOptions{})
	if err != nil {
		t.Fatalf("repeated signal session did not close: %v", err)
	}
	messages := reopened.Context().Messages()
	_ = reopened.Close()
	if roles := messageRoles(messages); !reflectRolesEqual(roles, []llm.Role{
		llm.RoleUser,
		llm.RoleAssistant,
		llm.RoleToolResult,
		llm.RoleAssistant,
	}) {
		t.Fatalf("repeated signal durable roles = %v", roles)
	}
	result, ok := messages[2].(llm.ToolResultMessage)
	if !ok || !result.IsError() || textBlocks(result.Content()) != "Tool execution cancelled" {
		t.Fatalf("repeated signal tool result = %T", messages[2])
	}
	failure, ok := messages[3].(llm.AssistantFailureMessage)
	if !ok || failure.FinishReason() != llm.FinishAborted || failure.ErrorMessage() != "Run cancelled during tool execution" {
		t.Fatalf("repeated signal final = %T", messages[3])
	}
}

func waitForFile(t *testing.T, path string, command *exec.Cmd) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("marker stat error = %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = command.Process.Kill()
	_ = command.Wait()
	t.Fatal("timed out waiting for tool marker")
}
