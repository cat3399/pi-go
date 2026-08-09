package app

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	agentruntime "github.com/cat3399/pi-go/internal/runtime"
)

// ProductionRuntimeOptions are the non-interactive selections needed by a
// long-lived embedding such as RPC mode. Nil APIKey preserves the distinction
// between an omitted credential and an explicitly invalid empty credential.
type ProductionRuntimeOptions struct {
	SessionPath string
	ProviderID  string
	ModelID     string
	APIKey      *string
}

// OpenProductionRuntime performs the same cwd-bound production assembly and
// session restore path as RunProduction, but returns the long-lived Runtime to
// a transport host instead of executing one print prompt.
func OpenProductionRuntime(ctx context.Context, config ProductionConfig, selection ProductionRuntimeOptions) (*agentruntime.Runtime, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if selection.SessionPath != "" && !utf8.ValidString(selection.SessionPath) {
		return nil, fmt.Errorf("%w: session path is not valid UTF-8", ErrInvalidArguments)
	}
	for name, value := range map[string]string{"provider ID": selection.ProviderID, "model ID": selection.ModelID} {
		if value != "" && !validSelectorValue(value) {
			return nil, fmt.Errorf("%w: %s must be valid UTF-8 without control characters", ErrInvalidArguments, name)
		}
	}
	if selection.APIKey != nil {
		if !utf8.ValidString(*selection.APIKey) || strings.TrimSpace(*selection.APIKey) == "" || strings.ContainsFunc(*selection.APIKey, unicode.IsControl) {
			return nil, fmt.Errorf("%w: API key must be non-empty valid UTF-8 without control characters", ErrInvalidArguments)
		}
	}
	parsed := options{
		sessionPath: selection.SessionPath, providerID: selection.ProviderID,
		modelID: selection.ModelID, hasAPIKey: selection.APIKey != nil,
	}
	if selection.APIKey != nil {
		parsed.apiKey = *selection.APIKey
	}
	dependencies, err := assembleProductionRuntime(ctx, config, parsed)
	if err != nil {
		return nil, err
	}
	sessionPath, err := resolveSessionPath(selection.SessionPath, dependencies)
	if err != nil {
		return nil, err
	}
	manager, err := openOrCreateSessionManager(ctx, sessionPath, dependencies)
	if err != nil {
		return nil, err
	}
	ownedByRuntime := false
	defer func() {
		if !ownedByRuntime {
			_ = manager.Close()
		}
	}()
	if err := agentruntime.AssertSessionCwdExists(manager, dependencies.workingDir); err != nil {
		return nil, err
	}
	runtime, err := agentruntime.Create(ctx, dependencies.factory, agentruntime.InitialOptions{
		CWD: manager.Cwd(), AgentDir: dependencies.agentDir, SessionManager: manager,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize agent runtime: %w", err)
	}
	ownedByRuntime = true
	return runtime, nil
}
