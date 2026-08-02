package app

import (
	"fmt"
	"path/filepath"
)

type storedOpenAICredential struct {
	key         string
	environment map[string]string
}

func loadStoredOpenAICredential(agentDir string) (storedOpenAICredential, bool, error) {
	path := filepath.Join(agentDir, "auth.json")
	root, exists, err := readConfigObject(path, "auth.json", false, true)
	if err != nil || !exists {
		return storedOpenAICredential{}, false, err
	}
	rawCredential, selected := root[openAIProviderID]
	if !selected {
		return storedOpenAICredential{}, false, nil
	}
	credential, ok := rawCredential.(map[string]any)
	if !ok {
		return storedOpenAICredential{}, false, fmt.Errorf(
			"%w: auth.json OpenAI credential must be an object",
			ErrInvalidProductionConfig,
		)
	}
	typeName, ok := credential["type"].(string)
	if !ok || typeName == "" {
		return storedOpenAICredential{}, false, fmt.Errorf(
			"%w: auth.json OpenAI credential requires a type",
			ErrInvalidProductionConfig,
		)
	}
	if typeName != "api_key" {
		return storedOpenAICredential{}, false, fmt.Errorf(
			"%w: auth.json OpenAI credential type is not migrated",
			ErrUnsupportedProductionValue,
		)
	}
	key, ok := credential["key"].(string)
	if !ok {
		return storedOpenAICredential{}, false, fmt.Errorf(
			"%w: auth.json OpenAI API-key credential requires a string key",
			ErrInvalidProductionConfig,
		)
	}

	providerEnvironment := make(map[string]string)
	if rawEnvironment, present := credential["env"]; present {
		environmentObject, ok := rawEnvironment.(map[string]any)
		if !ok {
			return storedOpenAICredential{}, false, fmt.Errorf(
				"%w: auth.json OpenAI credential env must be an object",
				ErrInvalidProductionConfig,
			)
		}
		for name, rawValue := range environmentObject {
			value, ok := rawValue.(string)
			if !ok {
				return storedOpenAICredential{}, false, fmt.Errorf(
					"%w: auth.json OpenAI credential env values must be strings",
					ErrInvalidProductionConfig,
				)
			}
			providerEnvironment[name] = value
		}
	}
	return storedOpenAICredential{key: key, environment: providerEnvironment}, true, nil
}
