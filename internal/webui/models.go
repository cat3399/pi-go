package webui

import (
	"fmt"
	"sort"
	"strings"

	modelcatalog "github.com/cat3399/pi-go/internal/model"
	"github.com/cat3399/pi-go/internal/provider"
)

type modelListItemWire struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
}

type modelsWire struct {
	Models             map[string]string                             `json:"models"`
	ModelList          []modelListItemWire                           `json:"modelList"`
	DefaultModel       *selectedModelWire                            `json:"defaultModel"`
	ThinkingLevels     map[string][]provider.ThinkingLevel           `json:"thinkingLevels"`
	ThinkingLevelMaps  map[string]map[provider.ThinkingLevel]*string `json:"thinkingLevelMaps"`
	ThinkingLevelPins  map[string]provider.ThinkingLevel             `json:"thinkingLevelPins"`
	ModelScopeWarnings []string                                      `json:"modelScopeWarnings,omitempty"`
}

func (s *Supervisor) Models(cwd string) (modelsWire, error) {
	if strings.TrimSpace(cwd) == "" {
		cwd = s.paths.WorkingDir
	}
	resolved, err := validateCWD(cwd)
	if err != nil {
		return modelsWire{}, err
	}
	runtime, err := modelcatalog.NewRuntime(modelcatalog.Options{
		AgentDir: s.paths.AgentDir, WorkingDir: resolved, ProjectTrusted: false,
	})
	if err != nil {
		return modelsWire{}, fmt.Errorf("load model catalog: %w", err)
	}
	snapshot := runtime.Snapshot()
	visible := append([]modelcatalog.Model(nil), snapshot.Models...)
	pins := make(map[string]provider.ThinkingLevel)
	warnings := []string{}
	if len(snapshot.Settings.EnabledModels) != 0 {
		scope := modelcatalog.ResolveModelScope(snapshot.Settings.EnabledModels, snapshot.Models)
		visible = make([]modelcatalog.Model, 0, len(scope.ScopedModels))
		for _, scoped := range scope.ScopedModels {
			visible = append(visible, scoped.Model)
			if scoped.ThinkingLevel != nil {
				pins[scoped.Model.Provider+"/"+scoped.Model.ID] = *scoped.ThinkingLevel
			}
		}
		for _, diagnostic := range scope.Diagnostics {
			warnings = append(warnings, diagnostic.Message)
		}
	}
	sort.SliceStable(visible, func(left, right int) bool {
		leftName := strings.ToLower(visible[left].Name)
		rightName := strings.ToLower(visible[right].Name)
		if leftName != rightName {
			return leftName < rightName
		}
		if visible[left].Provider != visible[right].Provider {
			return visible[left].Provider < visible[right].Provider
		}
		return visible[left].ID < visible[right].ID
	})
	result := modelsWire{
		Models: make(map[string]string, len(visible)), ModelList: make([]modelListItemWire, 0, len(visible)),
		ThinkingLevels:    make(map[string][]provider.ThinkingLevel, len(visible)),
		ThinkingLevelMaps: make(map[string]map[provider.ThinkingLevel]*string),
		ThinkingLevelPins: pins, ModelScopeWarnings: warnings,
	}
	for _, candidate := range visible {
		key := candidate.Provider + ":" + candidate.ID
		result.Models[key] = candidate.Name
		result.ModelList = append(result.ModelList, modelListItemWire{ID: candidate.ID, Name: candidate.Name, Provider: candidate.Provider})
		ref, refErr := candidate.Ref()
		if refErr == nil {
			result.ThinkingLevels[key] = ref.SupportedThinkingLevels()
		}
		if len(candidate.ThinkingLevelMap) != 0 {
			result.ThinkingLevelMaps[key] = cloneThinkingLevelMap(candidate.ThinkingLevelMap)
		}
		if result.DefaultModel == nil && candidate.Provider == snapshot.Settings.DefaultProvider && candidate.ID == snapshot.Settings.DefaultModel {
			result.DefaultModel = &selectedModelWire{Provider: candidate.Provider, ModelID: candidate.ID}
		}
	}
	if result.DefaultModel == nil && len(visible) != 0 {
		result.DefaultModel = &selectedModelWire{Provider: visible[0].Provider, ModelID: visible[0].ID}
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
