package application

import (
	"context"
	"slices"
	"strings"

	modelcatalog "github.com/cat3399/pi-go/internal/model"
)

// ModelPresetRequest contains only identity and route hints. Looking up a
// typed ID never needs a valid endpoint, credentials or a network request.
type ModelPresetRequest struct {
	ProviderID string             `json:"providerId"`
	Models     []ModelPresetQuery `json:"models"`
}

type ModelPresetQuery struct {
	ID      string `json:"id"`
	API     string `json:"api"`
	BaseURL string `json:"baseUrl"`
}

// LookupModelPresets returns one draft per query, in order. Unknown or
// ambiguous IDs have no suggested fields.
func (s *Service) LookupModelPresets(ctx context.Context, request ModelPresetRequest) ([]DiscoveredModel, error) {
	if err := context.Cause(normalizeContext(ctx)); err != nil {
		return nil, err
	}
	index := newModelPresetIndex(modelcatalog.LoadBuiltinModels(s.paths.AgentDir))
	result := make([]DiscoveredModel, 0, len(request.Models))
	for _, q := range request.Models {
		result = append(result, index.lookup(strings.TrimSpace(q.ID), strings.TrimSpace(request.ProviderID), catalogHostname(q.BaseURL), q.API))
	}
	return result, nil
}

type modelPresetIndex map[string][]*modelcatalog.Model

func newModelPresetIndex(builtins []modelcatalog.Model) modelPresetIndex {
	// Gateways may advertise a literal or provider-qualified ID. Neither
	// form changes the actual ID or route that the user is configuring.
	index := make(modelPresetIndex, 2*len(builtins))
	for i := range builtins {
		m := &builtins[i]
		index[m.ID] = append(index[m.ID], m)
		qualified := m.Provider + "/" + m.ID
		index[qualified] = append(index[qualified], m)
	}
	return index
}

func (index modelPresetIndex) lookup(id, providerID, host, api string) DiscoveredModel {
	// A shared ID may have different metadata on OpenAI, Azure, Codex or a
	// hosted provider. Don't break ties by catalog iteration order.
	var best *modelcatalog.Model
	bestRank := -1
	for _, m := range index[id] {
		rank := 0
		switch {
		case m.Provider == providerID:
			rank = 5
		case host != "" && host == catalogHostname(m.BaseURL):
			rank = 4
		case id == m.Provider+"/"+m.ID:
			rank = 3
		case m.API == api:
			rank = 2
		case api == modelcatalog.OpenAICompletionsAPI && m.Provider == modelcatalog.OpenAIProviderID:
			// Ordinary OpenAI models are catalogued under Responses even when
			// a gateway exposes their IDs via Chat Completions.
			rank = 1
		}
		if rank > bestRank {
			best, bestRank = m, rank
		} else if rank == bestRank {
			best = nil
		}
	}
	if best == nil {
		return DiscoveredModel{ID: id}
	}
	return DiscoveredModel{ID: id, Name: best.Name, Reasoning: boolPointer(best.Reasoning),
		ThinkingLevelMap: cloneApplicationThinkingMap(best.ThinkingLevelMap), Input: slices.Clone(best.Input),
		ContextWindow: best.ContextWindow, MaxTokens: best.MaxTokens}
}
