package surfacewire

import (
	"context"
	"strings"

	"github.com/cat3399/pi-go/internal/application"
)

type ModelProviderInfo struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Configured     bool   `json:"configured"`
	Source         string `json:"source,omitempty"`
	CredentialType string `json:"credentialType,omitempty"`
	ModelCount     int    `json:"modelCount"`
	SupportsAPIKey bool   `json:"supportsApiKey"`
	SupportsOAuth  bool   `json:"supportsOAuth"`
	OAuthName      string `json:"oauthName,omitempty"`
	Builtin        bool   `json:"builtin"`
	Enabled        bool   `json:"enabled"`
}

type ProviderModelInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	API           string `json:"api"`
	ContextWindow uint64 `json:"contextWindow"`
	MaxTokens     uint64 `json:"maxTokens"`
	Reasoning     bool   `json:"reasoning"`
	ImageInput    bool   `json:"imageInput"`
}

func ModelProviders(ctx context.Context, api application.API, cwd string) ([]ModelProviderInfo, error) {
	if strings.TrimSpace(cwd) == "" {
		cwd = api.DefaultCWD()
	}
	values, err := api.ListModelProviders(ctx, cwd)
	if err != nil {
		return nil, err
	}
	result := make([]ModelProviderInfo, 0, len(values))
	for _, value := range values {
		result = append(result, ModelProviderInfo{ID: value.ID, Name: value.Name, Configured: value.Configured,
			Source: value.Source, CredentialType: value.CredentialType, ModelCount: value.ModelCount,
			SupportsAPIKey: value.SupportsAPIKey, SupportsOAuth: value.SupportsOAuth, OAuthName: value.OAuthName,
			Builtin: value.Builtin, Enabled: value.Enabled})
	}
	return result, nil
}

func ProviderModels(ctx context.Context, api application.API, cwd, providerID string) ([]ProviderModelInfo, error) {
	if strings.TrimSpace(cwd) == "" {
		cwd = api.DefaultCWD()
	}
	values, err := api.ListProviderModels(ctx, cwd, providerID)
	if err != nil {
		return nil, err
	}
	result := make([]ProviderModelInfo, 0, len(values))
	for _, value := range values {
		result = append(result, ProviderModelInfo{ID: value.ID, Name: value.Name, API: value.API,
			ContextWindow: value.ContextWindow, MaxTokens: value.MaxTokens, Reasoning: value.Reasoning, ImageInput: value.ImageInput})
	}
	return result, nil
}
