package app

import (
	"fmt"
	"path/filepath"
	"strings"
)

type openAIModelConfig struct {
	apiKey  *string
	baseURL string
}

func loadOpenAIModelConfig(agentDir string) (openAIModelConfig, error) {
	path := filepath.Join(agentDir, "models.json")
	root, exists, err := readConfigObject(path, "models.json", true, false)
	if err != nil || !exists {
		return openAIModelConfig{}, err
	}
	rawProviders, present := root["providers"]
	if !present {
		return openAIModelConfig{}, fmt.Errorf(
			"%w: models.json requires a providers object",
			ErrInvalidProductionConfig,
		)
	}
	providers, ok := rawProviders.(map[string]any)
	if !ok {
		return openAIModelConfig{}, fmt.Errorf(
			"%w: models.json providers must be an object",
			ErrInvalidProductionConfig,
		)
	}
	rawOpenAI, selected := providers[openAIProviderID]
	if !selected {
		return openAIModelConfig{}, nil
	}
	openAI, ok := rawOpenAI.(map[string]any)
	if !ok {
		return openAIModelConfig{}, fmt.Errorf(
			"%w: models.json OpenAI provider must be an object",
			ErrInvalidProductionConfig,
		)
	}

	acceptedFields := map[string]struct{}{
		"name":    {},
		"baseUrl": {},
		"apiKey":  {},
		"api":     {},
	}
	for field := range openAI {
		if _, accepted := acceptedFields[field]; !accepted {
			return openAIModelConfig{}, fmt.Errorf(
				"%w: models.json OpenAI provider contains a field outside the migrated projection",
				ErrUnsupportedProductionValue,
			)
		}
	}

	if rawName, present := openAI["name"]; present {
		name, ok := rawName.(string)
		if !ok || strings.TrimSpace(name) == "" {
			return openAIModelConfig{}, fmt.Errorf(
				"%w: models.json providers.openai.name must be a non-empty string",
				ErrInvalidProductionConfig,
			)
		}
	}
	if rawAPI, present := openAI["api"]; present {
		api, ok := rawAPI.(string)
		if !ok || strings.TrimSpace(api) == "" {
			return openAIModelConfig{}, fmt.Errorf(
				"%w: models.json providers.openai.api must be a non-empty string",
				ErrInvalidProductionConfig,
			)
		}
		if api != openAIResponsesAPI {
			return openAIModelConfig{}, fmt.Errorf(
				"%w: models.json OpenAI API is not supported by this assembly",
				ErrUnsupportedProductionValue,
			)
		}
	}

	var result openAIModelConfig
	if rawAPIKey, present := openAI["apiKey"]; present {
		apiKey, ok := rawAPIKey.(string)
		if !ok || apiKey == "" {
			return openAIModelConfig{}, fmt.Errorf(
				"%w: models.json providers.openai.apiKey must be a non-empty string",
				ErrInvalidProductionConfig,
			)
		}
		result.apiKey = &apiKey
	}
	if rawBaseURL, present := openAI["baseUrl"]; present {
		baseURL, ok := rawBaseURL.(string)
		if !ok || strings.TrimSpace(baseURL) == "" {
			return openAIModelConfig{}, fmt.Errorf(
				"%w: models.json providers.openai.baseUrl must be a non-empty string",
				ErrInvalidProductionConfig,
			)
		}
		result.baseURL = baseURL
	}
	if result.apiKey == nil && result.baseURL == "" {
		return openAIModelConfig{}, fmt.Errorf(
			"%w: models.json OpenAI provider does not contain a migrated setting",
			ErrUnsupportedProductionValue,
		)
	}
	return result, nil
}
