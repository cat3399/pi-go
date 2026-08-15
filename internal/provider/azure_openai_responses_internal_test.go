package provider

import (
	"net/url"
	"testing"
)

func TestNormalizeAzureOpenAIBaseURLMatchesPinnedPi(t *testing.T) {
	tests := map[string]string{
		"https://resource.cognitiveservices.azure.com":               "https://resource.cognitiveservices.azure.com/openai/v1",
		"https://resource.ai.azure.com":                              "https://resource.ai.azure.com/openai/v1",
		"https://resource.openai.azure.com":                          "https://resource.openai.azure.com/openai/v1",
		"https://resource.cognitiveservices.azure.com/openai":        "https://resource.cognitiveservices.azure.com/openai/v1",
		"https://resource.cognitiveservices.azure.com/openai/v1":     "https://resource.cognitiveservices.azure.com/openai/v1",
		"https://resource.services.ai.azure.com/openai/v1/responses": "https://resource.services.ai.azure.com/openai/v1",
		"https://resource.openai.azure.com/openai?api-version=old":   "https://resource.openai.azure.com/openai/v1",
		"https://proxy.example.com/v1?custom=true":                   "https://proxy.example.com/v1?custom=true",
	}
	for input, want := range tests {
		got, err := normalizeAzureOpenAIBaseURL(input)
		if err != nil || got != want {
			t.Errorf("normalizeAzureOpenAIBaseURL(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := normalizeAzureOpenAIBaseURL("not-a-url"); err == nil {
		t.Fatal("invalid Azure base URL was accepted")
	}
}

func TestAzureOpenAIResponsesTargetUsesTypedEnvironmentAndModelFallbacks(t *testing.T) {
	implementation, err := NewAzureOpenAIResponsesProvider(AzureOpenAIResponsesConfig{APIKey: "key", BaseURL: "https://config.openai.azure.com", APIVersion: "config-version", DeploymentName: "config-deployment"})
	if err != nil {
		t.Fatal(err)
	}
	model := mustParityOptionModel(t, ModelSpec{Provider: AzureOpenAIProviderID, API: AzureOpenAIResponsesAPI, ID: "gpt-model", BaseURL: "https://model.openai.azure.com"})
	options := StreamOptions{
		AzureBaseURL: "https://typed.openai.azure.com", AzureAPIVersion: "typed-version", AzureDeploymentName: "typed-deployment",
		Env: map[string]string{
			"AZURE_OPENAI_BASE_URL":            "https://env.openai.azure.com",
			"AZURE_OPENAI_API_VERSION":         "env-version",
			"AZURE_OPENAI_DEPLOYMENT_NAME_MAP": "gpt-model=env-deployment",
		},
	}
	endpoint, deployment, err := implementation.resolveAzureResponsesTarget(model, options)
	if err != nil || endpoint != "https://typed.openai.azure.com/openai/v1/responses?api-version=typed-version" || deployment != "typed-deployment" {
		t.Fatalf("typed target = %q, %q, %v", endpoint, deployment, err)
	}

	options.AzureBaseURL, options.AzureAPIVersion, options.AzureDeploymentName = "", "", ""
	endpoint, deployment, err = implementation.resolveAzureResponsesTarget(model, options)
	if err != nil || endpoint != "https://env.openai.azure.com/openai/v1/responses?api-version=env-version" || deployment != "env-deployment" {
		t.Fatalf("environment target = %q, %q, %v", endpoint, deployment, err)
	}

	implementation.azure = azureOpenAIResponsesDefaults{}
	options.Env = nil
	endpoint, deployment, err = implementation.resolveAzureResponsesTarget(model, options)
	if err != nil || endpoint != "https://model.openai.azure.com/openai/v1/responses?api-version=v1" || deployment != "gpt-model" {
		t.Fatalf("model target = %q, %q, %v", endpoint, deployment, err)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Query().Get("api-version") != defaultAzureOpenAPIVersion {
		t.Fatalf("default api-version = %q, %v", endpoint, err)
	}
}

func TestAzureOpenAIResponsesTargetBuildsResourceURLAndRequiresConfiguration(t *testing.T) {
	implementation, err := NewAzureOpenAIResponsesProvider(AzureOpenAIResponsesConfig{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	model := mustParityOptionModel(t, ModelSpec{Provider: AzureOpenAIProviderID, API: AzureOpenAIResponsesAPI, ID: "gpt-model"})
	endpoint, deployment, err := implementation.resolveAzureResponsesTarget(model, StreamOptions{Env: map[string]string{"AZURE_OPENAI_RESOURCE_NAME": "resource"}})
	if err != nil || endpoint != "https://resource.openai.azure.com/openai/v1/responses?api-version=v1" || deployment != "gpt-model" {
		t.Fatalf("resource target = %q, %q, %v", endpoint, deployment, err)
	}
	if _, _, err := implementation.resolveAzureResponsesTarget(model, StreamOptions{}); err == nil {
		t.Fatal("missing Azure endpoint configuration was accepted")
	}
}
