//go:build windows

package app_test

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/testenv"
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
			agentDir := testenv.AgentDir(t)
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
			modelID := strings.TrimPrefix(testCase.args[1], "openai/")
			if exitCode != app.ExitSuccess || stdout.String() != "ok\n" || stderr.String() != customModelWarning(modelID) {
				t.Fatalf("RunProduction() = code %d, stdout %q, stderr %q", exitCode, stdout.String(), stderr.String())
			}
			request := capture.snapshot()
			if request.count != 1 || request.authorization != "Bearer "+testCase.wantKey {
				t.Fatalf("request = count %d, authorization %q", request.count, request.authorization)
			}
		})
	}
}
