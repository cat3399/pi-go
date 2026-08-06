package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
// summarization, durable compaction, and resume path used by the application.
func TestLiveProductionDeepSeekCompaction(t *testing.T) {
	apiKey := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	if apiKey == "" {
		t.Skip("DEEPSEEK_API_KEY is not set")
	}

	for _, api := range []string{provider.OpenAIResponsesAPI, provider.OpenAICompletionsAPI} {
		t.Run(api, func(t *testing.T) {
			cwd, agentDir, docsDir := t.TempDir(), t.TempDir(), t.TempDir()
			writeDeepSeekCompactionLiveConfig(t, agentDir, api)
			sessionPath := filepath.Join(t.TempDir(), "deepseek-compaction.jsonl")
			manager, err := session.OpenSessionManager(sessionPath, "", cwd)
			if err != nil {
				t.Fatalf("open live session failed (%T)", err)
			}

			assemblyContext, assemblyCancel := context.WithTimeout(context.Background(), 15*time.Second)
			dependencies, err := assembleProductionRuntime(assemblyContext, fixedProductionConfig(cwd, agentDir, docsDir), options{
				modelID: "openai/" + deepSeekCompactionLiveModelID,
			})
			assemblyCancel()
			if err != nil {
				_ = manager.Close()
				t.Fatalf("assemble production runtime failed (%T)", err)
			}

			createContext, createCancel := context.WithTimeout(context.Background(), 15*time.Second)
			productRuntime, err := agentruntime.Create(createContext, dependencies.factory, agentruntime.InitialOptions{
				CWD: cwd, AgentDir: agentDir, SessionManager: manager,
			})
			createCancel()
			if err != nil {
				_ = manager.Close()
				t.Fatalf("create production runtime without auth failed (%T)", err)
			}
			disposed := false
			defer func() {
				if disposed {
					return
				}
				closeContext, closeCancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer closeCancel()
				_ = productRuntime.Dispose(closeContext)
			}()

			selected, selectedOK := productRuntime.Session().SelectedModel()
			if !selectedOK || selected.Provider() != "openai" || selected.ID() != deepSeekCompactionLiveModelID || selected.API() != api {
				t.Fatalf("session-first model selection = %s/%s api=%s present=%t", selected.Provider(), selected.ID(), selected.API(), selectedOK)
			}
			if _, exists, readErr := productRuntime.Services().AuthRuntime.Read(context.Background(), "openai"); readErr != nil || exists {
				t.Fatalf("unexpected pre-injection credential state: exists=%t error=%T", exists, readErr)
			}
			if err := productRuntime.Services().AuthRuntime.SetAPIKey("openai", apiKey); err != nil {
				t.Fatalf("inject runtime credential failed (%T)", err)
			}

			lifecycle := &deepSeekCompactionLifecycle{}
			unsubscribe := productRuntime.Session().Subscribe(lifecycle.observe)
			defer unsubscribe()

			first := runDeepSeekCompactionLivePrompt(t, productRuntime.Session(),
				"Reply with exactly FIRST_OK. Do not call any tool.", "FIRST_OK")
			if first.ProviderTurns() != 1 || first.ToolExecutions() != 0 {
				t.Fatalf("first live run counts = turns %d tools %d, want 1/0", first.ProviderTurns(), first.ToolExecutions())
			}
			preCompact := runDeepSeekCompactionLivePrompt(t, productRuntime.Session(),
				"Reply with exactly PRECOMPACT_OK. Do not call any tool.", "PRECOMPACT_OK")
			if preCompact.ProviderTurns() != 1 || preCompact.ToolExecutions() != 0 {
				t.Fatalf("second pre-compaction live run counts = turns %d tools %d, want 1/0", preCompact.ProviderTurns(), preCompact.ToolExecutions())
			}

			compactContext, compactCancel := context.WithTimeout(context.Background(), 120*time.Second)
			compactResult, compactErr := productRuntime.Session().Compact(compactContext,
				"Preserve the exact FIRST_OK and PRECOMPACT_OK results and that both requests completed.")
			compactCancel()
			if compactErr != nil {
				t.Fatalf("remote manual compaction failed (%T)", compactErr)
			}
			if !compactResult.Committed || !compactResult.Input.IsSplitTurn ||
				len(compactResult.Input.MessagesToSummarize) == 0 || len(compactResult.Input.TurnPrefixMessages) == 0 ||
				!strings.Contains(compactResult.Output.Text, "**Turn Context (split turn):**") || compactResult.Output.Usage == nil {
				t.Fatalf("remote manual compaction did not execute the real ordered dual-summary path")
			}

			second := runDeepSeekCompactionLivePrompt(t, productRuntime.Session(),
				"The prior exchange was compacted. Reply with exactly SECOND_OK. Do not call any tool.", "SECOND_OK")
			if second.ProviderTurns() != 1 || second.ToolExecutions() != 0 {
				t.Fatalf("post-compaction live run counts = turns %d tools %d, want 1/0", second.ProviderTurns(), second.ToolExecutions())
			}
			lifecycle.assertManualSuccess(t)
			if countCompactionEntries(manager.Entries()) != 1 {
				t.Fatalf("live session durable compaction count is not exactly one")
			}

			disposeContext, disposeCancel := context.WithTimeout(context.Background(), 15*time.Second)
			disposeErr := productRuntime.Dispose(disposeContext)
			disposeCancel()
			if disposeErr != nil {
				t.Fatalf("dispose production runtime failed (%T)", disposeErr)
			}
			disposed = true

			assertDeepSeekCredentialNotPersisted(t, apiKey,
				filepath.Join(agentDir, "models.json"), filepath.Join(agentDir, "settings.json"), sessionPath)
			reopened, err := session.OpenSessionManager(sessionPath, "", "")
			if err != nil {
				t.Fatalf("reopen compacted JSONL failed (%T)", err)
			}
			defer reopened.Close()
			if countCompactionEntries(reopened.Entries()) != 1 {
				t.Fatalf("reopened live session compaction count is not exactly one")
			}
			messages := reopened.BuildContext().Messages()
			if len(messages) == 0 {
				t.Fatal("reopened context is empty")
			}
			last, ok := messages[len(messages)-1].(llm.AssistantTerminal)
			if !ok || last.FinishReason() != llm.FinishStop || !strings.Contains(deepSeekCompactionLiveAssistantText(last), "SECOND_OK") {
				t.Fatalf("reopened final assistant did not preserve the successful post-compaction response")
			}
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

func runDeepSeekCompactionLivePrompt(t *testing.T, runtime *agent.AgentSession, prompt, marker string) agent.Result {
	t.Helper()
	runContext, runCancel := context.WithTimeout(context.Background(), 120*time.Second)
	result, err := runtime.Run(runContext, prompt)
	runCancel()
	if err != nil {
		t.Fatalf("live Agent run failed (%T)", err)
	}
	terminal, ok := result.Terminal()
	if !result.Succeeded() || !ok || terminal.FinishReason() != llm.FinishStop {
		t.Fatalf("live Agent run did not finish successfully: turns=%d tools=%d terminal=%T", result.ProviderTurns(), result.ToolExecutions(), terminal)
	}
	if !strings.Contains(deepSeekCompactionLiveAssistantText(terminal), marker) {
		t.Fatalf("live Agent response did not contain the required marker")
	}
	return result
}

type deepSeekCompactionLifecycle struct {
	mu     sync.Mutex
	starts []agent.CompactionStartEvent
	ends   []agent.CompactionEndEvent
}

func (l *deepSeekCompactionLifecycle) observe(_ context.Context, event agent.SessionEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch value := event.(type) {
	case agent.CompactionStartEvent:
		l.starts = append(l.starts, value)
	case agent.CompactionEndEvent:
		l.ends = append(l.ends, value)
	}
}

func (l *deepSeekCompactionLifecycle) assertManualSuccess(t *testing.T) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.starts) != 1 || len(l.ends) != 1 || l.starts[0].Reason != agent.CompactionManual ||
		l.ends[0].Reason != agent.CompactionManual || l.ends[0].Aborted || l.ends[0].WillRetry ||
		l.ends[0].ErrorMessage != "" || l.ends[0].Result == nil || !l.ends[0].Result.Committed {
		t.Fatalf("manual compaction lifecycle did not close successfully: starts=%d ends=%d", len(l.starts), len(l.ends))
	}
}

func countCompactionEntries(entries []session.Entry) int {
	count := 0
	for _, entry := range entries {
		if entry.Type() == "compaction" {
			count++
		}
	}
	return count
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

func deepSeekCompactionLiveAssistantText(message llm.AssistantTerminal) string {
	var blocks []llm.AssistantBlock
	switch value := message.(type) {
	case llm.AssistantTextMessage:
		blocks = value.Blocks()
	case llm.AssistantRichMessage:
		blocks = value.Blocks()
	case llm.AssistantToolUseMessage:
		blocks = value.Blocks()
	case llm.AssistantFailureMessage:
		blocks = value.Blocks()
	}
	var text strings.Builder
	for _, block := range blocks {
		if value, ok := block.(llm.TextBlock); ok {
			text.WriteString(value.Text())
		}
	}
	return text.String()
}
