package provider

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

func (p *OpenAIResponsesProvider) resolveAzureResponsesTarget(model Model, options StreamOptions) (string, string, error) {
	if p == nil || p.dialect != responsesDialectAzure {
		return "", "", fmt.Errorf("%w: Azure Responses provider is not configured", ErrInvalidAzureOpenAIResponsesConfig)
	}
	apiVersion := firstResponsesValue(
		options.AzureAPIVersion,
		options.Env["AZURE_OPENAI_API_VERSION"],
		p.azure.apiVersion,
		defaultAzureOpenAPIVersion,
	)
	baseURL := firstResponsesValue(
		options.AzureBaseURL,
		options.Env["AZURE_OPENAI_BASE_URL"],
		p.azure.baseURL,
	)
	resourceName := firstResponsesValue(
		options.AzureResourceName,
		options.Env["AZURE_OPENAI_RESOURCE_NAME"],
		p.azure.resourceName,
	)
	if baseURL == "" && resourceName != "" {
		baseURL = "https://" + resourceName + ".openai.azure.com/openai/v1"
	}
	if baseURL == "" {
		baseURL = strings.TrimSpace(model.BaseURL())
	}
	if baseURL == "" {
		return "", "", fmt.Errorf(
			"%w: Azure OpenAI base URL is required; set AZURE_OPENAI_BASE_URL or AZURE_OPENAI_RESOURCE_NAME",
			ErrInvalidAzureOpenAIResponsesConfig,
		)
	}
	normalized, err := normalizeAzureOpenAIBaseURL(baseURL)
	if err != nil {
		return "", "", err
	}
	endpoint, err := azureOpenAIResponsesEndpoint(normalized, apiVersion)
	if err != nil {
		return "", "", err
	}
	deploymentName := firstResponsesValue(options.AzureDeploymentName, azureDeploymentForModel(options.Env["AZURE_OPENAI_DEPLOYMENT_NAME_MAP"], model.ID()), p.azure.deploymentName, model.ID())
	if deploymentName == "" {
		return "", "", fmt.Errorf("%w: Azure deployment name is empty", ErrInvalidAzureOpenAIResponsesConfig)
	}
	return endpoint, deploymentName, nil
}

func firstResponsesValue(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func azureDeploymentForModel(value, modelID string) string {
	for _, entry := range strings.Split(value, ",") {
		parts := strings.SplitN(strings.TrimSpace(entry), "=", 2)
		if len(parts) != 2 {
			continue
		}
		if strings.TrimSpace(parts[0]) == modelID && strings.TrimSpace(parts[1]) != "" {
			return strings.TrimSpace(parts[1])
		}
	}
	return ""
}

func normalizeAzureOpenAIBaseURL(rawBaseURL string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(rawBaseURL), "/")
	if !utf8.ValidString(trimmed) || trimmed == "" {
		return "", fmt.Errorf("%w: invalid Azure OpenAI base URL", ErrInvalidAzureOpenAIResponsesConfig)
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return "", fmt.Errorf("%w: invalid Azure OpenAI base URL %q", ErrInvalidAzureOpenAIResponsesConfig, rawBaseURL)
	}
	hostname := strings.ToLower(parsed.Hostname())
	isAzureHost := strings.HasSuffix(hostname, ".openai.azure.com") || strings.HasSuffix(hostname, ".cognitiveservices.azure.com") || strings.HasSuffix(hostname, ".ai.azure.com")
	normalizedPath := strings.TrimRight(parsed.Path, "/")
	if isAzureHost && (normalizedPath == "" || normalizedPath == "/" || normalizedPath == "/openai" || normalizedPath == "/openai/v1/responses") {
		parsed.Path = "/openai/v1"
		parsed.RawPath = ""
		parsed.RawQuery = ""
		parsed.ForceQuery = false
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func azureOpenAIResponsesEndpoint(baseURL, apiVersion string) (string, error) {
	if !utf8.ValidString(apiVersion) || strings.TrimSpace(apiVersion) == "" || strings.ContainsFunc(apiVersion, unicode.IsControl) {
		return "", fmt.Errorf("%w: Azure API version is invalid", ErrInvalidAzureOpenAIResponsesConfig)
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("%w: invalid Azure OpenAI base URL", ErrInvalidAzureOpenAIResponsesConfig)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/responses"
	parsed.RawPath = ""
	query := parsed.Query()
	query.Set("api-version", strings.TrimSpace(apiVersion))
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func encodeAzureOpenAIResponsesRequest(request Request, systemRole, deploymentName string) ([]byte, error) {
	raw, err := encodeOpenAIResponsesRequest(request, systemRole)
	if err != nil {
		return nil, err
	}
	var payload responsesRequestPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("%w: decode shared request JSON: %w", ErrOpenAIResponsesRequest, err)
	}
	payload.Model = deploymentName
	payload.PromptCacheKey = clampOpenAIPromptCacheKey(request.StreamOptions().SessionID)
	payload.PromptCacheTTL = ""
	payload.PromptCacheMode = nil
	payload.ServiceTier = ""
	payload.ToolChoice = nil
	raw, err = json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("%w: encode Azure request JSON: %w", ErrOpenAIResponsesRequest, err)
	}
	return raw, nil
}
