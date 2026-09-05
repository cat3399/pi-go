package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cat3399/pi-go/internal/app"
	modelcatalog "github.com/cat3399/pi-go/internal/model"
)

type ModelProbeResult struct {
	LatencyMS    int64
	Status       int
	ResponseText string
}

func (s *Service) TestModel(
	ctx context.Context,
	providerName string,
	providerDraft json.RawMessage,
	modelDraft json.RawMessage,
) (ModelProbeResult, error) {
	providerName = strings.TrimSpace(providerName)
	if providerName == "" {
		return ModelProbeResult{}, errors.New("providerName is required")
	}
	var providerObject map[string]json.RawMessage
	if err := json.Unmarshal(providerDraft, &providerObject); err != nil || providerObject == nil {
		return ModelProbeResult{}, errors.New("provider is required")
	}
	var modelObject map[string]json.RawMessage
	if err := json.Unmarshal(modelDraft, &modelObject); err != nil || modelObject == nil {
		return ModelProbeResult{}, errors.New("model is required")
	}
	var modelID string
	if err := json.Unmarshal(modelObject["id"], &modelID); err != nil || strings.TrimSpace(modelID) == "" {
		return ModelProbeResult{}, errors.New("Model ID is required")
	}
	modelID = strings.TrimSpace(modelID)
	encodedModels, err := json.Marshal([]json.RawMessage{modelDraft})
	if err != nil {
		return ModelProbeResult{}, err
	}
	providerObject = cloneRawObject(providerObject)
	providerObject["models"] = encodedModels
	encodedProvider, err := json.Marshal(providerObject)
	if err != nil {
		return ModelProbeResult{}, err
	}
	runtime, err := s.openModels(ctx, s.paths.WorkingDir)
	if err != nil {
		return ModelProbeResult{}, err
	}
	configured, models, err := runtime.ParseProviderDraft(providerName, encodedProvider)
	if err != nil {
		return ModelProbeResult{}, err
	}
	var selected modelcatalog.Model
	for _, candidate := range models {
		if candidate.ID == modelID {
			selected = candidate
			break
		}
	}
	if selected.ID == "" {
		return ModelProbeResult{}, fmt.Errorf("model not found: %s/%s", providerName, modelID)
	}
	ref, err := selected.Ref()
	if err != nil {
		return ModelProbeResult{}, err
	}
	probeContext, cancel := context.WithTimeout(normalizeContext(ctx), 20*time.Second)
	defer cancel()
	config := cloneProductionConfig(s.production)
	config.AgentDir = s.paths.AgentDir
	config.WorkingDir = s.paths.WorkingDir
	result, err := app.ProbeProductionModel(probeContext, config, configured, ref)
	projection := ModelProbeResult{
		LatencyMS: result.Latency.Milliseconds(), Status: result.Status, ResponseText: result.ResponseText,
	}
	return projection, err
}

func cloneRawObject(source map[string]json.RawMessage) map[string]json.RawMessage {
	result := make(map[string]json.RawMessage, len(source))
	for key, value := range source {
		result[key] = append(json.RawMessage(nil), value...)
	}
	return result
}
