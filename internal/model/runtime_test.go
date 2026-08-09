package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
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

func TestQueueModeSettingsDefaultsMergeLegacyMigrationAndLosslessWrite(t *testing.T) {
	defaults := Settings{}
	if defaults.SteeringModeOrDefault() != QueueModeOneAtATime || defaults.FollowUpModeOrDefault() != QueueModeOneAtATime {
		t.Fatalf("queue defaults = %q/%q", defaults.SteeringModeOrDefault(), defaults.FollowUpModeOrDefault())
	}
	agentDir, cwd := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(agentDir, "settings.json"), `{"queueMode":"all","followUpMode":"one-at-a-time","future":{"keep":true}}`)
	writeFile(t, filepath.Join(cwd, ".pi", "settings.json"), `{"steeringMode":"one-at-a-time","followUpMode":"all"}`)
	runtime, err := NewRuntime(Options{AgentDir: agentDir, WorkingDir: cwd, ProjectTrusted: true})
	if err != nil {
		t.Fatal(err)
	}
	effective := runtime.Snapshot().Settings
	if effective.SteeringModeOrDefault() != QueueModeOneAtATime || effective.FollowUpModeOrDefault() != QueueModeAll {
		t.Fatalf("effective queue modes = %q/%q", effective.SteeringMode, effective.FollowUpMode)
	}
	if err := runtime.SetGlobalSettings(context.Background(), func(settings *Settings) error {
		settings.FollowUpMode = QueueModeAll
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(agentDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	if _, exists := root["queueMode"]; exists {
		t.Fatal("legacy queueMode survived migration write")
	}
	var steering, follow string
	if err := json.Unmarshal(root["steeringMode"], &steering); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(root["followUpMode"], &follow); err != nil {
		t.Fatal(err)
	}
	var future map[string]bool
	if err := json.Unmarshal(root["future"], &future); err != nil {
		t.Fatal(err)
	}
	if steering != QueueModeAll || follow != QueueModeAll || !future["keep"] {
		t.Fatalf("lossless migrated root = %s", data)
	}
}

func TestQueueModeMigrationDoesNotReplaceExplicitSteeringOrUnknownLegacyField(t *testing.T) {
	runtime, agentDir, _ := newTestRuntime(t, "", `{"queueMode":"all","steeringMode":"one-at-a-time","future":7}`, false)
	if got := runtime.Snapshot().Settings.SteeringModeOrDefault(); got != QueueModeOneAtATime {
		t.Fatalf("explicit steering = %q", got)
	}
	if err := runtime.SetGlobalSettings(context.Background(), func(settings *Settings) error {
		settings.FollowUpMode = QueueModeAll
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(agentDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	if string(root["queueMode"]) != `"all"` || string(root["future"]) != "7" {
		t.Fatalf("explicit steering lossless root = %s", data)
	}
}

func TestQueueModeSettingsRejectInvalidValues(t *testing.T) {
	for _, input := range []string{
		`{"steeringMode":"batch"}`,
		`{"followUpMode":"batch"}`,
		`{"queueMode":"batch"}`,
	} {
		agentDir := t.TempDir()
		writeFile(t, filepath.Join(agentDir, "settings.json"), input)
		if _, err := NewRuntime(Options{AgentDir: agentDir, WorkingDir: t.TempDir()}); err == nil {
			t.Fatalf("NewRuntime(%s) error = %v", input, err)
		}
	}
}

func TestBranchSummarySettingsDefaultMergeAndPersistence(t *testing.T) {
	if got := (BranchSummarySettings{}).ReserveTokensOrDefault(); got != 16_384 {
		t.Fatalf("default reserve=%d", got)
	}
	agentDir, cwd := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(agentDir, "settings.json"), `{"branchSummary":{"reserveTokens":123,"skipPrompt":true,"future":"kept"}}`)
	writeFile(t, filepath.Join(cwd, ".pi", "settings.json"), `{"branchSummary":{"reserveTokens":0}}`)
	runtime, err := NewRuntime(Options{AgentDir: agentDir, WorkingDir: cwd, ProjectTrusted: true})
	if err != nil {
		t.Fatal(err)
	}
	settings := runtime.Snapshot().Settings.BranchSummary
	if settings.ReserveTokensOrDefault() != 0 || settings.SkipPrompt == nil || !*settings.SkipPrompt {
		t.Fatalf("merged settings=%#v", settings)
	}
	zero, skip := uint64(0), false
	if err := runtime.SetGlobalSettings(context.Background(), func(settings *Settings) error {
		settings.BranchSummary.ReserveTokens = &zero
		settings.BranchSummary.SkipPrompt = &skip
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
	branch := root["branchSummary"].(map[string]any)
	if branch["reserveTokens"] != float64(0) || branch["skipPrompt"] != false || branch["future"] != "kept" {
		t.Fatalf("persisted branch settings=%#v", branch)
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

func TestProviderTransportSettingsParseMergeCloneAndPersistPresence(t *testing.T) {
	if got := (Settings{}).TransportOrDefault(); got != provider.TransportAuto {
		t.Fatalf("default transport = %q", got)
	}
	if got := (Settings{}).HTTPIdleTimeoutMSOrDefault(); got != 300_000 {
		t.Fatalf("default httpIdleTimeoutMs = %d", got)
	}
	if got := (ProviderRetrySettings{}).MaxRetryDelayMSOrDefault(); got != 60_000 {
		t.Fatalf("default provider maxRetryDelayMs = %d", got)
	}
	agentDir, cwd := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(agentDir, "settings.json"), `{"transport":"websocket","httpIdleTimeoutMs":111,"websocketConnectTimeoutMs":222,"retry":{"provider":{"timeoutMs":333,"maxRetries":4,"maxRetryDelayMs":555}}}`)
	writeFile(t, filepath.Join(cwd, ".pi", "settings.json"), `{"transport":"sse","httpIdleTimeoutMs":0,"retry":{"provider":{"maxRetries":0}}}`)
	runtime, err := NewRuntime(Options{AgentDir: agentDir, WorkingDir: cwd, ProjectTrusted: true})
	if err != nil {
		t.Fatal(err)
	}
	settings := runtime.Snapshot().Settings
	if settings.Transport != provider.TransportSSE || settings.HTTPIdleTimeoutMS == nil || *settings.HTTPIdleTimeoutMS != 0 ||
		settings.WebsocketConnectTimeoutMS == nil || *settings.WebsocketConnectTimeoutMS != 222 ||
		settings.Retry.Provider.TimeoutMS != nil || settings.Retry.Provider.MaxRetries == nil || *settings.Retry.Provider.MaxRetries != 0 ||
		settings.Retry.Provider.MaxRetryDelayMS != nil {
		t.Fatalf("merged provider transport settings = %#v", settings)
	}
	// Snapshot pointers are defensive clones.
	*settings.HTTPIdleTimeoutMS = 999
	*settings.Retry.Provider.MaxRetries = 999
	again := runtime.Snapshot().Settings
	if *again.HTTPIdleTimeoutMS != 0 || *again.Retry.Provider.MaxRetries != 0 {
		t.Fatalf("snapshot retained caller pointers = %#v", again)
	}
	zero := uint64(0)
	if err := runtime.SetGlobalSettings(context.Background(), func(settings *Settings) error {
		settings.Transport = provider.TransportWebsocketCached
		settings.HTTPIdleTimeoutMS = &zero
		settings.WebsocketConnectTimeoutMS = &zero
		settings.Retry.Provider.TimeoutMS = &zero
		settings.Retry.Provider.MaxRetries = &zero
		settings.Retry.Provider.MaxRetryDelayMS = &zero
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
	retry := root["retry"].(map[string]any)["provider"].(map[string]any)
	if root["transport"] != string(provider.TransportWebsocketCached) || root["httpIdleTimeoutMs"] != float64(0) || root["websocketConnectTimeoutMs"] != float64(0) ||
		retry["timeoutMs"] != float64(0) || retry["maxRetries"] != float64(0) || retry["maxRetryDelayMs"] != float64(0) {
		t.Fatalf("persisted provider transport settings = %#v", root)
	}
}

func TestProviderTransportSettingsRejectNullNegativeAndWrongTypes(t *testing.T) {
	for _, settings := range []string{
		`{"transport":null}`,
		`{"transport":""}`,
		`{"transport":"http"}`,
		`{"transport":true}`,
		`{"httpIdleTimeoutMs":null}`,
		`{"httpIdleTimeoutMs":-1}`,
		`{"websocketConnectTimeoutMs":"1"}`,
		`{"retry":[]}`,
		`{"retry":{"provider":"default"}}`,
		`{"retry":{"provider":{"timeoutMs":null}}}`,
		`{"retry":{"provider":{"maxRetries":-1}}}`,
		`{"retry":{"provider":{"maxRetryDelayMs":"1"}}}`,
	} {
		if _, _, err := newTestRuntimeNoFatal(t, "", settings, false); err == nil {
			t.Fatalf("invalid provider transport settings accepted: %s", settings)
		}
	}
}

func TestTransportSettingsMigrateLegacyWebsocketsExactly(t *testing.T) {
	for _, testCase := range []struct {
		name, settings string
		want           provider.Transport
		keepLegacy     bool
	}{
		{name: "legacy false selects sse", settings: `{"websockets":false,"future":1}`, want: provider.TransportSSE},
		{name: "legacy true selects websocket", settings: `{"websockets":true}`, want: provider.TransportWebsocket},
		{name: "explicit transport wins", settings: `{"transport":"auto","websockets":false}`, want: provider.TransportAuto, keepLegacy: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			runtime, agentDir, _ := newTestRuntime(t, "", testCase.settings, false)
			if got := runtime.Snapshot().Settings.TransportOrDefault(); got != testCase.want {
				t.Fatalf("transport = %q, want %q", got, testCase.want)
			}
			if err := runtime.SetGlobalSettings(context.Background(), func(*Settings) error { return nil }); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(agentDir, "settings.json"))
			if err != nil {
				t.Fatal(err)
			}
			var root map[string]json.RawMessage
			if err := json.Unmarshal(data, &root); err != nil {
				t.Fatal(err)
			}
			_, hasLegacy := root["websockets"]
			if hasLegacy != testCase.keepLegacy || string(root["transport"]) != fmt.Sprintf("%q", testCase.want) {
				t.Fatalf("persisted migration = %s", data)
			}
		})
	}
}

func TestRetrySettingsPreserveAbsentObjectAndNullOverlayStates(t *testing.T) {
	const global = `{"retry":{"enabled":false,"maxRetries":7,"baseDelayMs":250,"provider":{"timeoutMs":11,"maxRetries":2,"maxRetryDelayMs":33}}}`
	for _, testCase := range []struct {
		name, global, project         string
		wantEnabled                   bool
		wantMaxRetries, wantBaseDelay uint64
		wantTimeout, wantProviderMax  *uint64
		wantProviderDelay             uint64
	}{
		{name: "retry absent keeps global", project: `{}`, wantMaxRetries: 7, wantBaseDelay: 250, wantTimeout: uint64TestPointer(11), wantProviderMax: uint64TestPointer(2), wantProviderDelay: 33},
		{name: "retry empty object merges", project: `{"retry":{}}`, wantMaxRetries: 7, wantBaseDelay: 250, wantTimeout: uint64TestPointer(11), wantProviderMax: uint64TestPointer(2), wantProviderDelay: 33},
		{name: "retry null clears global", project: `{"retry":null}`, wantEnabled: true, wantMaxRetries: 3, wantBaseDelay: 2_000, wantProviderDelay: 60_000},
		{name: "provider null clears provider", project: `{"retry":{"provider":null}}`, wantMaxRetries: 7, wantBaseDelay: 250, wantProviderDelay: 60_000},
		{name: "provider empty object replaces provider", project: `{"retry":{"provider":{}}}`, wantMaxRetries: 7, wantBaseDelay: 250, wantProviderDelay: 60_000},
		{name: "provider object replaces by field", project: `{"retry":{"provider":{"maxRetries":0}}}`, wantMaxRetries: 7, wantBaseDelay: 250, wantProviderMax: uint64TestPointer(0), wantProviderDelay: 60_000},
		{name: "project object replaces global null", global: `{"retry":null}`, project: `{"retry":{"maxRetries":0}}`, wantEnabled: true, wantMaxRetries: 0, wantBaseDelay: 2_000, wantProviderDelay: 60_000},
		{name: "project empty keeps global provider null", global: `{"retry":{"enabled":false,"provider":null}}`, project: `{"retry":{}}`, wantMaxRetries: 3, wantBaseDelay: 2_000, wantProviderDelay: 60_000},
		{name: "project null over global absence", global: `{}`, project: `{"retry":null}`, wantEnabled: true, wantMaxRetries: 3, wantBaseDelay: 2_000, wantProviderDelay: 60_000},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			agentDir, cwd := t.TempDir(), t.TempDir()
			globalSettings := testCase.global
			if globalSettings == "" {
				globalSettings = global
			}
			writeFile(t, filepath.Join(agentDir, "settings.json"), globalSettings)
			writeFile(t, filepath.Join(cwd, ".pi", "settings.json"), testCase.project)
			runtime, err := NewRuntime(Options{AgentDir: agentDir, WorkingDir: cwd, ProjectTrusted: true})
			if err != nil {
				t.Fatal(err)
			}
			got := runtime.Snapshot().Settings.Retry
			if got.EnabledOrDefault() != testCase.wantEnabled || got.MaxRetriesOrDefault() != testCase.wantMaxRetries ||
				got.BaseDelayMSOrDefault() != testCase.wantBaseDelay || !reflect.DeepEqual(got.Provider.TimeoutMS, testCase.wantTimeout) ||
				!reflect.DeepEqual(got.Provider.MaxRetries, testCase.wantProviderMax) || got.Provider.MaxRetryDelayMSOrDefault() != testCase.wantProviderDelay {
				t.Fatalf("effective retry = %#v", got)
			}
		})
	}
}

func TestRetryNullStatesSurviveUnrelatedGlobalPersistence(t *testing.T) {
	for _, testCase := range []struct {
		name, settings string
		providerNull   bool
	}{
		{name: "top-level null", settings: `{"retry":null,"future":true}`},
		{name: "nested provider null", settings: `{"retry":{"enabled":false,"provider":null,"future":true}}`, providerNull: true},
		{name: "empty object", settings: `{"retry":{},"future":true}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			runtime, agentDir, _ := newTestRuntime(t, "", testCase.settings, false)
			if err := runtime.SetGlobalSettings(context.Background(), func(settings *Settings) error {
				settings.DefaultModel = "persist-only"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(agentDir, "settings.json"))
			if err != nil {
				t.Fatal(err)
			}
			var root map[string]json.RawMessage
			if err := json.Unmarshal(data, &root); err != nil {
				t.Fatal(err)
			}
			if testCase.settings == `{"retry":null,"future":true}` {
				if string(root["retry"]) != "null" {
					t.Fatalf("top-level null lost: %s", data)
				}
				return
			}
			var retry map[string]json.RawMessage
			if err := json.Unmarshal(root["retry"], &retry); err != nil {
				t.Fatal(err)
			}
			if testCase.providerNull && string(retry["provider"]) != "null" {
				t.Fatalf("provider null lost: %s", data)
			}
			if !testCase.providerNull && len(retry) != 0 {
				t.Fatalf("empty retry object lost: %s", data)
			}
		})
	}
}

func uint64TestPointer(value uint64) *uint64 { return &value }

func TestProviderRetryMigratesLegacyMaxDelayWithoutOverridingNestedValue(t *testing.T) {
	for _, testCase := range []struct {
		name, settings string
		want           uint64
	}{
		{name: "moves legacy value", settings: `{"retry":{"maxDelayMs":1234}}`, want: 1234},
		{name: "nested value wins", settings: `{"retry":{"maxDelayMs":1234,"provider":{"maxRetryDelayMs":5678}}}`, want: 5678},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			runtime, agentDir, _ := newTestRuntime(t, "", testCase.settings, false)
			value := runtime.Snapshot().Settings.Retry.Provider.MaxRetryDelayMS
			if value == nil || *value != testCase.want {
				t.Fatalf("migrated max retry delay = %#v, want %d", value, testCase.want)
			}
			if err := runtime.SetGlobalSettings(context.Background(), func(*Settings) error { return nil }); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(agentDir, "settings.json"))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(data, []byte(`"maxDelayMs"`)) || !bytes.Contains(data, []byte(`"maxRetryDelayMs"`)) {
				t.Fatalf("persisted migration = %s", data)
			}
		})
	}
}

func TestSetGlobalSettingsPreservesUnportedRetryProviderFields(t *testing.T) {
	runtime, agentDir, _ := newTestRuntime(t, "", `{"retry":{"enabled":true,"provider":{"maxRetries":9,"futureProvider":true},"future":true}}`, false)
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
		retry["future"] != true || providerSettings["maxRetries"] != float64(9) || providerSettings["futureProvider"] != true {
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
	var model Model
	for _, candidate := range builtinModels() {
		if candidate.Provider == OpenAIProviderID && candidate.ID == DefaultOpenAIModel {
			model = candidate
			break
		}
	}
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

func TestGeneratedBuiltinCatalogMatchesScopedUpstreamOracle(t *testing.T) {
	if generatedCatalogSource != "@earendil-works/pi-ai@0.83.0" {
		t.Fatalf("catalog source = %q", generatedCatalogSource)
	}
	models := builtinModels()
	if len(models) != 60 {
		t.Fatalf("builtin model count = %d, want 60", len(models))
	}
	ids := make([]string, 0, len(models))
	byID := make(map[string]Model, len(models))
	for _, model := range models {
		switch model.Provider {
		case OpenAIProviderID:
			if model.API != OpenAIResponsesAPI {
				t.Fatalf("openai model %q API = %q", model.ID, model.API)
			}
		case OpenAICodexProviderID:
			if model.API != OpenAICodexResponsesAPI {
				t.Fatalf("openai-codex model %q API = %q", model.ID, model.API)
			}
		case AnthropicProviderID:
			if model.API != AnthropicMessagesAPI {
				t.Fatalf("anthropic model %q API = %q", model.ID, model.API)
			}
		default:
			t.Fatalf("out-of-scope generated provider %q", model.Provider)
		}
		key := model.Provider + "/" + model.ID
		if _, duplicate := byID[key]; duplicate {
			t.Fatalf("duplicate generated model %q", key)
		}
		byID[key] = model
		ids = append(ids, key)
	}
	sort.Strings(ids)
	if !reflect.DeepEqual(ids, generatedCatalogModelIDs) {
		t.Fatalf("generated catalog IDs differ from oracle\n got: %v\nwant: %v", ids, generatedCatalogModelIDs)
	}
	for _, required := range []string{
		"openai/gpt-5.4", "openai/gpt-5.4-mini", "openai/gpt-5.4-nano", "openai/gpt-5.4-pro",
		"openai/gpt-5.5-pro", "openai/gpt-5.6-sol", "openai-codex/gpt-5.6-sol",
		"anthropic/claude-haiku-4-5", "anthropic/claude-opus-4-6", "anthropic/claude-opus-4-8",
		"anthropic/claude-sonnet-4-6", "anthropic/claude-fable-5",
	} {
		if _, ok := byID[required]; !ok {
			t.Fatalf("catalog is missing required upstream model %q", required)
		}
	}
	opus := byID["anthropic/claude-opus-4-8"]
	if _, present := opus.ThinkingLevelMap[provider.ThinkingOff]; present {
		t.Fatalf("Opus 4.8 must use portable off -> disabled: %#v", opus.ThinkingLevelMap)
	}
	fable := byID["anthropic/claude-fable-5"]
	if value, present := fable.ThinkingLevelMap[provider.ThinkingOff]; !present || value != nil {
		t.Fatalf("Fable 5 must explicitly omit disabled thinking: %#v", fable.ThinkingLevelMap)
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
{"providers":{"openai":{"baseUrl":"https://example.test/v1","api":"openai-responses","headers":{"X-Base":"one"},"models":[{"id":"custom","api":"openai-responses","headers":{"x-base":"two"}},]}}}`, "", false)
	s := r.Snapshot()
	if len(s.Models) != len(builtinModels())+1 {
		t.Fatalf("models = %#v", s.Models)
	}
	got, err := r.Resolve(Selection{Provider: "openai", Model: "custom"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Model.API != OpenAIResponsesAPI || got.Model.BaseURL != "https://example.test/v1" {
		t.Fatalf("custom overlay = %#v", got.Model)
	}
	if err := r.ValidateRoute(got.Model); err != nil {
		t.Fatalf("configured headers are part of the supported request path: %v", err)
	}
	if got.Model.Headers["x-base"] != "two" || len(got.Model.Headers) != 1 {
		t.Fatalf("model header precedence = %#v", got.Model.Headers)
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
	if err := r.ValidateRoute(got.Model); err != nil {
		t.Fatalf("implemented override options must be routable: %v", err)
	}
	if got.Model.Headers["x-base"] != "two" || got.Model.ContextWindow != 272_000 || got.Model.MaxTokens != 128_000 || len(got.Model.Input) != 2 {
		t.Fatalf("override erased builtin metadata: %#v", got.Model)
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

func TestRuntimeRejectsMissingModelIDAndNullModelsArray(t *testing.T) {
	for _, models := range []string{
		`{"providers":{"custom":{"api":"openai-completions","models":[{"name":"missing id"}]}}}`,
		`{"providers":{"custom":{"api":"openai-completions","models":null}}}`,
	} {
		if _, _, err := newTestRuntimeNoFatal(t, models, "", false); err == nil {
			t.Fatalf("invalid models.json was accepted: %s", models)
		} else if strings.Contains(err.Error(), "missing id") {
			t.Fatalf("diagnostic leaked model metadata: %v", err)
		}
	}
}

func TestRuntimeRejectsNullKnownModelFieldsWithoutLeakingUnknownMetadata(t *testing.T) {
	for _, models := range []string{
		`{"providers":{"custom":{"api":"openai-completions","authHeader":null,"future":"secret-value"}}}`,
		`{"providers":{"custom":{"api":"openai-completions","headers":null,"future":"secret-value"}}}`,
		`{"providers":{"custom":{"api":"openai-completions","models":[{"id":"fixture","reasoning":null,"future":"secret-value"}]}}}`,
		`{"providers":{"custom":{"api":"openai-completions","models":[{"id":"fixture","headers":null,"future":"secret-value"}]}}}`,
		`{"providers":{"custom":{"api":"openai-completions","models":[{"id":"fixture","thinkingLevelMap":null,"future":"secret-value"}]}}}`,
		`{"providers":{"custom":{"api":"openai-completions","modelOverrides":{"fixture":{"reasoning":null,"future":"secret-value"}}}}}`,
		`{"providers":{"custom":{"api":"openai-completions","modelOverrides":{"fixture":{"headers":null,"future":"secret-value"}}}}}`,
		`{"providers":{"custom":{"api":"openai-completions","modelOverrides":{"fixture":{"thinkingLevelMap":null,"future":"secret-value"}}}}}`,
	} {
		if _, _, err := newTestRuntimeNoFatal(t, models, "", false); err == nil {
			t.Fatalf("invalid known models.json field was accepted: %s", models)
		} else if strings.Contains(err.Error(), "secret-value") {
			t.Fatalf("diagnostic leaked unknown metadata: %v", err)
		}
	}
}

func TestRuntimeUnknownModelsJSONFieldsAreIgnoredLikeOpenTypeBoxObjects(t *testing.T) {
	r, _, _ := newTestRuntime(t, `{"providers":{"future":{"compat":{"token":"do-not-leak"},"models":[{"id":"ignored","api":"openai-responses"}]},"openai":{"models":[{"id":"supported","api":"openai-responses"}]}}}`, "", false)
	openAI, err := r.Resolve(Selection{Provider: "openai", Model: "supported"})
	if err != nil || r.ValidateRoute(openAI.Model) != nil {
		t.Fatalf("unselected future provider must not block openai: %#v, %v", openAI, err)
	}
	future, err := r.Resolve(Selection{Provider: "future", Model: "ignored"})
	if err != nil || r.ValidateRoute(future.Model) != nil {
		t.Fatalf("unknown compat members must be ignored on a supported route: %#v, %v", future, err)
	}
	overridden, _, err := newTestRuntimeNoFatal(t, `{"providers":{"openai":{"modelOverrides":{"custom":{"compat":{"token":"do-not-leak"}}}}}}`, "", false)
	if err != nil {
		t.Fatalf("unknown override compatibility must be ignored without leaking values: %v", err)
	}
	resolved, err := overridden.Resolve(Selection{Provider: "openai", Model: "custom"})
	if err != nil || overridden.ValidateRoute(resolved.Model) != nil || strings.Contains(fmt.Sprintf("%#v", resolved.Model.Compat), "do-not-leak") {
		t.Fatalf("unknown override changed the supported projection: %#v, %v", resolved, err)
	}
}

func TestRuntimeRejectsNullAndInvalidKnownCompatFieldsAtProviderAndModelLevel(t *testing.T) {
	testCases := []struct {
		name, api, compat string
	}{
		{name: "responses bool null", api: OpenAIResponsesAPI, compat: `{"supportsDeveloperRole":null}`},
		{name: "chat bool null", api: OpenAICompletionsAPI, compat: `{"supportsStore":null}`},
		{name: "chat literal null", api: OpenAICompletionsAPI, compat: `{"maxTokensField":null}`},
		{name: "chat literal invalid", api: OpenAICompletionsAPI, compat: `{"thinkingFormat":"future"}`},
		{name: "cache literal invalid", api: OpenAICompletionsAPI, compat: `{"cacheControlFormat":"openai"}`},
		{name: "deferred literal invalid", api: OpenAICompletionsAPI, compat: `{"deferredToolsMode":"all"}`},
		{name: "chat template descriptor null", api: OpenAICompletionsAPI, compat: `{"chatTemplateKwargs":{"effort":{"$var":null}}}`},
		{name: "openrouter nested null", api: OpenAICompletionsAPI, compat: `{"openRouterRouting":{"allow_fallbacks":null}}`},
		{name: "vercel nested type", api: OpenAICompletionsAPI, compat: `{"vercelGatewayRouting":{"only":[1]}}`},
		{name: "anthropic bool null", api: AnthropicMessagesAPI, compat: `{"forceAdaptiveThinking":null}`},
	}
	for _, level := range []string{"provider", "model"} {
		for _, testCase := range testCases {
			t.Run(level+"/"+testCase.name, func(t *testing.T) {
				providerCompat, modelCompat := "", ""
				if level == "provider" {
					providerCompat = `,"compat":` + testCase.compat
				} else {
					modelCompat = `,"compat":` + testCase.compat
				}
				models := fmt.Sprintf(`{"providers":{"fixture":{"api":%q%s,"models":[{"id":"m"%s}]}}}`, testCase.api, providerCompat, modelCompat)
				if _, _, err := newTestRuntimeNoFatal(t, models, "", false); err == nil || !strings.Contains(err.Error(), "compat") {
					t.Fatalf("NewRuntime() error = %v, want compat validation failure", err)
				}
			})
		}
	}
	// Provider compat is schema-checked before a per-model API is selected. An
	// omitted provider API must not turn a known null field into opaque metadata.
	models := `{"providers":{"fixture":{"compat":{"supportsStore":null},"models":[{"id":"m","api":"openai-completions"}]}}}`
	if _, _, err := newTestRuntimeNoFatal(t, models, "", false); err == nil || !strings.Contains(err.Error(), "compat") {
		t.Fatalf("provider compat without provider API error = %v, want schema failure", err)
	}
}

func TestRuntimeKeepsCompatObjectsOpenForUnknownKeys(t *testing.T) {
	runtime, _, _ := newTestRuntime(t, `{"providers":{"fixture":{"api":"openai-completions","compat":{"futureSetting":null},"models":[{"id":"m","compat":{"futureNested":{"secret":true}}}]}}}`, "", false)
	resolved, err := runtime.Resolve(Selection{Provider: "fixture", Model: "m"})
	if err != nil || resolved.Model.Compat.OpenAICompletions == nil {
		t.Fatalf("open compat object = %#v, %v", resolved, err)
	}
}

func TestRuntimeUnknownNestedOverrideFieldsDoNotEnterProjection(t *testing.T) {
	r, _, _ := newTestRuntime(t, `{"providers":{"openai":{"modelOverrides":{"gpt-5.5":{"future":"secret","thinkingLevelMap":{"future-level":"secret","high":"high"},"cost":{"input":7,"futureRate":999},"compat":{"futureCompat":"secret"}}}}}}`, "", false)
	resolved, err := r.Resolve(Selection{Provider: "openai", Model: "gpt-5.5"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ValidateRoute(resolved.Model); err != nil {
		t.Fatalf("unknown nested fields blocked route: %v", err)
	}
	if resolved.Model.Cost.Input != 7 || resolved.Model.ThinkingLevelMap[provider.ThinkingHigh] == nil || *resolved.Model.ThinkingLevelMap[provider.ThinkingHigh] != "high" {
		t.Fatalf("known overrides were not applied: %#v", resolved.Model)
	}
	if _, present := resolved.Model.ThinkingLevelMap[provider.ThinkingLevel("future-level")]; present || strings.Contains(fmt.Sprintf("%#v", resolved.Model), "secret") {
		t.Fatalf("unknown nested fields entered runtime projection: %#v", resolved.Model)
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
	r, _, _ := newTestRuntime(t, `{"providers":{"OpEnAi":{"modelOverrides":{"GPT-5.5":{"compat":{"supportsStore":false}},"CUSTOM":{"futureOption":"case-secret"}}}}}`, "", false)
	for _, selection := range []Selection{{Provider: "OPENAI", Model: "gPt-5.5"}, {Provider: "openai", Model: "custom"}} {
		resolved, err := r.Resolve(selection)
		if err != nil {
			t.Fatalf("resolve %#v: %v", selection, err)
		}
		err = r.ValidateRoute(resolved.Model)
		if err != nil {
			t.Fatalf("canonical supported builtin override = %v", err)
		}
		if strings.Contains(fmt.Sprintf("%#v", resolved.Model), "case-secret") {
			t.Fatalf("unknown custom override entered runtime behavior: %#v", resolved.Model)
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
	if model.Provider != OpenAIProviderID || model.ID != "custom" || model.Name != "custom" || model.API != OpenAIResponsesAPI || model.BaseURL != "https://provider.invalid/v1" {
		t.Fatalf("custom baseline = %#v", model)
	}
	if len(model.Headers) != 0 || len(model.UnsupportedFields) != 0 || len(model.UnknownFields) != 0 || strings.Contains(fmt.Sprintf("%#v", model), "aaa-secret") ||
		!model.Reasoning || len(model.Input) != 2 || model.ContextWindow != 272_000 || model.MaxTokens != 128_000 {
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
	if got := r.Snapshot().Settings.DefaultModel; got != "after" {
		t.Fatalf("post-rename snapshot was not reconciled forward: %q", got)
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
