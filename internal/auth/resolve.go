package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	configCommandTimeout   = 10 * time.Second
	configCommandMaxOutput = 1 << 20
)

type commandCacheValue struct {
	value string
	ok    bool
}

var configCommandCache = struct {
	sync.Mutex
	values map[string]commandCacheValue
}{values: map[string]commandCacheValue{}}

// ResolveValue accepts literals, environment templates, and pi's leading-!
// shell-command form. Command stdout is trimmed, bounded, and cached for the
// process lifetime exactly like resolve-config-value.ts; stderr and command
// text never enter returned errors.
func ResolveValue(ctx context.Context, raw, description string, scoped, ambient map[string]string) (string, error) {
	return resolveValue(ctx, raw, description, scoped, ambient, true, false)
}

// ResolveValueUncached matches the request-time models.json path in pi:
// command-backed provider keys and headers are executed for each resolution,
// while stored auth values continue to use the process cache via ResolveValue.
func ResolveValueUncached(ctx context.Context, raw, description string, scoped, ambient map[string]string) (string, error) {
	return resolveValue(ctx, raw, description, scoped, ambient, false, false)
}

func resolveValue(ctx context.Context, raw, description string, scoped, ambient map[string]string, cacheCommand, allowEmptyLiteral bool) (string, error) {
	if cause := context.Cause(ctx); cause != nil {
		return "", failure(KindCancelled, "resolve "+description, "", cause)
	}
	if !utf8.ValidString(raw) {
		return "", failure(KindInvalid, "resolve "+description, "", nil)
	}
	if strings.HasPrefix(raw, "!") {
		return resolveCommandValue(ctx, raw, description, cacheCommand)
	}
	var output strings.Builder
	for index := 0; index < len(raw); {
		dollar := strings.IndexByte(raw[index:], '$')
		if dollar < 0 {
			output.WriteString(raw[index:])
			break
		}
		dollar += index
		output.WriteString(raw[index:dollar])
		if dollar+1 == len(raw) {
			output.WriteByte('$')
			break
		}
		next := raw[dollar+1]
		if next == '$' || next == '!' {
			output.WriteByte(next)
			index = dollar + 2
			continue
		}
		if next == '{' {
			end := strings.IndexByte(raw[dollar+2:], '}')
			if end < 0 {
				output.WriteByte('$')
				index = dollar + 1
				continue
			}
			end += dollar + 2
			name := raw[dollar+2 : end]
			if !validEnvironmentName(name) {
				output.WriteString(raw[dollar : end+1])
				index = end + 1
				continue
			}
			value, ok := lookupEnvironment(name, scoped, ambient)
			if !ok {
				return "", failure(KindNotConfigured, "resolve "+description, "", nil)
			}
			output.WriteString(value)
			index = end + 1
			continue
		}
		if !isEnvironmentStart(next) {
			output.WriteByte('$')
			index = dollar + 1
			continue
		}
		end := dollar + 2
		for end < len(raw) && isEnvironmentContinue(raw[end]) {
			end++
		}
		value, ok := lookupEnvironment(raw[dollar+1:end], scoped, ambient)
		if !ok {
			return "", failure(KindNotConfigured, "resolve "+description, "", nil)
		}
		output.WriteString(value)
		index = end
	}
	value := output.String()
	if (!allowEmptyLiteral && !validAPIKey(value)) || (allowEmptyLiteral && !utf8.ValidString(value)) {
		return "", failure(KindInvalid, "resolve "+description, "", nil)
	}
	return value, nil
}

func resolveCommandValue(ctx context.Context, raw, description string, cache bool) (string, error) {
	if cache {
		configCommandCache.Lock()
		cached, exists := configCommandCache.values[raw]
		configCommandCache.Unlock()
		if exists {
			if cached.ok {
				return cached.value, nil
			}
			return "", failure(KindNotConfigured, "resolve command-backed "+description, "", ErrCommandFailed)
		}
	}
	command := strings.TrimPrefix(raw, "!")
	if strings.TrimSpace(command) == "" {
		if cache {
			cacheCommandValue(raw, commandCacheValue{})
		}
		return "", failure(KindNotConfigured, "resolve command-backed "+description, "", ErrCommandFailed)
	}
	commandContext, cancel := context.WithTimeout(ctx, configCommandTimeout)
	defer cancel()
	shell, arguments := configCommandShell(command)
	process := exec.CommandContext(commandContext, shell, arguments...)
	output := &boundedCommandOutput{remaining: configCommandMaxOutput}
	process.Stdout = output
	process.Stdin = nil
	process.Stderr = io.Discard
	err := process.Run()
	if cause := context.Cause(ctx); cause != nil {
		return "", failure(KindCancelled, "resolve command-backed "+description, "", cause)
	}
	if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
		return "", failure(KindTimeout, "resolve command-backed "+description, "", context.DeadlineExceeded)
	}
	value := strings.TrimSpace(output.String())
	if err != nil || output.exceeded || !validAPIKey(value) {
		if cache {
			cacheCommandValue(raw, commandCacheValue{})
		}
		return "", failure(KindNotConfigured, "resolve command-backed "+description, "", ErrCommandFailed)
	}
	if cache {
		cacheCommandValue(raw, commandCacheValue{value: value, ok: true})
	}
	return value, nil
}

func configCommandShell(command string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd.exe", []string{"/d", "/s", "/c", command}
	}
	return "/bin/sh", []string{"-c", command}
}

type boundedCommandOutput struct {
	buffer    bytes.Buffer
	remaining int
	exceeded  bool
}

func (w *boundedCommandOutput) Write(value []byte) (int, error) {
	if len(value) > w.remaining {
		if w.remaining > 0 {
			_, _ = w.buffer.Write(value[:w.remaining])
		}
		w.remaining = 0
		w.exceeded = true
		return len(value), fmt.Errorf("command output exceeded limit")
	}
	w.remaining -= len(value)
	return w.buffer.Write(value)
}

func (w *boundedCommandOutput) String() string { return w.buffer.String() }

func cacheCommandValue(raw string, value commandCacheValue) {
	configCommandCache.Lock()
	configCommandCache.values[raw] = value
	configCommandCache.Unlock()
}

func ClearConfigValueCache() {
	configCommandCache.Lock()
	configCommandCache.values = map[string]commandCacheValue{}
	configCommandCache.Unlock()
}

// ResolveHeaders resolves models.json header templates and commands at request
// time. It never returns a partially resolved set: one failed value rejects
// the complete provider/model auth projection, as in resolveHeadersOrThrow.
func ResolveHeaders(ctx context.Context, raw map[string]string, description string, scoped, ambient map[string]string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	resolved := make(map[string]string, len(raw))
	for name, value := range raw {
		if strings.TrimSpace(name) == "" || !utf8.ValidString(name) || strings.ContainsAny(name, "\r\n") {
			return nil, failure(KindInvalid, "resolve "+description+" headers", "", nil)
		}
		item, err := resolveValue(ctx, value, description+" header", scoped, ambient, false, true)
		if err != nil {
			return nil, err
		}
		if strings.ContainsAny(item, "\r\n") {
			return nil, failure(KindInvalid, "resolve "+description+" headers", "", nil)
		}
		resolved[name] = item
	}
	return resolved, nil
}

// ValueConfigured is the side-effect-free availability check used before a
// request. Command values count as configured without executing them; template
// values require every referenced environment variable, matching pi's model
// runtime auth status behavior.
func ValueConfigured(raw string, scoped, ambient map[string]string) bool {
	if !utf8.ValidString(raw) {
		return false
	}
	if strings.HasPrefix(raw, "!") {
		return strings.TrimSpace(strings.TrimPrefix(raw, "!")) != ""
	}
	for index := 0; index < len(raw); {
		dollar := strings.IndexByte(raw[index:], '$')
		if dollar < 0 {
			break
		}
		dollar += index
		if dollar+1 == len(raw) {
			break
		}
		next := raw[dollar+1]
		if next == '$' || next == '!' {
			index = dollar + 2
			continue
		}
		if next == '{' {
			end := strings.IndexByte(raw[dollar+2:], '}')
			if end < 0 {
				index = dollar + 1
				continue
			}
			end += dollar + 2
			name := raw[dollar+2 : end]
			if validEnvironmentName(name) {
				if _, ok := lookupEnvironment(name, scoped, ambient); !ok {
					return false
				}
			}
			index = end + 1
			continue
		}
		if !isEnvironmentStart(next) {
			index = dollar + 1
			continue
		}
		end := dollar + 2
		for end < len(raw) && isEnvironmentContinue(raw[end]) {
			end++
		}
		if _, ok := lookupEnvironment(raw[dollar+1:end], scoped, ambient); !ok {
			return false
		}
		index = end
	}
	return validAPIKey(raw) || strings.Contains(raw, "$")
}

func lookupEnvironment(name string, scoped, ambient map[string]string) (string, bool) {
	if value := scoped[name]; value != "" {
		return value, true
	}
	if value := ambient[name]; value != "" {
		return value, true
	}
	return "", false
}
func validEnvironmentName(value string) bool {
	if value == "" || !isEnvironmentStart(value[0]) {
		return false
	}
	for i := 1; i < len(value); i++ {
		if !isEnvironmentContinue(value[i]) {
			return false
		}
	}
	return true
}
func isEnvironmentStart(value byte) bool {
	return value == '_' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}
func isEnvironmentContinue(value byte) bool {
	return isEnvironmentStart(value) || value >= '0' && value <= '9'
}
