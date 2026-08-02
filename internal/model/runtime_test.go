package model

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

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
	if got.Model.API != OpenAIResponsesAPI || got.Model.BaseURL != "https://example.test/v1" || got.Model.Headers["x-base"] != "two" {
		t.Fatalf("custom overlay = %#v", got.Model)
	}
	if _, err := r.Resolve(Selection{Provider: "openai", Model: "not-listed"}); err != nil {
		t.Fatalf("explicit custom model: %v", err)
	}
}

func TestRuntimeModelOverrideDoesNotEraseBuiltinMetadata(t *testing.T) {
	r, _, _ := newTestRuntime(t, `{"providers":{"openai":{"headers":{"X-Base":"one"},"modelOverrides":{"gpt-5.5":{"name":"renamed","reasoning":true,"headers":{"x-base":"two"}}}}}}`, "", false)
	got, err := r.Resolve(Selection{Model: "gpt-5.5"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Model.Name != "renamed" || !got.Model.Reasoning || got.Model.API != OpenAIResponsesAPI || got.Model.Headers["x-base"] != "two" {
		t.Fatalf("override = %#v", got.Model)
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
	r, agent, cwd := newTestRuntime(t, "", `{"defaultProvider":"openai","defaultModel":"gpt-5.5","unknown":{"keep":1}}`, false)
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
	if err := r.SetGlobalSettings(context.Background(), func(s *Settings) error { s.EnabledModels = []string{"openai/gpt-5.5"}; return nil }); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(agent, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"unknown"`) {
		t.Fatalf("unknown setting lost: %s", b)
	}
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
