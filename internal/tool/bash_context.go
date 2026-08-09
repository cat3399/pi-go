package tool

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"unicode/utf8"
)

// BashSessionEnvironment is the per-call session metadata exposed by the
// original coding-agent Bash tool. AgentSession owns the live values; the tool
// package only provides this injection seam.
type BashSessionEnvironment struct {
	SessionID      string
	SessionFile    string
	Provider       string
	Model          string
	ReasoningLevel string
}

// BashExecutionContext customizes one invocation without mutating Bash's
// immutable base configuration. Environment entries overlay the sanitized base
// environment. They are applied after PI_* injection, matching the upstream
// spawn hook's ability to adjust the final environment.
type BashExecutionContext struct {
	WorkingDir         string
	Environment        map[string]string
	SessionEnvironment *BashSessionEnvironment
}

type bashExecutionContextKey struct{}

// WithBashExecutionContext attaches one immutable per-call execution snapshot
// to ctx. Execute and ExecuteJSON consume it. Callers that do not use a context
// value can call ExecuteWithContext or ExecuteJSONWithContext directly.
func WithBashExecutionContext(ctx context.Context, execution BashExecutionContext) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, bashExecutionContextKey{}, cloneBashExecutionContext(execution))
}

func bashExecutionContextFromContext(ctx context.Context) BashExecutionContext {
	if ctx == nil {
		return BashExecutionContext{}
	}
	execution, _ := ctx.Value(bashExecutionContextKey{}).(BashExecutionContext)
	return cloneBashExecutionContext(execution)
}

func cloneBashExecutionContext(execution BashExecutionContext) BashExecutionContext {
	if execution.Environment != nil {
		environment := make(map[string]string, len(execution.Environment))
		for key, value := range execution.Environment {
			environment[key] = value
		}
		execution.Environment = environment
	}
	if execution.SessionEnvironment != nil {
		session := *execution.SessionEnvironment
		execution.SessionEnvironment = &session
	}
	return execution
}

func (b *Bash) resolveExecutionContext(execution BashExecutionContext) (string, []string, error) {
	workingDir := b.workingDir
	if execution.WorkingDir != "" {
		if err := validateExecutionText("working directory", execution.WorkingDir); err != nil {
			return "", nil, err
		}
		workingDir = execution.WorkingDir
		if !filepath.IsAbs(workingDir) {
			workingDir = filepath.Join(b.workingDir, workingDir)
		}
		absolute, err := filepath.Abs(workingDir)
		if err != nil {
			return "", nil, fmt.Errorf("%w: resolve working directory: %v", ErrInvalidBashInput, err)
		}
		workingDir = filepath.Clean(absolute)
	}

	overlay := make(map[string]string, len(execution.Environment)+len(strippedSessionEnvironment))
	if session := execution.SessionEnvironment; session != nil {
		values := []struct {
			name  string
			value string
		}{
			{"PI_SESSION_ID", session.SessionID},
			{"PI_SESSION_FILE", session.SessionFile},
			{"PI_PROVIDER", session.Provider},
			{"PI_MODEL", session.Model},
			{"PI_REASONING_LEVEL", session.ReasoningLevel},
		}
		for _, item := range values {
			if item.value == "" {
				continue
			}
			if err := validateExecutionText(item.name, item.value); err != nil {
				return "", nil, err
			}
			overlay[item.name] = item.value
		}
	}
	for key, value := range execution.Environment {
		if key == "" || strings.Contains(key, "=") || !utf8.ValidString(key) || strings.IndexByte(key, 0) >= 0 {
			return "", nil, fmt.Errorf("%w: environment key %q is invalid", ErrInvalidBashInput, key)
		}
		if err := validateExecutionText("environment value for "+key, value); err != nil {
			return "", nil, err
		}
		overlay[key] = value
	}
	return workingDir, overlayEnvironment(b.environment, overlay), nil
}

func validateExecutionText(name, value string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: %s must be valid UTF-8", ErrInvalidBashInput, name)
	}
	if strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%w: %s contains NUL", ErrInvalidBashInput, name)
	}
	return nil
}

func overlayEnvironment(base []string, overlay map[string]string) []string {
	result := append([]string(nil), base...)
	if len(overlay) == 0 {
		return result
	}
	keys := make([]string, 0, len(overlay))
	for key := range overlay {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		filtered := result[:0]
		for _, entry := range result {
			existing, _, ok := splitEnvironmentEntry(entry)
			if ok && (existing == key || runtime.GOOS == "windows" && strings.EqualFold(existing, key)) {
				continue
			}
			filtered = append(filtered, entry)
		}
		result = append(filtered, key+"="+overlay[key])
	}
	return result
}
