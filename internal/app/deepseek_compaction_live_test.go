package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	agentruntime "github.com/cat3399/pi-go/internal/runtime"
	"github.com/cat3399/pi-go/internal/session"
)

const (
	deepSeekCompactionLiveBaseURL = "https://api.deepseek.com"
	deepSeekCompactionLiveModelID = "deepseek-v4-flash"
)

// TestLiveProductionDeepSeekCompaction is deliberately opt-in. It exercises
// the same production assembly, runtime auth resolution, Agent runs, remote
// handoff generation, durable checkpoints, and resume used by the application.
func TestLiveProductionDeepSeekCompaction(t *testing.T) {
	apiKey := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	if apiKey == "" {
		t.Skip("DEEPSEEK_API_KEY is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "pi-go")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/pi-go")
	build.Dir = root
	if err := build.Run(); err != nil {
		t.Fatalf("build live CLI failed (%T)", err)
	}
	for _, api := range []string{provider.OpenAIResponsesAPI, provider.OpenAICompletionsAPI} {
		t.Run(api, func(t *testing.T) {
			cwd, agentDir := t.TempDir(), t.TempDir()
			writeDeepSeekCompactionLiveConfig(t, agentDir, api)
			path := filepath.Join(agentDir, "models.json")
			data, _ := os.ReadFile(path)
			var catalog map[string]any
			if err := json.Unmarshal(data, &catalog); err != nil {
				t.Fatal(err)
			}
			configured := catalog["providers"].(map[string]any)["openai"].(map[string]any)
			configured["apiKey"] = "DEEPSEEK_API_KEY"
			configured["models"].([]any)[0].(map[string]any)["maxTokens"] = 8192
			data, _ = json.Marshal(catalog)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(agentDir, "sessions", "live", "handoff.jsonl")
			run := func(args ...string) string {
				t.Helper()
				command := exec.CommandContext(ctx, binary, args...)
				command.Dir = cwd
				command.Env = append(os.Environ(), "PI_GO_AGENT_DIR="+agentDir)
				output, err := command.Output()
				if err != nil {
					t.Fatalf("live CLI failed (%T)", err)
				}
				return string(output)
			}
			for _, marker := range []string{"FIRST_OK", "PRECOMPACT_OK"} {
				output := run("run", "--model", "openai/"+deepSeekCompactionLiveModelID, "--session", file, "-p", "Reply with exactly "+marker+". Do not call any tool.")
				if !strings.Contains(output, marker) {
					t.Fatal("live task marker missing")
				}
			}
			index, err := session.OpenReadIndex(file)
			if err != nil {
				t.Fatal(err)
			}
			view, err := index.ReadHistoryView(session.HistoryViewQuery{})
			if err != nil {
				t.Fatal(err)
			}
			output := run("session", "--id", view.SessionID, "--agent-dir", agentDir, "--json", "context", "compact", "--instructions", "Preserve FIRST_OK and PRECOMPACT_OK as completed results.")
			var change agent.ContextChangeResult
			if err := json.Unmarshal([]byte(output), &change); err != nil || change.State != "completed" {
				t.Fatal("live CLI handoff did not commit")
			}
			index, err = session.OpenReadIndex(file)
			if err != nil {
				t.Fatal(err)
			}
			view, err = index.ReadHistoryView(session.HistoryViewQuery{})
			if err != nil || len(view.Windows) != 2 || !strings.Contains(view.Windows[1].Handoff, "FIRST_OK") || !strings.Contains(view.Windows[1].Handoff, "PRECOMPACT_OK") {
				t.Fatal("live handoff lost completed work")
			}
			if output := run("run", "--model", "openai/"+deepSeekCompactionLiveModelID, "--session", file, "-p", "Reply with exactly SECOND_OK. Do not call any tool."); !strings.Contains(output, "SECOND_OK") {
				t.Fatal("resume after handoff failed")
			}
			assertDeepSeekCredentialNotPersisted(t, apiKey, path, filepath.Join(agentDir, "settings.json"), file)
		})
	}
}

// TestLiveProductionDeepSeekBranchSummary is opt-in with the same credential
// gate. It executes the real production navigateTree provider path for both
// DeepSeek-compatible transports and verifies durable reopen semantics.
func TestLiveProductionDeepSeekBranchSummary(t *testing.T) {
	apiKey := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	if apiKey == "" {
		t.Skip("DEEPSEEK_API_KEY is not set")
	}
	for _, api := range []string{provider.OpenAIResponsesAPI, provider.OpenAICompletionsAPI} {
		t.Run(api, func(t *testing.T) {
			cwd, agentDir, docsDir := t.TempDir(), t.TempDir(), t.TempDir()
			writeDeepSeekCompactionLiveConfig(t, agentDir, api)
			sessionPath := filepath.Join(t.TempDir(), "deepseek-branch-summary.jsonl")
			manager, err := session.OpenSessionManager(sessionPath, "", cwd)
			if err != nil {
				t.Fatal(err)
			}
			appendUser := func(text string, at int64) session.Entry {
				message, createErr := llm.NewUserTextMessage(text, time.UnixMilli(at))
				if createErr != nil {
					t.Fatal(createErr)
				}
				entry, appendErr := manager.AppendLLMMessage(context.Background(), message)
				if appendErr != nil {
					t.Fatal(appendErr)
				}
				return entry
			}
			appendAssistant := func(text string) session.Entry {
				entry, appendErr := manager.AppendLLMMessage(context.Background(), deepSeekBranchHistoryAssistant(t, text, api))
				if appendErr != nil {
					t.Fatal(appendErr)
				}
				return entry
			}
			appendUser("root request", 1)
			common := appendAssistant("common answer")
			target := appendUser("target draft", 2)
			if err := manager.Branch(common.ID()); err != nil {
				t.Fatal(err)
			}
			appendUser("Explore a different implementation and preserve the exact marker BRANCH_WORK.", 3)
			appendAssistant("BRANCH_WORK completed on the alternate branch")

			assemblyCtx, assemblyCancel := context.WithTimeout(context.Background(), 15*time.Second)
			dependencies, err := assembleProductionRuntime(assemblyCtx, fixedProductionConfig(cwd, agentDir, docsDir), options{modelID: "openai/" + deepSeekCompactionLiveModelID})
			assemblyCancel()
			if err != nil {
				_ = manager.Close()
				t.Fatal(err)
			}
			createCtx, createCancel := context.WithTimeout(context.Background(), 15*time.Second)
			productRuntime, err := agentruntime.Create(createCtx, dependencies.factory, agentruntime.InitialOptions{CWD: cwd, AgentDir: agentDir, SessionManager: manager})
			createCancel()
			if err != nil {
				_ = manager.Close()
				t.Fatal(err)
			}
			disposed := false
			defer func() {
				if !disposed {
					closeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					_ = productRuntime.Dispose(closeCtx)
				}
			}()
			if err := productRuntime.Services().AuthRuntime.SetAPIKey("openai", apiKey); err != nil {
				t.Fatal(err)
			}
			navigateCtx, navigateCancel := context.WithTimeout(context.Background(), 120*time.Second)
			instructions := "Preserve the exact marker BRANCH_WORK in the structured branch summary."
			result, err := productRuntime.Session().NavigateTree(navigateCtx, target.ID(), agent.NavigateTreeOptions{Summarize: true, CustomInstructions: &instructions})
			navigateCancel()
			if err != nil {
				t.Fatalf("live branch summary failed (%T)", err)
			}
			if result.EditorText == nil || *result.EditorText != "target draft" || result.SummaryEntry == nil {
				t.Fatalf("navigate result=%#v", result)
			}
			payload, ok := result.SummaryEntry.Payload().(session.BranchSummaryPayload)
			if !ok || payload.FromID != common.ID() || payload.FromHook || payload.Usage == nil || !strings.Contains(payload.Summary, "BRANCH_WORK") {
				t.Fatalf("live branch payload missing real model summary/provenance")
			}
			disposeCtx, disposeCancel := context.WithTimeout(context.Background(), 15*time.Second)
			err = productRuntime.Dispose(disposeCtx)
			disposeCancel()
			if err != nil {
				t.Fatal(err)
			}
			disposed = true
			assertDeepSeekCredentialNotPersisted(t, apiKey, filepath.Join(agentDir, "models.json"), filepath.Join(agentDir, "settings.json"), sessionPath)
			reopened, err := session.OpenSessionManager(sessionPath, "", "")
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			persisted, ok := reopened.Entry(result.SummaryEntry.ID())
			if !ok {
				t.Fatal("live branch summary missing after reopen")
			}
			persistedPayload, ok := persisted.Payload().(session.BranchSummaryPayload)
			if !ok || persistedPayload.Usage == nil || persistedPayload.FromID != common.ID() {
				t.Fatalf("reopened live branch payload=%#v", persisted.Payload())
			}
		})
	}
}

func deepSeekBranchHistoryAssistant(t *testing.T, text, api string) llm.AssistantTextMessage {
	t.Helper()
	block, err := llm.NewTextBlock(text)
	if err != nil {
		t.Fatal(err)
	}
	usage, err := llm.NewUsage(llm.UsageSpec{Input: 1, Output: 1})
	if err != nil {
		t.Fatal(err)
	}
	message, err := llm.NewAssistantTextMessage([]llm.TextBlock{block}, llm.FinishStop, usage, time.Now(), llm.AssistantProvenance{
		Provider: "openai", API: api, Model: deepSeekCompactionLiveModelID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func writeDeepSeekCompactionLiveConfig(t *testing.T, agentDir, api string) {
	t.Helper()
	compat := map[string]any{"supportsDeveloperRole": false}
	if api == provider.OpenAICompletionsAPI {
		compat = map[string]any{
			"supportsStore": false, "supportsDeveloperRole": false, "supportsReasoningEffort": false,
			"requiresReasoningContentOnAssistantMessages": true, "thinkingFormat": "deepseek",
		}
	}
	models, err := json.Marshal(map[string]any{"providers": map[string]any{"openai": map[string]any{
		"baseUrl": deepSeekCompactionLiveBaseURL,
		"models": []any{map[string]any{
			"id": deepSeekCompactionLiveModelID, "name": "DeepSeek V4 Flash", "api": api,
			"reasoning": true, "contextWindow": 1_000_000, "maxTokens": 256, "compat": compat,
		}},
	}}})
	if err != nil {
		t.Fatalf("encode live models config failed (%T)", err)
	}
	settings, err := json.Marshal(map[string]any{
		"defaultProvider": "openai", "defaultModel": deepSeekCompactionLiveModelID, "defaultThinkingLevel": "off",
		"compaction": map[string]any{"enabled": true, "reserveTokens": 256, "keepRecentTokens": 1},
		"retry":      map[string]any{"enabled": true, "maxRetries": 1, "baseDelayMs": 1_000},
	})
	if err != nil {
		t.Fatalf("encode live settings failed (%T)", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), models, 0o600); err != nil {
		t.Fatalf("write live models config failed (%T)", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), settings, 0o600); err != nil {
		t.Fatalf("write live settings failed (%T)", err)
	}
}

func assertDeepSeekCredentialNotPersisted(t *testing.T, apiKey string, paths ...string) {
	t.Helper()
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read durable live artifact failed (%T)", err)
		}
		if bytes.Contains(data, []byte(apiKey)) {
			t.Fatalf("runtime credential was persisted in %s", filepath.Base(path))
		}
	}
}
