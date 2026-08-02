package app_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

const (
	helperEnabledEnv         = "PI_GO_APP_TEST_HELPER"
	helperScenarioEnv        = "PI_GO_APP_TEST_SCENARIO"
	helperWorkDirEnv         = "PI_GO_APP_TEST_WORKDIR"
	helperMarkerEnv          = "PI_GO_APP_TEST_MARKER"
	repeatCancelObservedName = "cancel-observed"
	repeatCancelReleaseName  = "cancel-release"
)

func TestProcessHelper(t *testing.T) {
	if os.Getenv(helperEnabledEnv) != "1" {
		return
	}
	workingDir := os.Getenv(helperWorkDirEnv)
	if workingDir == "" {
		os.Exit(97)
	}
	scenario := os.Getenv(helperScenarioEnv)
	runner := &scriptedRunner{output: []byte("process-tool-output\n")}
	var providerImpl *provider.ScriptedProvider
	switch scenario {
	case "workflow":
		providerImpl = newScriptedProvider(
			t,
			fixedStep(t, toolTerminal(t, "call-process", "bash", []byte(`{"command":"ignored"}`))),
			fixedStep(t, textTerminal(t, "process final")),
		)
	case "resume":
		providerImpl = newScriptedProvider(t, factoryStep(t, func(
			_ context.Context,
			request provider.Request,
			call uint64,
		) (llm.AssistantTerminal, error) {
			messages := request.Messages()
			if call != 1 || len(messages) != 5 || messages[4].Role() != llm.RoleUser {
				return nil, fmt.Errorf("resume request = call %d, messages %d", call, len(messages))
			}
			return textTerminal(t, "resumed final"), nil
		}))
	case "provider-error":
		providerImpl = newScriptedProvider(t)
	case "signal", "signal-repeat":
		providerImpl = newScriptedProvider(
			t,
			fixedStep(t, toolTerminal(t, "call-signal", "bash", []byte(`{"command":"wait"}`))),
			fixedStep(t, textTerminal(t, "must not run")),
		)
		runner.waitCancel = true
		runner.markerPath = os.Getenv(helperMarkerEnv)
		if scenario == "signal-repeat" {
			runner.cancelObservedPath = filepath.Join(workingDir, repeatCancelObservedName)
			runner.cancelReleasePath = filepath.Join(workingDir, repeatCancelReleaseName)
		}
	default:
		os.Exit(98)
	}
	deps := testDependencies(t, workingDir, providerImpl, runner)
	applicationArgs := helperApplicationArgs(os.Args)
	exitCode := app.Run(context.Background(), deps, applicationArgs, os.Stdout, os.Stderr)
	if scenario == "signal" || scenario == "signal-repeat" {
		sessionPath := argumentValue(applicationArgs, "--session")
		reopened, err := session.Open(sessionPath, session.OpenOptions{})
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "signal session reopen after Run: %v\n", err)
			os.Exit(96)
		}
		if err := reopened.Close(); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "signal session close after reopen: %v\n", err)
			os.Exit(95)
		}
	}
	os.Exit(exitCode)
}

func TestProcessWorkflowRestartsAndResumes(t *testing.T) {
	workingDir := t.TempDir()
	sessionPath := filepath.Join(workingDir, "process-session.jsonl")

	first := runHelperProcess(t, workingDir, "workflow", []string{
		"-p", "first prompt", "--session", sessionPath,
	}, "")
	if first.exitCode != 0 || first.stdout != "process final\n" || first.stderr != "" {
		t.Fatalf("first process = exit %d, stdout %q, stderr %q", first.exitCode, first.stdout, first.stderr)
	}

	second := runHelperProcess(t, workingDir, "resume", []string{
		"--session", sessionPath, "-p", "second prompt",
	}, "")
	if second.exitCode != 0 || second.stdout != "resumed final\n" || second.stderr != "" {
		t.Fatalf("resume process = exit %d, stdout %q, stderr %q", second.exitCode, second.stdout, second.stderr)
	}

	reopened, err := session.Open(sessionPath, session.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	messages := reopened.Context().Messages()
	_ = reopened.Close()
	if roles := messageRoles(messages); !reflectRolesEqual(roles, []llm.Role{
		llm.RoleUser,
		llm.RoleAssistant,
		llm.RoleToolResult,
		llm.RoleAssistant,
		llm.RoleUser,
		llm.RoleAssistant,
	}) {
		t.Fatalf("resumed durable roles = %v", roles)
	}
}

func TestProcessFailureAndInvalidSessionKeepOutputChannelsSeparated(t *testing.T) {
	workingDir := t.TempDir()
	failurePath := filepath.Join(workingDir, "provider-failure.jsonl")
	failure := runHelperProcess(t, workingDir, "provider-error", []string{
		"-p", "fail", "--session", failurePath,
	}, "")
	if failure.exitCode != 1 || failure.stdout != "" || failure.stderr != provider.ErrQueueExhausted.Error()+"\n" {
		t.Fatalf("failure process = exit %d, stdout %q, stderr %q", failure.exitCode, failure.stdout, failure.stderr)
	}

	invalidPath := filepath.Join(workingDir, "invalid.jsonl")
	original := []byte("{\"not\":\"a session\"}\n")
	if err := os.WriteFile(invalidPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	invalid := runHelperProcess(t, workingDir, "workflow", []string{
		"--session", invalidPath, "-p", "unused",
	}, "")
	if invalid.exitCode != 1 || invalid.stdout != "" || !strings.Contains(invalid.stderr, "open session "+invalidPath) {
		t.Fatalf("invalid process = exit %d, stdout %q, stderr %q", invalid.exitCode, invalid.stdout, invalid.stderr)
	}
	after, err := os.ReadFile(invalidPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("invalid process rewrote session: %q", after)
	}
}

type processResult struct {
	exitCode int
	stdout   string
	stderr   string
}

func newHelperCommand(
	t *testing.T,
	workingDir string,
	scenario string,
	args []string,
	marker string,
) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	commandArgs := []string{"-test.run=^TestProcessHelper$", "--"}
	commandArgs = append(commandArgs, args...)
	command := exec.Command(os.Args[0], commandArgs...)
	command.Dir = workingDir
	command.Env = append(os.Environ(),
		helperEnabledEnv+"=1",
		helperScenarioEnv+"="+scenario,
		helperWorkDirEnv+"="+workingDir,
		helperMarkerEnv+"="+marker,
	)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	return command, &stdout, &stderr
}

func runHelperProcess(
	t *testing.T,
	workingDir string,
	scenario string,
	args []string,
	marker string,
) processResult {
	t.Helper()
	command, stdout, stderr := newHelperCommand(t, workingDir, scenario, args, marker)
	err := command.Run()
	exitCode := processExitCode(err)
	if exitCode < 0 {
		t.Fatalf("helper process error = %v", err)
	}
	return processResult{exitCode: exitCode, stdout: stdout.String(), stderr: stderr.String()}
}

func helperApplicationArgs(args []string) []string {
	for index, arg := range args {
		if arg == "--" {
			return append([]string(nil), args[index+1:]...)
		}
	}
	return nil
}

func argumentValue(args []string, name string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == name {
			return args[index+1]
		}
	}
	return ""
}

func processExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode()
	}
	return -1
}

func reflectRolesEqual(left, right []llm.Role) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
