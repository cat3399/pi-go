package model_test

import (
	"encoding/json"
	"testing"

	"github.com/cat3399/pi-go/internal/model"
)

func TestResponsesWebSocketCapabilityProviderAndModelOverrides(t *testing.T) {
	_, models, err := model.ParseProviderDraft("gateway", json.RawMessage(`{
		"api":"openai-responses",
		"baseUrl":"http://localhost:8317/v1",
		"compat":{"supportsWebsockets":true},
		"models":[{"id":"inherited"},{"id":"disabled","compat":{"supportsWebsockets":false}}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %v", models)
	}
	for _, item := range models {
		selected, err := item.Ref()
		if err != nil {
			t.Fatal(err)
		}
		compat := selected.Compat().OpenAIResponses
		want := item.ID == "inherited"
		if compat == nil || compat.SupportsWebsockets == nil || *compat.SupportsWebsockets != want {
			t.Fatalf("%s capability = %#v", item.ID, compat)
		}
		*compat.SupportsWebsockets = !want
		if *selected.Compat().OpenAIResponses.SupportsWebsockets != want {
			t.Fatal("capability snapshot aliases model state")
		}
	}
}

func TestResponsesWebSocketCapabilityRejectsMalformedValues(t *testing.T) {
	for _, value := range []string{`"true"`, `null`, `1`, `{}`} {
		_, _, err := model.ParseProviderDraft("gateway", json.RawMessage(`{"api":"openai-responses","baseUrl":"http://localhost:8317/v1","compat":{"supportsWebsockets":`+value+`},"models":[{"id":"model"}]}`))
		if err == nil {
			t.Fatalf("accepted invalid capability %s", value)
		}
	}
}
