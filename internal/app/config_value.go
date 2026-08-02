package app

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

func environmentMap(environment []string) map[string]string {
	result := make(map[string]string, len(environment))
	for _, entry := range environment {
		name, value, found := strings.Cut(entry, "=")
		if !found || name == "" {
			continue
		}
		result[name] = value
	}
	return result
}

func lookupNonEmptyEnvironment(name string, scoped, ambient map[string]string) (string, bool) {
	if value := scoped[name]; value != "" {
		return value, true
	}
	if value := ambient[name]; value != "" {
		return value, true
	}
	return "", false
}

// resolveProductionConfigValue implements the literal and environment-template
// subset shared by auth.json and models.json at the fixed upstream commit.
// Command-backed values are an explicit later subprocess/security slice; they
// fail here instead of being treated as literals or falling through sources.
func resolveProductionConfigValue(
	raw string,
	description string,
	scopedEnvironment map[string]string,
	ambientEnvironment map[string]string,
) (string, error) {
	if !utf8.ValidString(raw) {
		return "", fmt.Errorf("%w: %s is not valid UTF-8", ErrInvalidProductionConfig, description)
	}
	if strings.HasPrefix(raw, "!") {
		return "", fmt.Errorf(
			"%w: command-backed %s is not migrated",
			ErrUnsupportedProductionValue,
			description,
		)
	}

	var resolved strings.Builder
	for index := 0; index < len(raw); {
		dollar := strings.IndexByte(raw[index:], '$')
		if dollar < 0 {
			resolved.WriteString(raw[index:])
			break
		}
		dollar += index
		resolved.WriteString(raw[index:dollar])
		if dollar+1 >= len(raw) {
			resolved.WriteByte('$')
			break
		}

		next := raw[dollar+1]
		switch next {
		case '$', '!':
			resolved.WriteByte(next)
			index = dollar + 2
			continue
		case '{':
			closing := strings.IndexByte(raw[dollar+2:], '}')
			if closing < 0 {
				resolved.WriteByte('$')
				index = dollar + 1
				continue
			}
			closing += dollar + 2
			name := raw[dollar+2 : closing]
			if !validEnvironmentName(name) {
				resolved.WriteString(raw[dollar : closing+1])
				index = closing + 1
				continue
			}
			value, ok := lookupNonEmptyEnvironment(name, scopedEnvironment, ambientEnvironment)
			if !ok {
				return "", fmt.Errorf(
					"%w: %s references missing environment variable %q",
					ErrInvalidProductionConfig,
					description,
					name,
				)
			}
			resolved.WriteString(value)
			index = closing + 1
			continue
		default:
			if !isEnvironmentNameStart(next) {
				resolved.WriteByte('$')
				index = dollar + 1
				continue
			}
			end := dollar + 2
			for end < len(raw) && isEnvironmentNameContinue(raw[end]) {
				end++
			}
			name := raw[dollar+1 : end]
			value, ok := lookupNonEmptyEnvironment(name, scopedEnvironment, ambientEnvironment)
			if !ok {
				return "", fmt.Errorf(
					"%w: %s references missing environment variable %q",
					ErrInvalidProductionConfig,
					description,
					name,
				)
			}
			resolved.WriteString(value)
			index = end
		}
	}

	value := resolved.String()
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%w: %s resolved to an empty value", ErrInvalidProductionConfig, description)
	}
	return value, nil
}

func validEnvironmentName(value string) bool {
	if value == "" || !isEnvironmentNameStart(value[0]) {
		return false
	}
	for index := 1; index < len(value); index++ {
		if !isEnvironmentNameContinue(value[index]) {
			return false
		}
	}
	return true
}

func isEnvironmentNameStart(value byte) bool {
	return value == '_' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

func isEnvironmentNameContinue(value byte) bool {
	return isEnvironmentNameStart(value) || value >= '0' && value <= '9'
}
