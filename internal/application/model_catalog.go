package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	modelCatalogTTL      = time.Hour
	modelCatalogTimeout  = 15 * time.Second
	maxModelCatalogBytes = 64 << 20
	modelCatalogMinShare = 0.6
)

type ModelCatalogCost struct {
	Input      *float64 `json:"input,omitempty"`
	Output     *float64 `json:"output,omitempty"`
	CacheRead  *float64 `json:"cacheRead,omitempty"`
	CacheWrite *float64 `json:"cacheWrite,omitempty"`
}

type ModelCatalogEntry struct {
	Key             string           `json:"key"`
	ProviderID      string           `json:"providerId"`
	ProviderName    string           `json:"providerName"`
	ProviderBaseURL string           `json:"providerBaseUrl,omitempty"`
	ID              string           `json:"id"`
	Name            string           `json:"name"`
	Reasoning       *bool            `json:"reasoning,omitempty"`
	Input           []string         `json:"input,omitempty"`
	ContextWindow   *uint64          `json:"contextWindow,omitempty"`
	MaxTokens       *uint64          `json:"maxTokens,omitempty"`
	Cost            ModelCatalogCost `json:"cost"`
}

type ModelCatalogPreset struct {
	Name          string            `json:"name,omitempty"`
	Reasoning     *bool             `json:"reasoning,omitempty"`
	Input         []string          `json:"input,omitempty"`
	ContextWindow *uint64           `json:"contextWindow,omitempty"`
	MaxTokens     *uint64           `json:"maxTokens,omitempty"`
	Cost          *ModelCatalogCost `json:"cost,omitempty"`
}

type ModelCatalogPriceRecommendation struct {
	Status       string            `json:"status"`
	Method       string            `json:"method,omitempty"`
	Reason       string            `json:"reason,omitempty"`
	Cost         *ModelCatalogCost `json:"cost,omitempty"`
	ProviderID   string            `json:"providerId,omitempty"`
	ProviderName string            `json:"providerName,omitempty"`
	Support      int               `json:"support"`
	Total        int               `json:"total"`
}

type ModelCatalogRecommendation struct {
	ExactMatches        int                             `json:"exactMatches"`
	MetadataMethod      string                          `json:"metadataMethod"`
	MatchedProviderID   string                          `json:"matchedProviderId,omitempty"`
	MatchedProviderName string                          `json:"matchedProviderName,omitempty"`
	Preset              ModelCatalogPreset              `json:"preset"`
	Price               ModelCatalogPriceRecommendation `json:"price"`
}

type ModelCatalogResult struct {
	Models         []ModelCatalogEntry
	Recommendation ModelCatalogRecommendation
	Source         string
}

func (s *Service) QueryModelCatalog(ctx context.Context, query, providerHint, baseURL string, limit int) (ModelCatalogResult, error) {
	query = truncateRunes(query, 120)
	providerHint = truncateRunes(providerHint, 120)
	baseURL = truncateRunes(baseURL, 500)
	entries, err := s.loadModelCatalog(normalizeContext(ctx))
	if err != nil {
		return ModelCatalogResult{}, err
	}
	return ModelCatalogResult{
		Models:         searchModelCatalog(entries, query, providerHint, limit),
		Recommendation: recommendModelCatalogPreset(entries, query, providerHint, baseURL),
		Source:         s.modelCatalogURL,
	}, nil
}

func (s *Service) loadModelCatalog(ctx context.Context) ([]ModelCatalogEntry, error) {
	s.modelCatalogMu.Lock()
	defer s.modelCatalogMu.Unlock()
	now := time.Now()
	if len(s.modelCatalogEntries) != 0 && now.Before(s.modelCatalogExpires) {
		return cloneModelCatalogEntries(s.modelCatalogEntries), nil
	}
	requestContext, cancel := context.WithTimeout(ctx, modelCatalogTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, s.modelCatalogURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := s.modelHTTP.Do(request)
	if err != nil {
		if len(s.modelCatalogEntries) != 0 {
			return cloneModelCatalogEntries(s.modelCatalogEntries), nil
		}
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		if len(s.modelCatalogEntries) != 0 {
			return cloneModelCatalogEntries(s.modelCatalogEntries), nil
		}
		return nil, fmt.Errorf("models.dev returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxModelCatalogBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxModelCatalogBytes {
		return nil, errors.New("models.dev catalog is too large")
	}
	var payload any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, errors.New("models.dev returned invalid JSON")
	}
	entries := flattenModelsDevCatalog(payload)
	if len(entries) == 0 {
		return nil, errors.New("models.dev returned an empty catalog")
	}
	s.modelCatalogEntries = cloneModelCatalogEntries(entries)
	s.modelCatalogExpires = now.Add(modelCatalogTTL)
	return entries, nil
}

func flattenModelsDevCatalog(payload any) []ModelCatalogEntry {
	root, ok := payload.(map[string]any)
	if !ok {
		return nil
	}
	providerIDs := sortedObjectKeys(root)
	entries := make([]ModelCatalogEntry, 0)
	for _, providerID := range providerIDs {
		rawProvider, ok := root[providerID].(map[string]any)
		if !ok {
			continue
		}
		rawModels, ok := rawProvider["models"].(map[string]any)
		if !ok {
			continue
		}
		providerName := cleanCatalogString(rawProvider["name"])
		if providerName == "" {
			providerName = providerID
		}
		providerBaseURL := cleanCatalogString(rawProvider["api"])
		for _, fallbackID := range sortedObjectKeys(rawModels) {
			rawModel, ok := rawModels[fallbackID].(map[string]any)
			if !ok {
				continue
			}
			id := cleanCatalogString(rawModel["id"])
			if id == "" {
				id = fallbackID
			}
			if id == "" {
				continue
			}
			name := cleanCatalogString(rawModel["name"])
			if name == "" {
				name = id
			}
			entry := ModelCatalogEntry{
				Key: providerID + "/" + id, ProviderID: providerID, ProviderName: providerName,
				ProviderBaseURL: providerBaseURL, ID: id, Name: name, Cost: readModelCatalogCost(rawModel["cost"]),
			}
			if reasoning, ok := rawModel["reasoning"].(bool); ok {
				entry.Reasoning = boolPointer(reasoning)
			}
			entry.Input = readCatalogInput(rawModel["modalities"])
			if limit, ok := rawModel["limit"].(map[string]any); ok {
				entry.ContextWindow = positiveUint64(limit["context"])
				entry.MaxTokens = positiveUint64(limit["output"])
			}
			entries = append(entries, entry)
		}
	}
	return entries
}

func recommendModelCatalogPreset(entries []ModelCatalogEntry, query, providerHint, baseURL string) ModelCatalogRecommendation {
	exact := make([]ModelCatalogEntry, 0)
	for _, entry := range entries {
		if exactCatalogModelMatch(entry, query) {
			exact = append(exact, entry)
		}
	}
	if len(exact) == 0 {
		return ModelCatalogRecommendation{
			MetadataMethod: "none", Preset: ModelCatalogPreset{},
			Price: ModelCatalogPriceRecommendation{Status: "unreliable", Reason: "no-exact-match"},
		}
	}
	providerEntries := filterCatalogEntries(exact, func(entry ModelCatalogEntry) bool { return catalogProviderMatches(entry, providerHint) })
	baseEntries := filterCatalogEntries(exact, func(entry ModelCatalogEntry) bool { return catalogBaseURLMatches(entry, baseURL) })
	method := "consensus"
	var metadataEntry *ModelCatalogEntry
	if len(providerEntries) != 0 {
		method = "provider"
		metadataEntry = &providerEntries[0]
	} else if len(baseEntries) != 0 {
		method = "base-url"
		metadataEntry = &baseEntries[0]
	}
	preset := consensusCatalogMetadata(exact)
	matchedID, matchedName := "", ""
	if metadataEntry != nil {
		preset = catalogMetadataFromEntry(*metadataEntry)
		matchedID, matchedName = metadataEntry.ProviderID, metadataEntry.ProviderName
	}
	var price ModelCatalogPriceRecommendation
	if entry, ok := firstPricedCatalogEntry(providerEntries); ok {
		price = catalogPriceFromEntry(entry, "provider")
	} else if entry, ok := firstPricedCatalogEntry(baseEntries); ok {
		price = catalogPriceFromEntry(entry, "base-url")
	} else {
		price = consensusCatalogPrice(exact)
	}
	if price.Status == "reliable" && price.Cost != nil {
		cost := cloneModelCatalogCost(*price.Cost)
		preset.Cost = &cost
	}
	return ModelCatalogRecommendation{
		ExactMatches: len(exact), MetadataMethod: method,
		MatchedProviderID: matchedID, MatchedProviderName: matchedName,
		Preset: preset, Price: price,
	}
}

func searchModelCatalog(entries []ModelCatalogEntry, query, providerHint string, limit int) []ModelCatalogEntry {
	query = strings.ToLower(strings.TrimSpace(query))
	providerHint = strings.ToLower(strings.TrimSpace(providerHint))
	if limit <= 0 {
		limit = 50
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 100 {
		limit = 100
	}
	type rankedEntry struct {
		entry ModelCatalogEntry
		rank  float64
	}
	ranked := make([]rankedEntry, 0, len(entries))
	for _, entry := range entries {
		rank := catalogMatchRank(entry, query, providerHint)
		if query == "" || rank < 20 {
			ranked = append(ranked, rankedEntry{entry: entry, rank: rank})
		}
	}
	sort.SliceStable(ranked, func(left, right int) bool {
		if ranked[left].rank != ranked[right].rank {
			return ranked[left].rank < ranked[right].rank
		}
		leftEntry, rightEntry := ranked[left].entry, ranked[right].entry
		if value := strings.Compare(strings.ToLower(leftEntry.ProviderName), strings.ToLower(rightEntry.ProviderName)); value != 0 {
			return value < 0
		}
		if value := strings.Compare(strings.ToLower(leftEntry.Name), strings.ToLower(rightEntry.Name)); value != 0 {
			return value < 0
		}
		return strings.ToLower(leftEntry.ID) < strings.ToLower(rightEntry.ID)
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	result := make([]ModelCatalogEntry, 0, len(ranked))
	for _, item := range ranked {
		result = append(result, cloneModelCatalogEntry(item.entry))
	}
	return result
}

func catalogMatchRank(entry ModelCatalogEntry, query, providerHint string) float64 {
	id, name := strings.ToLower(entry.ID), strings.ToLower(entry.Name)
	providerID, providerName := strings.ToLower(entry.ProviderID), strings.ToLower(entry.ProviderName)
	fullID := providerID + "/" + id
	rank := 20.0
	switch {
	case query == "":
		rank = 10
	case id == query || fullID == query:
		rank = 0
	case name == query:
		rank = 1
	case strings.HasPrefix(id, query) || strings.HasPrefix(name, query):
		rank = 2
	case strings.HasPrefix(fullID, query) || providerID == query || providerName == query:
		rank = 3
	case strings.Contains(id, query) || strings.Contains(name, query):
		rank = 4
	case strings.Contains(fullID, query) || strings.Contains(providerName, query):
		rank = 5
	}
	if rank < 20 && providerHint != "" && (providerID == providerHint || providerName == providerHint) {
		rank -= 0.5
	}
	return rank
}

func exactCatalogModelMatch(entry ModelCatalogEntry, query string) bool {
	normalizedQuery := normalizeCatalogModelID(query)
	if normalizedQuery == "" {
		return false
	}
	normalizedID := normalizeCatalogModelID(entry.ID)
	return normalizedID == normalizedQuery || strings.ToLower(entry.ProviderID)+"/"+normalizedID == normalizedQuery
}

func catalogProviderMatches(entry ModelCatalogEntry, hint string) bool {
	normalized := normalizeCatalogProvider(hint)
	return normalized != "" && (normalizeCatalogProvider(entry.ProviderID) == normalized || normalizeCatalogProvider(entry.ProviderName) == normalized)
}

func catalogBaseURLMatches(entry ModelCatalogEntry, baseURL string) bool {
	actual := catalogHostname(baseURL)
	if actual == "" {
		return false
	}
	known := map[string][]string{
		"anthropic": {"api.anthropic.com"}, "google": {"generativelanguage.googleapis.com"},
		"openai": {"api.openai.com"}, "openrouter": {"openrouter.ai"},
	}
	candidates := append([]string(nil), known[normalizeCatalogProvider(entry.ProviderID)]...)
	if providerHost := catalogHostname(entry.ProviderBaseURL); providerHost != "" {
		candidates = append(candidates, providerHost)
	}
	for _, candidate := range candidates {
		if actual == candidate || strings.HasSuffix(actual, "."+candidate) {
			return true
		}
	}
	return false
}

func catalogMetadataFromEntry(entry ModelCatalogEntry) ModelCatalogPreset {
	return ModelCatalogPreset{
		Name: entry.Name, Reasoning: cloneBoolPointer(entry.Reasoning), Input: append([]string(nil), entry.Input...),
		ContextWindow: cloneUint64Pointer(entry.ContextWindow), MaxTokens: cloneUint64Pointer(entry.MaxTokens),
	}
}

func consensusCatalogMetadata(entries []ModelCatalogEntry) ModelCatalogPreset {
	total := len(entries)
	names := make([]string, 0, total)
	reasoning := make([]bool, 0, total)
	inputs := make([][]string, 0, total)
	contexts, outputs := make([]uint64, 0, total), make([]uint64, 0, total)
	for _, entry := range entries {
		names = append(names, entry.Name)
		if entry.Reasoning != nil {
			reasoning = append(reasoning, *entry.Reasoning)
		}
		if len(entry.Input) != 0 {
			inputs = append(inputs, entry.Input)
		}
		if entry.ContextWindow != nil {
			contexts = append(contexts, *entry.ContextWindow)
		}
		if entry.MaxTokens != nil {
			outputs = append(outputs, *entry.MaxTokens)
		}
	}
	preset := ModelCatalogPreset{}
	if value, ok := catalogMode(names, total, func(value string) string { return strings.ToLower(value) }); ok {
		preset.Name = value
	}
	if value, ok := catalogMode(reasoning, total, strconv.FormatBool); ok {
		preset.Reasoning = boolPointer(value)
	}
	if value, ok := catalogMode(inputs, total, func(value []string) string {
		copy := append([]string(nil), value...)
		sort.Strings(copy)
		return strings.Join(copy, ",")
	}); ok {
		preset.Input = append([]string(nil), value...)
	}
	if value, ok := catalogMode(contexts, total, func(value uint64) string { return strconv.FormatUint(value, 10) }); ok {
		preset.ContextWindow = uint64Pointer(value)
	}
	if value, ok := catalogMode(outputs, total, func(value uint64) string { return strconv.FormatUint(value, 10) }); ok {
		preset.MaxTokens = uint64Pointer(value)
	}
	return preset
}

func catalogMode[T any](values []T, total int, key func(T) string) (T, bool) {
	var zero T
	if len(values) == 0 || total <= 0 {
		return zero, false
	}
	type group struct {
		value T
		count int
	}
	groups := make(map[string]group)
	for _, value := range values {
		item := groups[key(value)]
		item.value = value
		item.count++
		groups[key(value)] = item
	}
	ranked := make([]group, 0, len(groups))
	for _, item := range groups {
		ranked = append(ranked, item)
	}
	sort.SliceStable(ranked, func(left, right int) bool { return ranked[left].count > ranked[right].count })
	if len(ranked) == 0 || float64(ranked[0].count)/float64(total) < modelCatalogMinShare || len(ranked) > 1 && ranked[1].count == ranked[0].count {
		return zero, false
	}
	return ranked[0].value, true
}

func consensusCatalogPrice(entries []ModelCatalogEntry) ModelCatalogPriceRecommendation {
	priced := make([]ModelCatalogEntry, 0)
	for _, entry := range entries {
		if catalogEntryHasPrice(entry) {
			priced = append(priced, entry)
		}
	}
	if len(priced) == 0 {
		return ModelCatalogPriceRecommendation{Status: "unreliable", Reason: "no-valid-price"}
	}
	if len(priced) == 1 {
		return ModelCatalogPriceRecommendation{Status: "unreliable", Reason: "insufficient-support", Support: 1, Total: 1}
	}
	groups := make(map[string][]ModelCatalogEntry)
	for _, entry := range priced {
		key := fmt.Sprintf("%.17g/%.17g", *entry.Cost.Input, *entry.Cost.Output)
		groups[key] = append(groups[key], entry)
	}
	ranked := make([][]ModelCatalogEntry, 0, len(groups))
	for _, group := range groups {
		ranked = append(ranked, group)
	}
	sort.SliceStable(ranked, func(left, right int) bool { return len(ranked[left]) > len(ranked[right]) })
	winner := ranked[0]
	if len(ranked) > 1 && len(ranked[1]) == len(winner) || float64(len(winner))/float64(len(priced)) < modelCatalogMinShare {
		return ModelCatalogPriceRecommendation{Status: "unreliable", Reason: "conflict", Support: len(winner), Total: len(priced)}
	}
	cost := ModelCatalogCost{Input: float64Pointer(*winner[0].Cost.Input), Output: float64Pointer(*winner[0].Cost.Output)}
	cacheReads, cacheWrites := make([]float64, 0), make([]float64, 0)
	for _, entry := range winner {
		if entry.Cost.CacheRead != nil {
			cacheReads = append(cacheReads, *entry.Cost.CacheRead)
		}
		if entry.Cost.CacheWrite != nil {
			cacheWrites = append(cacheWrites, *entry.Cost.CacheWrite)
		}
	}
	if value, ok := numericCatalogMode(cacheReads); ok {
		cost.CacheRead = float64Pointer(value)
	}
	if value, ok := numericCatalogMode(cacheWrites); ok {
		cost.CacheWrite = float64Pointer(value)
	}
	return ModelCatalogPriceRecommendation{Status: "reliable", Method: "consensus", Cost: &cost, Support: len(winner), Total: len(priced)}
}

func numericCatalogMode(values []float64) (float64, bool) {
	if len(values) == 0 {
		return 0, false
	}
	groups := make(map[float64]int)
	for _, value := range values {
		groups[value]++
	}
	type pair struct {
		value float64
		count int
	}
	ranked := make([]pair, 0, len(groups))
	for value, count := range groups {
		ranked = append(ranked, pair{value, count})
	}
	sort.SliceStable(ranked, func(left, right int) bool { return ranked[left].count > ranked[right].count })
	if len(ranked) > 1 && ranked[0].count == ranked[1].count {
		return 0, false
	}
	return ranked[0].value, true
}

func catalogPriceFromEntry(entry ModelCatalogEntry, method string) ModelCatalogPriceRecommendation {
	cost := cloneModelCatalogCost(entry.Cost)
	return ModelCatalogPriceRecommendation{
		Status: "reliable", Method: method, Cost: &cost,
		ProviderID: entry.ProviderID, ProviderName: entry.ProviderName, Support: 1, Total: 1,
	}
}

func firstPricedCatalogEntry(entries []ModelCatalogEntry) (ModelCatalogEntry, bool) {
	for _, entry := range entries {
		if catalogEntryHasPrice(entry) {
			return entry, true
		}
	}
	return ModelCatalogEntry{}, false
}

func catalogEntryHasPrice(entry ModelCatalogEntry) bool {
	return entry.Cost.Input != nil && entry.Cost.Output != nil
}

func readModelCatalogCost(value any) ModelCatalogCost {
	object, _ := value.(map[string]any)
	if object == nil {
		return ModelCatalogCost{}
	}
	return ModelCatalogCost{
		Input: nonNegativeFloat(object["input"]), Output: nonNegativeFloat(object["output"]),
		CacheRead: nonNegativeFloat(object["cache_read"]), CacheWrite: nonNegativeFloat(object["cache_write"]),
	}
}

func readCatalogInput(value any) []string {
	object, _ := value.(map[string]any)
	values, _ := object["input"].([]any)
	seen := make(map[string]struct{})
	result := make([]string, 0, len(values))
	for _, raw := range values {
		text, _ := raw.(string)
		text = strings.ToLower(strings.TrimSpace(text))
		if text != "text" && text != "image" {
			continue
		}
		if _, exists := seen[text]; exists {
			continue
		}
		seen[text] = struct{}{}
		result = append(result, text)
	}
	return result
}

func nonNegativeFloat(value any) *float64 {
	number, ok := value.(float64)
	if !ok || number < 0 {
		return nil
	}
	return float64Pointer(number)
}

func positiveUint64(value any) *uint64 {
	number, ok := value.(float64)
	if !ok || number <= 0 || number > float64(^uint64(0)) || number != float64(uint64(number)) {
		return nil
	}
	return uint64Pointer(uint64(number))
}

func normalizeCatalogProvider(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsDigit(r) || r >= 'a' && r <= 'z' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return unicode.ToLower(r)
		}
		return -1
	}, strings.TrimSpace(value))
}

func normalizeCatalogModelID(value string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(value)), "models/")
}

func catalogHostname(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
}

func filterCatalogEntries(entries []ModelCatalogEntry, keep func(ModelCatalogEntry) bool) []ModelCatalogEntry {
	result := make([]ModelCatalogEntry, 0)
	for _, entry := range entries {
		if keep(entry) {
			result = append(result, entry)
		}
	}
	return result
}

func cleanCatalogString(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func sortedObjectKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func truncateRunes(value string, maximum int) string {
	runes := []rune(value)
	if len(runes) > maximum {
		runes = runes[:maximum]
	}
	return string(runes)
}

func cloneModelCatalogEntries(entries []ModelCatalogEntry) []ModelCatalogEntry {
	result := make([]ModelCatalogEntry, len(entries))
	for index, entry := range entries {
		result[index] = cloneModelCatalogEntry(entry)
	}
	return result
}

func cloneModelCatalogEntry(entry ModelCatalogEntry) ModelCatalogEntry {
	entry.Reasoning = cloneBoolPointer(entry.Reasoning)
	entry.Input = append([]string(nil), entry.Input...)
	entry.ContextWindow = cloneUint64Pointer(entry.ContextWindow)
	entry.MaxTokens = cloneUint64Pointer(entry.MaxTokens)
	entry.Cost = cloneModelCatalogCost(entry.Cost)
	return entry
}

func cloneModelCatalogCost(cost ModelCatalogCost) ModelCatalogCost {
	return ModelCatalogCost{
		Input: cloneFloat64Pointer(cost.Input), Output: cloneFloat64Pointer(cost.Output),
		CacheRead: cloneFloat64Pointer(cost.CacheRead), CacheWrite: cloneFloat64Pointer(cost.CacheWrite),
	}
}

func boolPointer(value bool) *bool          { return &value }
func uint64Pointer(value uint64) *uint64    { return &value }
func float64Pointer(value float64) *float64 { return &value }

func cloneBoolPointer(value *bool) *bool {
	if value == nil {
		return nil
	}
	return boolPointer(*value)
}
func cloneUint64Pointer(value *uint64) *uint64 {
	if value == nil {
		return nil
	}
	return uint64Pointer(*value)
}
func cloneFloat64Pointer(value *float64) *float64 {
	if value == nil {
		return nil
	}
	return float64Pointer(*value)
}
