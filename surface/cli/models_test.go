package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/model"
	"github.com/cat3399/pi-go/internal/model/catalog"
)

func TestModelsUpdateReplayReloadAndFailurePreserveUserData(t *testing.T) {
	agentDir := t.TempDir()
	files := map[string]string{
		"models.json":       `{"providers":{"openai":{"apiKey":"fixture-key","modelOverrides":{"gpt-5.4":{"contextWindow":45000,"cost":{"input":0.25}}}}}}`,
		"settings.json":     `{"defaultProvider":"openai","defaultModel":"gpt-5.4"}`,
		"auth.json":         `{"openai":{"type":"api_key","key":"fixture-auth"}}`,
		"models-store.json": `{"fixture":"must remain unchanged"}`,
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(agentDir, name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	source, err := filepath.Abs("../../internal/model/testdata/catalog-sync.json")
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := model.NewRuntime(model.Options{AgentDir: agentDir})
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"update", "--source", source, "--agent-dir", agentDir}
	var stdout, stderr bytes.Buffer
	if code := RunModels(context.Background(), args, &stdout, &stderr); code != 0 {
		t.Fatalf("update failed (%d): %s", code, stderr.String())
	}
	path := filepath.Join(agentDir, catalog.Filename)
	doc, err := catalog.Read(path)
	if err != nil || doc.Sources.Models.Name != "models.dev" {
		t.Fatalf("no models.dev catalog published: %v", err)
	}
	if err := runtime.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	m, ok := runtime.GetModel("openai", "gpt-5.4")
	if !ok || m.ContextWindow != 45000 || m.Cost.Input != 0.25 || m.Cost.Output != 15 ||
		m.Compat.OpenAIResponses == nil || m.Compat.OpenAIResponses.SupportsToolSearch == nil || !*m.Compat.OpenAIResponses.SupportsToolSearch {
		t.Fatalf("source facts, local controls or user precedence lost: %#v", m)
	}
	selected, err := runtime.Resolve(model.Selection{})
	if err != nil || selected.Model.ID != "gpt-5.4" {
		t.Fatalf("user default changed: %#v, %v", selected, err)
	}
	if _, exists := runtime.GetModel("openai", "gpt-4.1"); exists {
		t.Fatal("removed model resurrected from embedded catalog")
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := RunModels(context.Background(), args, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "Already up to date") {
		t.Fatalf("identical replay was not a no-op (%d): %s / %s", code, stdout.String(), stderr.String())
	}
	invalid := filepath.Join(t.TempDir(), "invalid.json")
	if err := os.WriteFile(invalid, []byte(`{"openai":{"id":"openai","models":{}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := RunModels(context.Background(), []string{"update", "--source", invalid, "--agent-dir", agentDir}, &stdout, &stderr); code != 1 {
		t.Fatalf("partial source accepted: %d / %s", code, stderr.String())
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed update replaced the last healthy catalog")
	}
	for name, contents := range files {
		raw, err := os.ReadFile(filepath.Join(agentDir, name))
		if err != nil || string(raw) != contents {
			t.Fatalf("user file %s changed", name)
		}
	}
}

func TestModelsHelpAndRemovedVersionOption(t *testing.T) {
	for _, test := range []struct {
		args []string
		code int
	}{
		{nil, 0}, {[]string{"update", "--help"}, 0},
		{[]string{"update", "--version", "1.4.0"}, 2},
		{[]string{"update", "--source"}, 2}, {[]string{"update", "extra"}, 2},
	} {
		var stdout, stderr bytes.Buffer
		if code := RunModels(context.Background(), test.args, &stdout, &stderr); code != test.code {
			t.Fatalf("%v: code = %d; stderr = %s", test.args, code, stderr.String())
		}
	}
}
