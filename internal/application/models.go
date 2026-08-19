package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/auth"
	modelcatalog "github.com/cat3399/pi-go/internal/model"
	"github.com/cat3399/pi-go/internal/provider"
)

const maxModelsConfigBytes int64 = 4 << 20

var (
	ErrUnknownModelProvider    = errors.New("unknown model provider")
	ErrProviderAuthUnsupported = errors.New("provider does not support API key authentication")
	ErrCredentialTypeMismatch  = errors.New("provider uses a different credential type")
	ErrInvalidModelsConfig     = errors.New("invalid models configuration")
)

type ModelSelection struct {
	Provider string
	ModelID  string
}

type AvailableModel struct {
	Provider         string
	ID               string
	Name             string
	ThinkingLevels   []provider.ThinkingLevel
	ThinkingLevelMap map[provider.ThinkingLevel]*string
}

type ModelsSnapshot struct {
	Models             []AvailableModel
	DefaultModel       *ModelSelection
	ThinkingLevelPins  map[string]provider.ThinkingLevel
	ModelScopeWarnings []string
	Diagnostic         string
}

// UISettings is the surface-neutral subset of settings consumed directly by
// interactive clients rather than the Agent runtime.
type UISettings struct {
	Theme string
}

func (s *Service) GetUISettings(ctx context.Context, cwd string) (UISettings, error) {
	runtime, err := s.openModels(ctx, cwd)
	if err != nil {
		return UISettings{}, err
	}
	return UISettings{Theme: runtime.Snapshot().Settings.Theme}, nil
}

func (s *Service) SetTheme(ctx context.Context, cwd, theme string) (UISettings, error) {
	runtime, err := s.openModels(ctx, cwd)
	if err != nil {
		return UISettings{}, err
	}
	theme = strings.TrimSpace(theme)
	if err := runtime.SetGlobalSettings(normalizeContext(ctx), func(settings *modelcatalog.Settings) error {
		settings.Theme = theme
		return nil
	}); err != nil {
		return UISettings{}, err
	}
	return UISettings{Theme: runtime.Snapshot().Settings.Theme}, nil
}

type ProviderAuthInfo struct {
	ID             string
	Name           string
	Configured     bool
	Source         string
	CredentialType string
	ModelCount     int
	SupportsAPIKey bool
	SupportsOAuth  bool
	OAuthName      string
	Builtin        bool
}

type ModelsConfigDocument map[string]json.RawMessage

func (s *Service) openModels(ctx context.Context, cwd string) (*modelcatalog.Runtime, error) {
	if s == nil {
		return nil, errors.New("application service is unavailable")
	}
	cwd, err := ValidateCWD(cwd)
	if err != nil {
		return nil, err
	}
	trust, err := s.ProjectTrust(ctx, cwd)
	if err != nil {
		return nil, err
	}
	config := cloneProductionConfig(s.production)
	config.AgentDir = s.paths.AgentDir
	return app.OpenProductionModelRuntime(normalizeContext(ctx), config, cwd, trust.Trusted)
}

func (s *Service) ListModels(ctx context.Context, cwd string) (ModelsSnapshot, error) {
	runtime, err := s.openModels(ctx, cwd)
	if err != nil {
		return ModelsSnapshot{}, err
	}
	available, err := runtime.GetAvailable(normalizeContext(ctx))
	if err != nil {
		return ModelsSnapshot{}, err
	}
	visible := make([]modelcatalog.Model, 0, len(available))
	for _, candidate := range available {
		if runtime.ValidateRoute(candidate) == nil {
			visible = append(visible, candidate)
		}
	}
	snapshot := runtime.Snapshot()
	pins := make(map[string]provider.ThinkingLevel)
	warnings := []string{}
	if len(snapshot.Settings.EnabledModels) != 0 {
		scope := modelcatalog.ResolveModelScope(snapshot.Settings.EnabledModels, visible)
		if len(scope.ScopedModels) != 0 {
			visible = visible[:0]
			for _, scoped := range scope.ScopedModels {
				visible = append(visible, scoped.Model)
				if scoped.ThinkingLevel != nil {
					pins[scoped.Model.Provider+"/"+scoped.Model.ID] = *scoped.ThinkingLevel
				}
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
	result := ModelsSnapshot{
		Models: make([]AvailableModel, 0, len(visible)), ThinkingLevelPins: pins,
		ModelScopeWarnings: warnings,
	}
	if runtimeError := runtime.Error(); runtimeError != nil {
		result.Diagnostic = runtimeError.Error()
	}
	for _, candidate := range visible {
		entry := AvailableModel{
			Provider: candidate.Provider, ID: candidate.ID, Name: candidate.Name,
			ThinkingLevelMap: cloneApplicationThinkingMap(candidate.ThinkingLevelMap),
		}
		if ref, refErr := candidate.Ref(); refErr == nil {
			entry.ThinkingLevels = ref.SupportedThinkingLevels()
		}
		result.Models = append(result.Models, entry)
		if result.DefaultModel == nil && candidate.Provider == snapshot.Settings.DefaultProvider && candidate.ID == snapshot.Settings.DefaultModel {
			result.DefaultModel = &ModelSelection{Provider: candidate.Provider, ModelID: candidate.ID}
		}
	}
	return result, nil
}

func (s *Service) ListModelProviders(ctx context.Context, cwd string) ([]ProviderAuthInfo, error) {
	runtime, err := s.openModels(ctx, cwd)
	if err != nil {
		return nil, err
	}
	store, err := auth.NewStore(auth.Options{Path: filepath.Join(s.paths.AgentDir, "auth.json")})
	if err != nil {
		return nil, err
	}
	credentials, err := store.List(normalizeContext(ctx))
	if err != nil {
		return nil, err
	}
	credentialTypes := make(map[string]string, len(credentials))
	for _, credential := range credentials {
		credentialTypes[credential.ProviderID] = credential.Type
	}
	providers := runtime.GetProviders()
	result := make([]ProviderAuthInfo, 0, len(providers))
	for _, entry := range providers {
		check, checkErr := entry.CheckAuth(normalizeContext(ctx))
		if checkErr != nil {
			return nil, checkErr
		}
		configured, _ := runtime.Provider(entry.ID())
		supportsOAuth, oauthName := productionOAuthInfo(entry.ID())
		info := ProviderAuthInfo{
			ID: entry.ID(), Name: entry.Name(), Configured: check != nil,
			CredentialType: credentialTypes[entry.ID()], ModelCount: len(entry.GetModels()),
			SupportsAPIKey: !configured.Keyless, SupportsOAuth: supportsOAuth, OAuthName: oauthName,
			Builtin: modelcatalog.IsBuiltinProvider(entry.ID()),
		}
		if check != nil {
			info.Source = check.Source
		}
		result = append(result, info)
	}
	return result, nil
}

func productionOAuthInfo(providerID string) (bool, string) {
	switch providerID {
	case auth.OpenAICodexProviderID:
		return true, "ChatGPT Plus/Pro"
	case auth.AnthropicProviderID:
		return true, "Anthropic (Claude Pro/Max)"
	default:
		return false, ""
	}
}

func (s *Service) SetProviderAPIKey(ctx context.Context, providerID, apiKey string) error {
	providerID, apiKey = strings.TrimSpace(providerID), strings.TrimSpace(apiKey)
	if providerID == "" || apiKey == "" {
		return fmt.Errorf("%w: provider and API key are required", ErrInvalidModelsConfig)
	}
	runtime, err := s.openModels(ctx, s.DefaultCWD())
	if err != nil {
		return err
	}
	if _, exists := runtime.GetProvider(providerID); !exists {
		return fmt.Errorf("%w: %s", ErrUnknownModelProvider, providerID)
	}
	configured, exists := runtime.Provider(providerID)
	if !exists || configured.Keyless {
		return fmt.Errorf("%w: %s", ErrProviderAuthUnsupported, providerID)
	}
	store, err := auth.NewStore(auth.Options{Path: filepath.Join(s.paths.AgentDir, "auth.json")})
	if err != nil {
		return err
	}
	s.modelMu.Lock()
	defer s.modelMu.Unlock()
	return store.SetAPIKey(normalizeContext(ctx), providerID, apiKey, nil)
}

func (s *Service) DeleteProviderCredential(ctx context.Context, providerID, expectedType string) error {
	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		return fmt.Errorf("%w: provider is required", ErrInvalidModelsConfig)
	}
	store, err := auth.NewStore(auth.Options{Path: filepath.Join(s.paths.AgentDir, "auth.json")})
	if err != nil {
		return err
	}
	s.modelMu.Lock()
	defer s.modelMu.Unlock()
	credential, exists, err := store.Read(normalizeContext(ctx), providerID)
	if err != nil || !exists {
		return err
	}
	if expectedType != "" && credential.Type != expectedType {
		return fmt.Errorf("%w: %s is authenticated with %s", ErrCredentialTypeMismatch, providerID, credential.Type)
	}
	return store.Delete(normalizeContext(ctx), providerID)
}

func (s *Service) ReadModelsConfig(ctx context.Context) (ModelsConfigDocument, error) {
	if cause := context.Cause(normalizeContext(ctx)); cause != nil {
		return nil, cause
	}
	path := filepath.Join(s.paths.AgentDir, "models.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ModelsConfigDocument{"providers": json.RawMessage(`{}`)}, nil
	}
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxModelsConfigBytes {
		return nil, fmt.Errorf("%w: models.json exceeds %d bytes", ErrInvalidModelsConfig, maxModelsConfigBytes)
	}
	var document ModelsConfigDocument
	if err := json.Unmarshal(data, &document); err != nil || document == nil {
		return nil, fmt.Errorf("%w: models.json must contain a JSON object", ErrInvalidModelsConfig)
	}
	if _, exists := document["providers"]; !exists {
		document["providers"] = json.RawMessage(`{}`)
	}
	return cloneModelsConfigDocument(document), nil
}

func (s *Service) WriteModelsConfig(ctx context.Context, document ModelsConfigDocument) error {
	if document == nil {
		return fmt.Errorf("%w: root must be an object", ErrInvalidModelsConfig)
	}
	providers, exists := document["providers"]
	if !exists {
		providers = json.RawMessage(`{}`)
		document = cloneModelsConfigDocument(document)
		document["providers"] = providers
	}
	var providerObject map[string]json.RawMessage
	if json.Unmarshal(providers, &providerObject) != nil || providerObject == nil {
		return fmt.Errorf("%w: providers must be an object", ErrInvalidModelsConfig)
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: encode models.json", ErrInvalidModelsConfig)
	}
	encoded = append(encoded, '\n')
	if int64(len(encoded)) > maxModelsConfigBytes {
		return fmt.Errorf("%w: models.json exceeds %d bytes", ErrInvalidModelsConfig, maxModelsConfigBytes)
	}
	s.modelMu.Lock()
	defer s.modelMu.Unlock()
	if err := os.MkdirAll(s.paths.AgentDir, 0o700); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.paths.AgentDir, "models.json"), encoded, 0o600)
}

func cloneApplicationThinkingMap(values map[provider.ThinkingLevel]*string) map[provider.ThinkingLevel]*string {
	if len(values) == 0 {
		return nil
	}
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

func cloneModelsConfigDocument(document ModelsConfigDocument) ModelsConfigDocument {
	result := make(ModelsConfigDocument, len(document))
	for key, value := range document {
		result[key] = append(json.RawMessage(nil), value...)
	}
	return result
}
