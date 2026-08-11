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
// generator. It contains only providers whose API dialects are registered by
// this assembly.
//
//go:embed catalogdata/*.json
var generatedCatalogData embed.FS

type catalogOracle struct {
	Provider, SHA256 string
	APIs             []string
	Count            int
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
		if err := json.Unmarshal(raw, &groups); err != nil || len(groups) != len(oracle.APIs) {
			panic(fmt.Sprintf("decode generated catalog %s: %v", file, err))
		}
		count := 0
		for _, api := range oracle.APIs {
			entries, ok := groups[api]
			if !ok {
				panic("generated catalog is missing API " + api + " for " + oracle.Provider)
			}
			count += len(entries)
			for id, wire := range entries {
				if id != wire.ID || wire.Provider != oracle.Provider || wire.API != api {
					panic("generated catalog identity mismatch for " + oracle.Provider + "/" + id)
				}
				compat := provider.ModelCompat{}
				if len(wire.Compat) != 0 {
					compat, err = decodeCompat(wire.Compat, oracle.Provider+"/"+id, api)
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
		if count != oracle.Count {
			panic(fmt.Sprintf("generated catalog %s has %d models, want %d", file, count, oracle.Count))
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
