package model

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/cat3399/pi-go/internal/model/catalog"
)

func installedTestCatalog(t *testing.T) catalog.Document {
	t.Helper()
	doc, err := catalog.Decode(embeddedCatalogJSON)
	if err != nil {
		t.Fatal(err)
	}
	doc.Defaults = []ProviderDefault{{Provider: OpenAIProviderID, ModelID: "z-synced"}}
	for i := range doc.Providers {
		if doc.Providers[i].ID != OpenAIProviderID {
			continue
		}
		doc.Providers[i].API = OpenAIResponsesAPI
		doc.Providers[i].BaseURL = "https://catalog.invalid/v1"
		doc.Providers[i].Models = map[string]map[string]json.RawMessage{OpenAIResponsesAPI: {
			"a-synced": json.RawMessage(`{"id":"a-synced","provider":"openai","api":"openai-responses","name":"A","baseUrl":"https://catalog.invalid/v1","input":["text"],"contextWindow":10000,"maxTokens":1000,"cost":{"input":1}}`),
			"z-synced": json.RawMessage(`{"id":"z-synced","provider":"openai","api":"openai-responses","name":"Z","baseUrl":"https://catalog.invalid/v1","input":["text"],"contextWindow":20000,"maxTokens":2000,"cost":{"input":2},"futureMetadata":{"keep":true}}`),
		}}
	}
	return doc
}

func publishTestCatalog(t *testing.T, path string, doc catalog.Document) {
	t.Helper()
	if _, err := catalog.Write(context.Background(), path, doc); err != nil {
		t.Fatal(err)
	}
}

func TestInstalledCatalogRemainsBelowUserConfiguration(t *testing.T) {
	agentDir := t.TempDir()
	path := filepath.Join(agentDir, catalog.Filename)
	doc := installedTestCatalog(t)
	publishTestCatalog(t, path, doc)
	userModels := `{"providers":{"openai":{"baseUrl":"https://user.invalid/v1","apiKey":"user-key","models":[{"id":"user-only","contextWindow":40000,"maxTokens":4000}],"modelOverrides":{"z-synced":{"contextWindow":90000,"cost":{"input":0.25}}}}}}`
	userSettings := `{"defaultProvider":"openai","defaultModel":"user-only"}`
	writeFile(t, filepath.Join(agentDir, "models.json"), userModels)
	writeFile(t, filepath.Join(agentDir, "settings.json"), userSettings)
	r, err := NewRuntime(Options{AgentDir: agentDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Error(); err != nil {
		t.Fatal(err)
	}
	assertUserPriority := func() {
		t.Helper()
		m, ok := r.GetModel(OpenAIProviderID, "z-synced")
		if !ok || m.ContextWindow != 90000 || m.Cost.Input != 0.25 || m.BaseURL != "https://user.invalid/v1" {
			t.Fatalf("user override lost: %#v", m)
		}
		if _, ok := r.GetModel(OpenAIProviderID, "user-only"); !ok {
			t.Fatal("user model was lost")
		}
		selected, err := r.Resolve(Selection{})
		if err != nil || selected.Model.ID != "user-only" {
			t.Fatalf("user default lost: %#v, %v", selected, err)
		}
		p, _ := r.Provider(OpenAIProviderID)
		if p.ConfiguredAPIKey == nil || *p.ConfiguredAPIKey != "user-key" {
			t.Fatal("user credential was changed")
		}
	}
	assertUserPriority()
	for _, m := range builtinModels() {
		if m.Provider == OpenAIProviderID {
			if _, ok := r.GetModel(m.Provider, m.ID); ok {
				t.Fatalf("removed embedded model was resurrected: %s", m.ID)
			}
		}
	}
	previous := r.Snapshot()
	for i := range doc.Providers {
		if doc.Providers[i].ID == OpenAIProviderID {
			doc.Providers[i].Models[OpenAIResponsesAPI]["z-synced"] = json.RawMessage(`{"id":"z-synced","provider":"openai","api":"openai-responses","name":"Z updated","baseUrl":"https://new-catalog.invalid/v1","input":["text"],"contextWindow":30000,"maxTokens":3000,"cost":{"input":3}}`)
		}
	}
	publishTestCatalog(t, path, doc)
	if err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertUserPriority()
	updated, _ := r.GetModel(OpenAIProviderID, "z-synced")
	if updated.Name != "Z updated" {
		t.Fatal("new built-in metadata was not loaded")
	}
	for _, m := range previous.Models {
		if m.ID == "z-synced" && m.Name != "Z" {
			t.Fatal("a published snapshot was mutated")
		}
	}
	for name, expected := range map[string]string{"models.json": userModels, "settings.json": userSettings} {
		raw, err := os.ReadFile(filepath.Join(agentDir, name))
		if err != nil || string(raw) != expected {
			t.Fatalf("%s changed during built-in reload", name)
		}
	}
	if err := r.SetGlobalSettings(context.Background(), func(s *Settings) error { s.Theme = "dark"; return nil }); err != nil {
		t.Fatal(err)
	}
	assertUserPriority()
	updated, _ = r.GetModel(OpenAIProviderID, "z-synced")
	if updated.Name != "Z updated" {
		t.Fatal("settings mutation restored embedded metadata")
	}
}

func TestInstalledCatalogDefaultsDraftAndRecovery(t *testing.T) {
	agentDir := t.TempDir()
	path := filepath.Join(agentDir, catalog.Filename)
	doc := installedTestCatalog(t)
	publishTestCatalog(t, path, doc)
	r, err := NewRuntime(Options{AgentDir: agentDir})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := r.Resolve(Selection{})
	if err != nil || selected.Model.ID != "z-synced" {
		t.Fatalf("installed default ignored: %#v, %v", selected, err)
	}
	selected, err = r.Resolve(Selection{Provider: OpenAIProviderID, Model: "unknown-model"})
	if err != nil || selected.Model.ContextWindow != 20000 {
		t.Fatalf("custom ID did not inherit installed provider default: %#v, %v", selected, err)
	}
	_, draftModels, err := r.ParseProviderDraft(OpenAIProviderID, json.RawMessage(`{"modelOverrides":{"z-synced":{"name":"Draft"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range draftModels {
		if m.ID == "z-synced" {
			found = m.Name == "Draft" && m.ContextWindow == 20000
		}
	}
	if !found {
		t.Fatal("draft did not use the installed catalog")
	}
	snapshot := r.Snapshot()
	snapshot.ProviderDefaults[0].ModelID = "mutated"
	if r.Snapshot().ProviderDefaults[0].ModelID != "z-synced" {
		t.Fatal("snapshot exposed preference storage")
	}
	writeFile(t, path, `{"schemaVersion":999}`)
	if err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.ModelSourceError() == nil {
		t.Fatal("invalid installed catalog had no diagnostic")
	}
	if _, ok := r.GetModel(OpenAIProviderID, "z-synced"); !ok {
		t.Fatal("invalid catalog discarded the last healthy snapshot")
	}
	firstStartup, err := NewRuntime(Options{AgentDir: agentDir})
	if err != nil {
		t.Fatal(err)
	}
	if firstStartup.ModelSourceError() == nil || len(firstStartup.Snapshot().Models) != len(builtinModels()) {
		t.Fatal("invalid catalog did not use the embedded startup fallback")
	}
	publishTestCatalog(t, path, doc)
	if err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Error(); err != nil {
		t.Fatalf("catalog did not recover: %v", err)
	}
}
