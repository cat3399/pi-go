package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/cat3399/pi-go/internal/provider"
)

func TestRuntimeCompatMergeRecursivelyClonesNamedMapsAndSlices(t *testing.T) {
	type namedMap map[string]string
	type namedSlice []namedMap
	baseNested := namedSlice{{"value": "base"}}
	overrideNested := map[string][]namedMap{"items": {{"value": "override"}}}
	base := provider.ModelCompat{OpenAICompletions: &provider.OpenAICompletionsCompat{
		ChatTemplateKwargs: map[string]any{"nested": baseNested},
	}}
	override := provider.ModelCompat{OpenAICompletions: &provider.OpenAICompletionsCompat{
		OpenRouterRouting: map[string]any{"nested": overrideNested},
	}}
	merged := mergeCompat(base, override)
	baseNested[0]["value"] = "mutated-base"
	overrideNested["items"][0]["value"] = "mutated-override"
	compat := merged.OpenAICompletions
	if got := compat.ChatTemplateKwargs["nested"].(namedSlice)[0]["value"]; got != "base" {
		t.Fatalf("base nested clone = %q", got)
	}
	if got := compat.OpenRouterRouting["nested"].(map[string][]namedMap)["items"][0]["value"]; got != "override" {
		t.Fatalf("override nested clone = %q", got)
	}
	snapshot := cloneCompat(merged)
	compat.ChatTemplateKwargs["nested"].(namedSlice)[0]["value"] = "mutated-result"
	if got := snapshot.OpenAICompletions.ChatTemplateKwargs["nested"].(namedSlice)[0]["value"]; got != "base" {
		t.Fatalf("snapshot nested clone = %q", got)
	}
}

func TestCompactionSettingsDefaultsAndExplicitFalseMatchPi(t *testing.T) {
	defaults := (CompactionSettings{})
	if !defaults.EnabledOrDefault() || defaults.ReserveTokensOrDefault() != 16_384 || defaults.KeepRecentTokensOrDefault() != 20_000 {
		t.Fatalf("compaction defaults = enabled %t reserve %d keep %d", defaults.EnabledOrDefault(), defaults.ReserveTokensOrDefault(), defaults.KeepRecentTokensOrDefault())
	}
	runtime, _, _ := newTestRuntime(t, "", `{"compaction":{"enabled":false,"reserveTokens":123,"keepRecentTokens":456}}`, false)
	settings := runtime.Snapshot().Settings.Compaction
	if settings.EnabledOrDefault() || settings.ReserveTokensOrDefault() != 123 || settings.KeepRecentTokensOrDefault() != 456 {
		t.Fatalf("explicit compaction settings = %#v", settings)
	}
}

func TestRetrySettingsDefaultsExplicitZerosAndFieldLevelProjectMerge(t *testing.T) {
	defaults := (RetrySettings{})
	if !defaults.EnabledOrDefault() || defaults.MaxRetriesOrDefault() != 3 || defaults.BaseDelayMSOrDefault() != 2_000 {
		t.Fatalf("retry defaults = enabled %t retries %d delay %d", defaults.EnabledOrDefault(), defaults.MaxRetriesOrDefault(), defaults.BaseDelayMSOrDefault())
	}
	agentDir, cwd := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(agentDir, "settings.json"), `{"retry":{"enabled":false,"maxRetries":7,"baseDelayMs":250}}`)
	writeFile(t, filepath.Join(cwd, ".pi", "settings.json"), `{"retry":{"maxRetries":0,"baseDelayMs":0}}`)
	runtime, err := NewRuntime(Options{AgentDir: agentDir, WorkingDir: cwd, ProjectTrusted: true})
	if err != nil {
		t.Fatal(err)
	}
	retry := runtime.Snapshot().Settings.Retry
	if retry.EnabledOrDefault() || retry.MaxRetriesOrDefault() != 0 || retry.BaseDelayMSOrDefault() != 0 {
		t.Fatalf("merged retry settings = %#v", retry)
	}
}

func TestSetGlobalSettingsPreservesUnportedRetryProviderFields(t *testing.T) {
	runtime, agentDir, _ := newTestRuntime(t, "", `{"retry":{"enabled":true,"provider":{"maxRetries":9},"future":true}}`, false)
	disabled, zero := false, uint64(0)
	if err := runtime.SetGlobalSettings(context.Background(), func(settings *Settings) error {
		settings.Retry.Enabled = &disabled
		settings.Retry.MaxRetries = &zero
		settings.Retry.BaseDelayMS = &zero
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(agentDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	retry := root["retry"].(map[string]any)
	providerSettings := retry["provider"].(map[string]any)
	if retry["enabled"] != false || retry["maxRetries"] != float64(0) || retry["baseDelayMs"] != float64(0) ||
		retry["future"] != true || providerSettings["maxRetries"] != float64(9) {
		t.Fatalf("persisted retry = %#v", retry)
	}
}

func TestSetGlobalSettingsReplacesNullOptionalObjects(t *testing.T) {
	runtime, agentDir, _ := newTestRuntime(t, "", `{"retry":null,"compaction":null}`, false)
	enabled := false
	zero := uint64(0)
	if err := runtime.SetGlobalSettings(context.Background(), func(settings *Settings) error {
		settings.Retry.Enabled = &enabled
		settings.Retry.MaxRetries = &zero
		settings.Compaction.Enabled = &enabled
		settings.Compaction.ReserveTokens = &zero
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(agentDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	retry, retryOK := root["retry"].(map[string]any)
	compaction, compactionOK := root["compaction"].(map[string]any)
	if !retryOK || !compactionOK || retry["enabled"] != false || retry["maxRetries"] != float64(0) ||
		compaction["enabled"] != false || compaction["reserveTokens"] != float64(0) {
		t.Fatalf("persisted optional objects = %#v", root)
	}
}

func TestBuiltinOpenAIModelMatchesPiCatalogBaseline(t *testing.T) {
	model := builtinOpenAIModel(DefaultOpenAIModel)
	if model.Provider != OpenAIProviderID || model.ID != "gpt-5.5" || model.Name != "GPT-5.5" ||
		model.API != OpenAIResponsesAPI || model.BaseURL != "https://api.openai.com/v1" || !model.Reasoning ||
		model.ContextWindow != 272_000 || model.MaxTokens != 128_000 {
		t.Fatalf("builtin identity/capabilities = %#v", model)
	}
	if len(model.Input) != 2 || model.Input[0] != provider.InputText || model.Input[1] != provider.InputImage {
		t.Fatalf("builtin input = %#v", model.Input)
	}
	off, hasOff := model.ThinkingLevelMap[provider.ThinkingOff]
	minimal, hasMinimal := model.ThinkingLevelMap[provider.ThinkingMinimal]
	xhigh, hasXHigh := model.ThinkingLevelMap[provider.ThinkingXHigh]
	if !hasOff || off == nil || *off != "none" || !hasMinimal || minimal != nil || !hasXHigh || xhigh == nil || *xhigh != "xhigh" {
		t.Fatalf("builtin thinking map = %#v", model.ThinkingLevelMap)
	}
	if model.Cost.Input != 5 || model.Cost.Output != 30 || model.Cost.CacheRead != 0.5 || model.Cost.CacheWrite != 0 || len(model.Cost.Tiers) != 1 {
		t.Fatalf("builtin base cost = %#v", model.Cost)
	}
	tier := model.Cost.Tiers[0]
	if tier.InputTokensAbove != 272_000 || tier.Input != 10 || tier.Output != 45 || tier.CacheRead != 1 || tier.CacheWrite != 0 {
		t.Fatalf("builtin long-context tier = %#v", tier)
	}
	if _, err := model.Ref(); err != nil {
		t.Fatalf("builtin is not a complete provider Model: %v", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newTestRuntime(t *testing.T, models, settings string, trusted bool) (*Runtime, string, string) {
	t.Helper()
	agent, cwd := t.TempDir(), t.TempDir()
	if models != "" {
		writeFile(t, filepath.Join(agent, "models.json"), models)
	}
	if settings != "" {
		writeFile(t, filepath.Join(agent, "settings.json"), settings)
	}
	r, err := NewRuntime(Options{AgentDir: agent, WorkingDir: cwd, ProjectTrusted: trusted})
	if err != nil {
		t.Fatal(err)
	}
	return r, agent, cwd
}

func TestRuntimeModelsJSONCOverlayAndCustomModel(t *testing.T) {
	r, _, _ := newTestRuntime(t, `// accepted comment
{"providers":{"openai":{"baseUrl":"https://example.test/v1","api":"openai-responses","headers":{"X-Base":"one"},"models":[{"id":"custom","api":"openai-responses","headers":{"x-base":"two"}},],"future":{"preserve":true}}}}`, "", false)
	s := r.Snapshot()
	if len(s.Models) != 2 {
		t.Fatalf("models = %#v", s.Models)
	}
	got, err := r.Resolve(Selection{Provider: "openai", Model: "custom"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Model.API != OpenAIResponsesAPI || got.Model.BaseURL != "https://example.test/v1" {
		t.Fatalf("custom overlay = %#v", got.Model)
	}
	if err := r.ValidateRoute(got.Model); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("headers must fail at selected route: %v", err)
	}
	if _, err := r.Resolve(Selection{Provider: "openai", Model: "not-listed"}); err != nil {
		t.Fatalf("explicit custom model: %v", err)
	}
}

func TestRuntimeAdmitsChatCompletionsCompatWithoutResponsesFallback(t *testing.T) {
	r, _, _ := newTestRuntime(t, `{"providers":{"openai":{"api":"openai-completions","compat":{"supportsUsageInStreaming":false,"maxTokensField":"max_tokens","thinkingFormat":"openai"},"models":[{"id":"chat"}]}}}`, "", false)
	selection, err := r.Resolve(Selection{Provider: "openai", Model: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ValidateRoute(selection.Model); err != nil {
		t.Fatalf("ValidateRoute: %v", err)
	}
	compat := selection.Model.Compat.OpenAICompletions
	if selection.Model.API != "openai-completions" || compat == nil || compat.SupportsUsageInStreaming == nil || *compat.SupportsUsageInStreaming || compat.MaxTokensField == nil || *compat.MaxTokensField != "max_tokens" {
		t.Fatalf("model=%#v", selection.Model)
	}
}

func TestRuntimeMergesProviderAndModelCompatFieldwise(t *testing.T) {
	r, _, _ := newTestRuntime(t, `{"providers":{"openai":{"api":"openai-completions","compat":{"supportsUsageInStreaming":false,"maxTokensField":"max_tokens"},"models":[{"id":"chat","compat":{"supportsUsageInStreaming":true}}]}}}`, "", false)
	selection, err := r.Resolve(Selection{Provider: "openai", Model: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	compat := selection.Model.Compat.OpenAICompletions
	if compat == nil || compat.SupportsUsageInStreaming == nil || !*compat.SupportsUsageInStreaming || compat.MaxTokensField == nil || *compat.MaxTokensField != "max_tokens" {
		t.Fatalf("merged compat=%#v", compat)
	}
}

func TestRuntimeModelOverrideDoesNotEraseBuiltinMetadata(t *testing.T) {
	r, _, _ := newTestRuntime(t, `{"providers":{"openai":{"headers":{"X-Base":"one"},"modelOverrides":{"gpt-5.5":{"name":"renamed","reasoning":true,"headers":{"x-base":"two"}}}}}}`, "", false)
	got, err := r.Resolve(Selection{Model: "gpt-5.5"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Model.Name != "renamed" || got.Model.API != OpenAIResponsesAPI {
		t.Fatalf("override = %#v", got.Model)
	}
	if err := r.ValidateRoute(got.Model); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("override options must fail at selected route: %v", err)
	}
}

func TestRuntimeDuplicateJSONCFieldsAreRejectedAtEveryDepth(t *testing.T) {
	for _, content := range []string{
		`{"providers":{"openai":{"api":"openai-responses","api":"future-secret"}}}`,
		`{"providers":{"openai":{"models":[{"id":"one","api":"openai-responses","nested":{"x":1,"x":2}}]}}}`,
		`{"providers":{"openai":{"models":[{"id":"one","api":"openai-responses","nested":[{"x":1,"x":2}]}]}}}`,
	} {
		path := filepath.Join(t.TempDir(), "models.json")
		writeFile(t, path, content)
		if _, err := loadModels(path); err == nil || !strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "future-secret") {
			t.Fatalf("loadModels duplicate = %v", err)
		}
	}
}

func TestRuntimeOnlySelectedProviderRejectsFutureFields(t *testing.T) {
	r, _, _ := newTestRuntime(t, `{"providers":{"future":{"compat":{"token":"do-not-leak"},"models":[{"id":"ignored","api":"openai-responses"}]},"openai":{"models":[{"id":"supported","api":"openai-responses"}]}}}`, "", false)
	openAI, err := r.Resolve(Selection{Provider: "openai", Model: "supported"})
	if err != nil || r.ValidateRoute(openAI.Model) != nil {
		t.Fatalf("unselected future provider must not block openai: %#v, %v", openAI, err)
	}
	future, err := r.Resolve(Selection{Provider: "future", Model: "ignored"})
	if err != nil || !errors.Is(r.ValidateRoute(future.Model), ErrUnsupported) {
		t.Fatalf("selected future provider must fail safely: %#v, %v", future, err)
	}
	r, _, _ = newTestRuntime(t, `{"providers":{"openai":{"modelOverrides":{"custom":{"compat":{"token":"do-not-leak"}}}}}}`, "", false)
	custom, err := r.Resolve(Selection{Provider: "openai", Model: "custom"})
	if err != nil || !errors.Is(r.ValidateRoute(custom.Model), ErrUnsupported) {
		t.Fatalf("selected custom override must fail safely at the compatibility boundary: %#v, %v", custom, err)
	}
}

func TestRuntimeCanonicalIdentifiersRejectDuplicatesAndApplyOverrides(t *testing.T) {
	for _, pair := range [][2]string{{"OpenAI", "openai"}, {"K", "K"}, {"Σ", "ς"}} {
		if !strings.EqualFold(pair[0], pair[1]) || canonicalKey(pair[0]) != canonicalKey(pair[1]) {
			t.Fatalf("canonical mismatch for %q and %q", pair[0], pair[1])
		}
	}
	if strings.EqualFold("İ", "i") || canonicalKey("İ") == canonicalKey("i") {
		t.Fatal("canonical key collapsed identifiers outside strings.EqualFold")
	}
	for _, content := range []string{
		`{"providers":{"OpenAI":{},"openai":{}}}`,
		`{"providers":{"openai":{"modelOverrides":{"GPT-5.5":{},"gpt-5.5":{}}}}}`,
	} {
		path := filepath.Join(t.TempDir(), "models.json")
		writeFile(t, path, content)
		if _, err := loadModels(path); err == nil || !strings.Contains(err.Error(), "case-fold duplicate") {
			t.Fatalf("case-fold duplicate = %v", err)
		}
	}
	r, _, _ := newTestRuntime(t, `{"providers":{"OpEnAi":{"modelOverrides":{"GPT-5.5":{"compat":{"token":"case-secret"}},"CUSTOM":{"futureOption":"case-secret"}}}}}`, "", false)
	for _, selection := range []Selection{{Provider: "OPENAI", Model: "gPt-5.5"}, {Provider: "openai", Model: "custom"}} {
		resolved, err := r.Resolve(selection)
		if err != nil {
			t.Fatalf("resolve %#v: %v", selection, err)
		}
		err = r.ValidateRoute(resolved.Model)
		if !errors.Is(err, ErrUnsupported) || strings.Contains(err.Error(), "case-secret") {
			t.Fatalf("canonical selected override = %v", err)
		}
		if resolved.Model.Provider != OpenAIProviderID {
			t.Fatalf("provider was not canonical: %#v", resolved.Model)
		}
	}
}

func TestRuntimeCustomFallbackUsesOriginalProviderDefaultBaseline(t *testing.T) {
	models := `{"providers":{"openai":{"api":"provider-api","baseUrl":"https://provider.invalid/v1","models":[{"id":"aaa","name":"poison","api":"poison-api","baseUrl":"https://aaa.invalid/v1","headers":{"Authorization":"aaa-secret"},"compat":{"token":"aaa-secret"},"futureOption":"aaa-secret"}]}}}`
	r, _, _ := newTestRuntime(t, models, "", false)
	resolved, err := r.Resolve(Selection{Provider: "OPENAI", Model: "custom"})
	if err != nil {
		t.Fatal(err)
	}
	model := resolved.Model
	if model.Provider != OpenAIProviderID || model.ID != "custom" || model.Name != "custom" || model.API != "provider-api" || model.BaseURL != "https://provider.invalid/v1" {
		t.Fatalf("custom baseline = %#v", model)
	}
	if len(model.Headers) != 0 || len(model.UnsupportedFields) != 0 || len(model.UnknownFields) != 0 || strings.Contains(fmt.Sprintf("%#v", model), "aaa-secret") {
		t.Fatalf("custom inherited non-default per-model metadata: %#v", model)
	}
	if err := r.ValidateRoute(model); err != nil {
		t.Fatalf("clean custom route = %v", err)
	}
}

func TestRuntimeStrictDiagnosticsAndKeepsLastHealthySnapshot(t *testing.T) {
	r, agent, _ := newTestRuntime(t, `{"providers":{"openai":{"models":[{"id":"first","api":"openai-responses"}]}}}`, "", false)
	before := r.Snapshot()
	writeFile(t, filepath.Join(agent, "models.json"), `{"providers":{"openai":{"models":[{"id":"dup"},{"id":"dup"}]}}}`)
	err := r.Reload(context.Background())
	var diagnostic Diagnostic
	if !errors.As(err, &diagnostic) || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("reload error = %v", err)
	}
	after := r.Snapshot()
	if after.Generation != before.Generation || len(after.Models) != len(before.Models) {
		t.Fatalf("unhealthy reload published %#v -> %#v", before, after)
	}
}

func TestRuntimeSettingsTrustPrecedenceScopesAndUnknownPreservation(t *testing.T) {
	r, agent, cwd := newTestRuntime(t, `{"providers":{"openai":{"models":[{"id":"project-model","api":"openai-responses"}]}}}`, `{"defaultProvider":"openai","defaultModel":"gpt-5.5","defaultThinkingLevel":"low","unknown":{"keep":1}}`, false)
	writeFile(t, filepath.Join(cwd, ".pi", "settings.json"), `{"defaultModel":"project-model"}`)
	got, err := r.Resolve(Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Model.ID != DefaultOpenAIModel {
		t.Fatalf("untrusted project selected %q", got.Model.ID)
	}
	r.options.ProjectTrusted = true
	if err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err = r.Resolve(Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Model.ID != "project-model" {
		t.Fatalf("trusted project did not win: %#v", got)
	}
	if r.Snapshot().Settings.DefaultThinkingLevel != provider.ThinkingLow {
		t.Fatalf("default thinking was not parsed/merged: %#v", r.Snapshot().Settings)
	}
	if err := r.SetGlobalSettings(context.Background(), func(s *Settings) error {
		s.EnabledModels = []string{"openai/gpt-5.5"}
		s.DefaultThinkingLevel = provider.ThinkingHigh
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(agent, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"unknown"`) || !strings.Contains(string(b), `"defaultThinkingLevel": "high"`) {
		t.Fatalf("unknown setting lost: %s", b)
	}
}

func TestRuntimeSettingsDefaultThinkingValidationAndProjectOverride(t *testing.T) {
	for _, invalid := range []string{`{"defaultThinkingLevel":"turbo"}`, `{"defaultThinkingLevel":null}`, `{"defaultThinkingLevel":1}`} {
		if _, _, err := newTestRuntimeNoFatal(t, "", invalid, false); err == nil {
			t.Fatalf("invalid defaultThinkingLevel was accepted: %s", invalid)
		}
	}
	r, _, cwd := newTestRuntime(t, "", `{"defaultThinkingLevel":"low"}`, true)
	writeFile(t, filepath.Join(cwd, ".pi", "settings.json"), `{"defaultThinkingLevel":"xhigh","future":{"keep":true}}`)
	if err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := r.Snapshot().Settings.DefaultThinkingLevel; got != provider.ThinkingXHigh {
		t.Fatalf("project thinking override = %q", got)
	}
}

func newTestRuntimeNoFatal(t *testing.T, models, settings string, trusted bool) (*Runtime, string, error) {
	t.Helper()
	agent, cwd := t.TempDir(), t.TempDir()
	if models != "" {
		writeFile(t, filepath.Join(agent, "models.json"), models)
	}
	if settings != "" {
		writeFile(t, filepath.Join(agent, "settings.json"), settings)
	}
	r, err := NewRuntime(Options{AgentDir: agent, WorkingDir: cwd, ProjectTrusted: trusted})
	return r, cwd, err
}

func TestRuntimeScopedOrderAndUnavailableDiagnostic(t *testing.T) {
	r, _, _ := newTestRuntime(t, `{"providers":{"openai":{"models":[{"id":"second","api":"openai-responses"},{"id":"third","api":"openai-responses"}]}}}`, `{"enabledModels":["openai/second","missing","openai/third"]}`, false)
	got, err := r.Resolve(Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Model.ID != "second" || len(got.Diagnostics) != 1 {
		t.Fatalf("scope = %#v", got)
	}
}

func TestRuntimeConcurrentSnapshotAndReload(t *testing.T) {
	r, agent, _ := newTestRuntime(t, `{"providers":{"openai":{"models":[{"id":"one","api":"openai-responses"}]}}}`, "", false)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 50; n++ {
				s := r.Snapshot()
				for _, m := range s.Models {
					if _, err := m.Ref(); err != nil {
						t.Errorf("bad model: %v", err)
					}
				}
				_, _ = r.Resolve(Selection{Provider: "openai", Model: "custom"})
			}
		}()
	}
	for i := 0; i < 20; i++ {
		writeFile(t, filepath.Join(agent, "models.json"), `{"providers":{"openai":{"models":[{"id":"one","api":"openai-responses"}]}}}`)
		if err := r.Reload(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}

func TestGlobalSettingsCancellationFaultAndPrivateAdmission(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows v0.1 fails closed for private durable configuration")
	}
	r, agent, _ := newTestRuntime(t, "", `{"defaultModel":"gpt-5.5"}`, false)
	release, err := acquireLocal(context.Background(), r.local)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.SetGlobalSettings(ctx, func(*Settings) error { return nil }); !errors.Is(err, ErrCancelled) {
		t.Fatalf("cancelled settings write = %v", err)
	}
	release()
	r.faults.beforeRename = func() error { return errors.New("injected") }
	if err := r.SetGlobalSettings(context.Background(), func(s *Settings) error { s.DefaultModel = "before"; return nil }); err == nil {
		t.Fatal("pre-rename settings fault succeeded")
	}
	content, err := os.ReadFile(filepath.Join(agent, "settings.json"))
	if err != nil || strings.Contains(string(content), "before") {
		t.Fatalf("pre-rename wrote settings: %q, %v", content, err)
	}
	r.faults.beforeRename = nil
	r.faults.afterRename = func() error { return errors.New("injected") }
	if err := r.SetGlobalSettings(context.Background(), func(s *Settings) error { s.DefaultModel = "after"; return nil }); !errors.Is(err, ErrCommitUnknown) {
		t.Fatalf("post-rename settings error = %v", err)
	}
	content, err = os.ReadFile(filepath.Join(agent, "settings.json"))
	if err != nil || !strings.Contains(string(content), "after") {
		t.Fatalf("post-rename did not publish: %q, %v", content, err)
	}
	r.faults.afterRename = nil
	synced := false
	r.faults.syncDirectory = func(path string) error { synced = true; return syncModelDirectory(path) }
	if err := r.SetGlobalSettings(context.Background(), func(s *Settings) error { s.DefaultModel = "durable"; return nil }); err != nil || !synced {
		t.Fatalf("successful settings publication did not sync parent: %v, synced=%t", err, synced)
	}
	r.faults.syncDirectory = nil
	if err := os.Chmod(filepath.Join(agent, "settings.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.Reload(context.Background()); !errors.Is(err, ErrUnsafeMode) {
		t.Fatalf("unsafe settings mode = %v", err)
	}
}

func TestGlobalSettingsRequiresPreexistingDurableParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows v0.1 fails closed earlier")
	}
	root := t.TempDir()
	agent := filepath.Join(root, "missing", "agent")
	r, err := NewRuntime(Options{AgentDir: agent, WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	err = r.SetGlobalSettings(context.Background(), func(s *Settings) error { s.DefaultModel = "model"; return nil })
	if !errors.Is(err, ErrPersistence) {
		t.Fatalf("missing parent settings write = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "missing")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("missing ancestor was created: %v", statErr)
	}
}

func TestWindowsGlobalSettingsPersistenceFailsClosed(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only contract")
	}
	r, _, _ := newTestRuntime(t, "", "", false)
	if err := r.SetGlobalSettings(context.Background(), func(s *Settings) error { s.DefaultModel = "model"; return nil }); !errors.Is(err, ErrPersistence) {
		t.Fatalf("settings write = %v", err)
	}
}

func FuzzLoadModelsDoesNotPanic(f *testing.F) {
	f.Add([]byte(`{"providers":{"openai":{}}}`))
	f.Add([]byte(`// comment\n{"providers":{}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(t.TempDir(), "models.json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		_, _ = loadModels(path)
	})
}
