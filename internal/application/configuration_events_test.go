package application

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cat3399/pi-go/internal/app"
)

func TestConfigurationWritesInvalidateModelsAfterPublication(t *testing.T) {
	s, err := NewService(ServiceOptions{Production: app.ProductionConfig{WorkingDir: t.TempDir(), AgentDir: t.TempDir(), Environment: []string{}}, DisableReaper: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	initial := s.CurrentCursor()
	empty := []string{}
	subscription, err := s.SubscribeReadEvents(ReadEventOptions{After: &initial, Sessions: &empty})
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	document := ModelsConfigDocument{"providers": json.RawMessage(`{"qa":{"api":"openai-completions","baseUrl":"http://127.0.0.1:1/v1","models":[{"id":"qa-model"}]}}`)}
	if err := s.WriteModelsConfig(context.Background(), document); err != nil {
		t.Fatal(err)
	}
	assertChanged := func() {
		t.Helper()
		select {
		case event := <-subscription.Events:
			value, ok := event.Value.(ConfigurationResourceEvent)
			if !ok || value.CWD != "" || !value.Models || value.Resources {
				t.Fatalf("configuration event = %#v", event)
			}
		default:
			t.Fatal("published configuration was not invalidated")
		}
	}
	assertChanged()
	if _, err := s.ReadModelsConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProviderAPIKey(context.Background(), "qa", "test-key"); err != nil {
		t.Fatal(err)
	}
	assertChanged()
	models, err := s.ListModels(context.Background(), s.DefaultCWD())
	if err != nil || len(models.Models) != 1 || models.Models[0].ID != "qa-model" {
		t.Fatalf("published models = %#v, %v", models, err)
	}
	if err := s.DeleteProviderCredential(context.Background(), "qa", "api_key"); err != nil {
		t.Fatal(err)
	}
	assertChanged()
	before := s.CurrentCursor()
	if err := s.WriteModelsConfig(context.Background(), ModelsConfigDocument{"providers": json.RawMessage(`[]`)}); err == nil {
		t.Fatal("invalid config succeeded")
	}
	if s.CurrentCursor() != before {
		t.Fatal("failed validation invalidated unrelated caches")
	}
	if len(s.activeSessions()) != 0 {
		t.Fatal("model directory write/read opened an Agent Runtime")
	}
}
