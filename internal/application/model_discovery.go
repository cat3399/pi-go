package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cat3399/pi-go/internal/auth"
)

const (
	modelDiscoveryTimeout  = 20 * time.Second
	maxDiscoveryBodyBytes  = 16 << 20
	maxDiscoveryErrorBytes = 500
)

type ModelProviderDraft struct {
	BaseURL string            `json:"baseUrl"`
	API     string            `json:"api"`
	APIKey  string            `json:"apiKey"`
	Headers map[string]string `json:"headers"`
}

type DiscoveredModel struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type ModelDiscoveryResult struct {
	Models   []DiscoveredModel
	Endpoint string
}

type ModelDiscoveryUpstreamError struct {
	Status  int
	Timeout bool
	Err     error
}

func (err *ModelDiscoveryUpstreamError) Error() string {
	if err == nil || err.Err == nil {
		return "model discovery failed"
	}
	return err.Err.Error()
}

func (err *ModelDiscoveryUpstreamError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Err
}

func (s *Service) DiscoverModels(ctx context.Context, providerName string, draft ModelProviderDraft) (ModelDiscoveryResult, error) {
	providerName = strings.TrimSpace(providerName)
	if providerName == "" {
		return ModelDiscoveryResult{}, errors.New("providerName is required")
	}
	endpoint, err := buildModelsListURL(draft.BaseURL, draft.API)
	if err != nil {
		return ModelDiscoveryResult{}, errors.New("Base URL is invalid")
	}
	ambient := applicationEnvironment(s.production.Environment)
	resolvedHeaders, err := auth.ResolveHeaders(normalizeContext(ctx), draft.Headers, "model discovery", nil, ambient)
	if err != nil {
		return ModelDiscoveryResult{}, err
	}
	apiKey, err := s.resolveDiscoveryAPIKey(normalizeContext(ctx), providerName, draft.APIKey, ambient)
	if err != nil {
		return ModelDiscoveryResult{}, err
	}
	if strings.TrimSpace(draft.APIKey) != "" && apiKey == "" {
		return ModelDiscoveryResult{}, fmt.Errorf("no API key found for %q", providerName)
	}
	headers := http.Header{}
	for name, value := range resolvedHeaders {
		headers.Set(name, value)
	}
	if headers.Get("Accept") == "" {
		headers.Set("Accept", "application/json")
	}
	installDiscoveryAuthHeaders(headers, draft.API, apiKey)

	requestContext, cancel := context.WithTimeout(normalizeContext(ctx), modelDiscoveryTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return ModelDiscoveryResult{}, err
	}
	request.Header = headers
	response, err := s.modelHTTP.Do(request)
	if err != nil {
		if errors.Is(requestContext.Err(), context.DeadlineExceeded) {
			return ModelDiscoveryResult{}, &ModelDiscoveryUpstreamError{Timeout: true, Err: errors.New("model discovery timed out")}
		}
		return ModelDiscoveryResult{}, &ModelDiscoveryUpstreamError{Err: err}
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxDiscoveryBodyBytes+1))
	if err != nil {
		return ModelDiscoveryResult{}, &ModelDiscoveryUpstreamError{Status: response.StatusCode, Err: err}
	}
	if len(data) > maxDiscoveryBodyBytes {
		return ModelDiscoveryResult{}, &ModelDiscoveryUpstreamError{Status: response.StatusCode, Err: errors.New("upstream model list is too large")}
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		message := strings.TrimSpace(string(data))
		if len(message) > maxDiscoveryErrorBytes {
			message = message[:maxDiscoveryErrorBytes]
		}
		if message == "" {
			message = fmt.Sprintf("upstream returned HTTP %d", response.StatusCode)
		}
		return ModelDiscoveryResult{}, &ModelDiscoveryUpstreamError{Status: response.StatusCode, Err: errors.New(message)}
	}
	var payload any
	if err := json.Unmarshal(data, &payload); err != nil {
		return ModelDiscoveryResult{}, &ModelDiscoveryUpstreamError{Status: response.StatusCode, Err: errors.New("upstream model list was not valid JSON")}
	}
	models := parseDiscoveredModels(payload)
	if len(models) == 0 {
		return ModelDiscoveryResult{}, &ModelDiscoveryUpstreamError{Status: response.StatusCode, Err: errors.New("no models found in the upstream response")}
	}
	return ModelDiscoveryResult{Models: models, Endpoint: endpoint.String()}, nil
}

func (s *Service) resolveDiscoveryAPIKey(ctx context.Context, providerName, configured string, ambient map[string]string) (string, error) {
	if strings.TrimSpace(configured) != "" {
		return auth.ResolveValueUncached(ctx, configured, "configured model discovery API key", nil, ambient)
	}
	store, err := auth.NewStore(auth.Options{Path: filepath.Join(s.paths.AgentDir, "auth.json")})
	if err != nil {
		return "", err
	}
	credential, exists, err := store.Read(ctx, providerName)
	if err != nil || !exists || credential.Type != "api_key" {
		return "", err
	}
	return auth.ResolveValue(ctx, credential.Key, "stored model discovery API key", credential.Env, ambient)
}

func buildModelsListURL(baseURL, api string) (*url.URL, error) {
	baseURL = strings.TrimSpace(baseURL)
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return nil, errors.New("invalid base URL")
	}
	trimmedPath := strings.TrimRight(parsed.Path, "/")
	if !strings.EqualFold(path.Base(trimmedPath), "models") {
		if api == "anthropic-messages" && !versionPathSuffix(trimmedPath) {
			trimmedPath += "/v1"
		}
		if api == "google-generative-ai" && !versionPathSuffix(trimmedPath) {
			trimmedPath += "/v1beta"
		}
		parsed.Path = strings.TrimRight(trimmedPath, "/") + "/models"
	}
	query := parsed.Query()
	if api == "anthropic-messages" && query.Get("limit") == "" {
		query.Set("limit", "1000")
	}
	if api == "google-generative-ai" && query.Get("pageSize") == "" {
		query.Set("pageSize", "1000")
	}
	parsed.RawQuery = query.Encode()
	return parsed, nil
}

func versionPathSuffix(value string) bool {
	last := strings.ToLower(path.Base(strings.TrimRight(value, "/")))
	if len(last) < 2 || last[0] != 'v' {
		return false
	}
	index := 1
	for index < len(last) && last[index] >= '0' && last[index] <= '9' {
		index++
	}
	if index == 1 {
		return false
	}
	return index == len(last) || last[index:] == "beta"
}

func installDiscoveryAuthHeaders(headers http.Header, api, apiKey string) {
	if apiKey == "" {
		return
	}
	switch api {
	case "anthropic-messages":
		if headers.Get("x-api-key") == "" {
			headers.Set("x-api-key", apiKey)
		}
		if headers.Get("anthropic-version") == "" {
			headers.Set("anthropic-version", "2023-06-01")
		}
	case "google-generative-ai":
		if headers.Get("x-goog-api-key") == "" {
			headers.Set("x-goog-api-key", apiKey)
		}
	default:
		if headers.Get("Authorization") == "" {
			headers.Set("Authorization", "Bearer "+apiKey)
		}
	}
}

func parseDiscoveredModels(payload any) []DiscoveredModel {
	items := discoveryList(payload)
	seen := make(map[string]struct{}, len(items))
	result := make([]DiscoveredModel, 0, len(items))
	for _, item := range items {
		model, ok := discoveredModel(item)
		if !ok {
			continue
		}
		if _, exists := seen[model.ID]; exists {
			continue
		}
		seen[model.ID] = struct{}{}
		result = append(result, model)
	}
	sort.SliceStable(result, func(left, right int) bool {
		leftName, rightName := result[left].Name, result[right].Name
		if leftName == "" {
			leftName = result[left].ID
		}
		if rightName == "" {
			rightName = result[right].ID
		}
		return strings.ToLower(leftName) < strings.ToLower(rightName)
	})
	return result
}

func discoveryList(payload any) []any {
	if values, ok := payload.([]any); ok {
		return values
	}
	object, ok := payload.(map[string]any)
	if !ok {
		return nil
	}
	for _, key := range []string{"data", "models", "results", "items"} {
		switch value := object[key].(type) {
		case []any:
			return value
		case map[string]any:
			result := make([]any, 0, len(value))
			keys := make([]string, 0, len(value))
			for itemKey := range value {
				keys = append(keys, itemKey)
			}
			sort.Strings(keys)
			for _, itemKey := range keys {
				result = append(result, value[itemKey])
			}
			return result
		}
	}
	return nil
}

func discoveredModel(value any) (DiscoveredModel, bool) {
	if text, ok := value.(string); ok {
		id := strings.TrimSpace(text)
		return DiscoveredModel{ID: id}, id != ""
	}
	object, ok := value.(map[string]any)
	if !ok {
		return DiscoveredModel{}, false
	}
	clean := func(key string) string {
		text, _ := object[key].(string)
		return strings.TrimSpace(text)
	}
	rawID := clean("id")
	if rawID == "" {
		rawID = clean("model")
	}
	if rawID == "" {
		rawID = clean("name")
	}
	id := strings.TrimPrefix(rawID, "models/")
	if id == "" {
		return DiscoveredModel{}, false
	}
	name := clean("display_name")
	if name == "" {
		name = clean("displayName")
	}
	if name == "" && (clean("id") != "" || clean("model") != "") {
		name = clean("name")
	}
	if name == id {
		name = ""
	}
	return DiscoveredModel{ID: id, Name: name}, true
}

func applicationEnvironment(values []string) map[string]string {
	if values == nil {
		values = os.Environ()
	}
	result := make(map[string]string, len(values))
	for _, entry := range values {
		name, value, found := strings.Cut(entry, "=")
		if found && name != "" {
			result[name] = value
		}
	}
	return result
}
