package surfacewire

import (
	"context"
	"strings"

	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/provider"
)

type ModelListItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
}

type ModelsView struct {
	Models             map[string]string                             `json:"models"`
	ModelList          []ModelListItem                               `json:"modelList"`
	DefaultModel       *SelectedModel                                `json:"defaultModel"`
	ThinkingLevels     map[string][]provider.ThinkingLevel           `json:"thinkingLevels"`
	ThinkingLevelMaps  map[string]map[provider.ThinkingLevel]*string `json:"thinkingLevelMaps"`
	ThinkingLevelPins  map[string]provider.ThinkingLevel             `json:"thinkingLevelPins"`
	ModelError         string                                        `json:"modelError,omitempty"`
	ModelScopeWarnings []string                                      `json:"modelScopeWarnings,omitempty"`
}

func Models(ctx context.Context, api application.API, cwd string) (ModelsView, error) {
	if strings.TrimSpace(cwd) == "" {
		cwd = api.DefaultCWD()
	}
	resolved, err := application.ValidateCWD(cwd)
	if err != nil {
		return ModelsView{}, err
	}
	snapshot, err := api.ListModels(ctx, resolved)
	if err != nil {
		return ModelsView{}, err
	}
	result := ModelsView{
		Models:             make(map[string]string, len(snapshot.Models)),
		ModelList:          make([]ModelListItem, 0, len(snapshot.Models)),
		ThinkingLevels:     make(map[string][]provider.ThinkingLevel, len(snapshot.Models)),
		ThinkingLevelMaps:  make(map[string]map[provider.ThinkingLevel]*string),
		ThinkingLevelPins:  snapshot.ThinkingLevelPins,
		ModelError:         snapshot.Diagnostic,
		ModelScopeWarnings: snapshot.ModelScopeWarnings,
	}
	for _, candidate := range snapshot.Models {
		key := candidate.Provider + ":" + candidate.ID
		result.Models[key] = candidate.Name
		result.ModelList = append(result.ModelList, ModelListItem{
			ID: candidate.ID, Name: candidate.Name, Provider: candidate.Provider,
		})
		result.ThinkingLevels[key] = append([]provider.ThinkingLevel(nil), candidate.ThinkingLevels...)
		if len(candidate.ThinkingLevelMap) != 0 {
			result.ThinkingLevelMaps[key] = cloneThinkingLevelMap(candidate.ThinkingLevelMap)
		}
	}
	if snapshot.DefaultModel != nil {
		result.DefaultModel = &SelectedModel{
			Provider: snapshot.DefaultModel.Provider,
			ModelID:  snapshot.DefaultModel.ModelID,
		}
	}
	return result, nil
}

func cloneThinkingLevelMap(values map[provider.ThinkingLevel]*string) map[provider.ThinkingLevel]*string {
	result := make(map[provider.ThinkingLevel]*string, len(values))
	for level, value := range values {
		if value == nil {
			result[level] = nil
			continue
		}
		copy := *value
		result[level] = &copy
	}
	return result
}
