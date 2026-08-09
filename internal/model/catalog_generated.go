package model

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/cat3399/pi-go/internal/provider"
)

//go:generate go run ./cataloggen

// generatedCatalogData is the scoped JSON emitted by pi-ai's catalog
// generator. Only the four API dialects registered by this assembly are
// present (OpenAI Responses, OpenAI Codex Responses, Anthropic Messages; Chat
// Completions remains user/provider configured because there is no extra
// built-in provider in scope).
//
//go:embed catalogdata/*.json
var generatedCatalogData embed.FS

type catalogOracle struct {
	Provider, API, SHA256 string
	Count                 int
}

type generatedCatalogModel struct {
	ID               string                             `json:"id"`
	Name             string                             `json:"name"`
	API              string                             `json:"api"`
	Provider         string                             `json:"provider"`
	BaseURL          string                             `json:"baseUrl"`
	Reasoning        bool                               `json:"reasoning"`
	ThinkingLevelMap map[provider.ThinkingLevel]*string `json:"thinkingLevelMap"`
	Input            []provider.InputKind               `json:"input"`
	Cost             provider.CostRates                 `json:"cost"`
	ContextWindow    uint64                             `json:"contextWindow"`
	MaxTokens        uint64                             `json:"maxTokens"`
	Compat           json.RawMessage                    `json:"compat"`
}

func generatedBuiltinModels() []Model {
	models := make([]Model, 0, len(generatedCatalogModelIDs))
	for file, oracle := range generatedCatalogOracle {
		raw, err := generatedCatalogData.ReadFile("catalogdata/" + file)
		if err != nil {
			panic(fmt.Sprintf("read generated catalog %s: %v", file, err))
		}
		digest := sha256.Sum256(raw)
		if hex.EncodeToString(digest[:]) != oracle.SHA256 {
			panic("generated catalog oracle mismatch for " + file)
		}
		var groups map[string]map[string]generatedCatalogModel
		if err := json.Unmarshal(raw, &groups); err != nil || len(groups) != 1 {
			panic(fmt.Sprintf("decode generated catalog %s: %v", file, err))
		}
		entries := groups[oracle.API]
		if len(entries) != oracle.Count {
			panic(fmt.Sprintf("generated catalog %s has %d models, want %d", file, len(entries), oracle.Count))
		}
		for id, wire := range entries {
			if id != wire.ID || wire.Provider != oracle.Provider || wire.API != oracle.API {
				panic("generated catalog identity mismatch for " + oracle.Provider + "/" + id)
			}
			compat := provider.ModelCompat{}
			if len(wire.Compat) != 0 {
				compat, err = decodeCompat(wire.Compat, oracle.Provider+"/"+id, oracle.API)
				if err != nil {
					panic(fmt.Sprintf("decode generated compat for %s/%s: %v", oracle.Provider, id, err))
				}
			}
			models = append(models, Model{
				Provider: wire.Provider, ID: wire.ID, Name: wire.Name, API: wire.API, BaseURL: wire.BaseURL,
				Reasoning: wire.Reasoning, ThinkingLevelMap: wire.ThinkingLevelMap, Input: wire.Input, Cost: wire.Cost,
				ContextWindow: wire.ContextWindow, MaxTokens: wire.MaxTokens, Compat: compat,
			})
		}
	}
	sort.Slice(models, func(left, right int) bool {
		if models[left].Provider != models[right].Provider {
			return models[left].Provider < models[right].Provider
		}
		return models[left].ID < models[right].ID
	})
	return models
}
