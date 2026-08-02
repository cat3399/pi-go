package auth

import (
	"context"
	"strings"
	"unicode/utf8"
)

// ResolveValue accepts literals and environment templates. Command strings
// (leading !) are deliberately rejected in production: upstream shell
// execution has no product-level permission/working-directory contract, and
// credentials must not silently inherit an arbitrary shell environment.
func ResolveValue(ctx context.Context, raw, description string, scoped, ambient map[string]string) (string, error) {
	if cause := context.Cause(ctx); cause != nil {
		return "", failure(KindCancelled, "resolve "+description, "", cause)
	}
	if !utf8.ValidString(raw) {
		return "", failure(KindInvalid, "resolve "+description, "", nil)
	}
	if strings.HasPrefix(raw, "!") {
		return "", failure(KindUnsupported, "resolve command-backed "+description, "", ErrCommandBacked)
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
	if !validAPIKey(value) {
		return "", failure(KindInvalid, "resolve "+description, "", nil)
	}
	return value, nil
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
