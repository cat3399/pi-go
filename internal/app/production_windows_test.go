//go:build windows

package app_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/app"
)

func TestRunProductionWindowsMissingAuthAllowsEphemeralConfiguredAndAmbientSources(t *testing.T) {
	testCases := []struct {
		name          string
		args          []string
		configuredKey *string
		environment   []string
		wantKey       string
	}{
		{
			name:          "CLI runtime override",
			args:          []string{"--model", "openai/windows-cli", "--api-key", "cli-key", "-p", "hello"},
			configuredKey: stringPointer("configured-lower"),
			environment:   []string{"OPENAI_API_KEY=ambient-lower"},
			wantKey:       "cli-key",
		},
		{
			name:          "configured models key",
			args:          []string{"--model", "openai/windows-configured", "-p", "hello"},
			configuredKey: stringPointer("configured-key"),
			environment:   []string{"OPENAI_API_KEY=ambient-lower"},
			wantKey:       "configured-key",
		},
		{
			name:        "ambient environment key",
			args:        []string{"--model", "openai/windows-ambient", "-p", "hello"},
			environment: []string{"OPENAI_API_KEY=ambient-key"},
			wantKey:     "ambient-key",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			workingDir := t.TempDir()
			agentDir := t.TempDir()
			capture := &capturedProductionRequest{}
			server := newProductionTextServer(t, capture, "ok")
			defer server.Close()
			writeModelsJSON(t, agentDir, server.URL+"/v1", testCase.configuredKey, nil)
			sessionPath := filepath.Join(workingDir, "session.jsonl")
			args := append(append([]string(nil), testCase.args...), "--session", sessionPath)
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			exitCode := app.RunProduction(
				context.Background(),
				productionTestConfig(workingDir, agentDir, testCase.environment),
				args,
				&stdout,
				&stderr,
			)
			if exitCode != app.ExitSuccess || stdout.String() != "ok\n" || stderr.Len() != 0 {
				t.Fatalf("RunProduction() = code %d, stdout %q, stderr %q", exitCode, stdout.String(), stderr.String())
			}
			request := capture.snapshot()
			if request.count != 1 || request.authorization != "Bearer "+testCase.wantKey {
				t.Fatalf("request = count %d, authorization %q", request.count, request.authorization)
			}
		})
	}
}

func TestRunProductionWindowsExistingAuthFailsBeforeSessionOrNetwork(t *testing.T) {
	workingDir := t.TempDir()
	agentDir := t.TempDir()
	writeModelsJSON(t, agentDir, "https://fixture.invalid/v1", stringPointer("configured-lower"), nil)
	writeAuthJSON(t, agentDir, `{"openai":{"type":"oauth","access":"stored-secret"}}`)
	sessionParent := filepath.Join(workingDir, "must-not-exist")
	doer := &countingProductionDoer{}
	config := productionTestConfig(workingDir, agentDir, []string{"OPENAI_API_KEY=ambient-secret"})
	config.OpenAIHTTPClient = doer
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := app.RunProduction(context.Background(), config, []string{
		"--model", "openai/windows-existing", "-p", "hello",
		"--session", filepath.Join(sessionParent, "session.jsonl"),
	}, &stdout, &stderr)
	if exitCode != app.ExitFailure || stdout.Len() != 0 || !strings.Contains(stderr.String(), "permissions are unsafe") {
		t.Fatalf("RunProduction() = code %d, stdout %q, stderr %q", exitCode, stdout.String(), stderr.String())
	}
	for _, secret := range []string{"stored-secret", "configured-lower", "ambient-secret"} {
		if strings.Contains(stderr.String(), secret) {
			t.Fatalf("stderr leaked %q: %q", secret, stderr.String())
		}
	}
	if doer.calls.Load() != 0 {
		t.Fatalf("preflight made %d HTTP calls", doer.calls.Load())
	}
	if _, err := os.Stat(sessionParent); !os.IsNotExist(err) {
		t.Fatalf("preflight changed session tree: %v", err)
	}
}
