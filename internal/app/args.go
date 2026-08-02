package app

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

type options struct {
	prompt      string
	sessionPath string
	providerID  string
	modelID     string
	apiKey      string
	hasAPIKey   bool
}

func parseArgs(args []string) (options, error) {
	var parsed options
	seenPrint := false
	seenSession := false
	seenProvider := false
	seenModel := false
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "-p", "--print":
			if seenPrint {
				return options{}, fmt.Errorf("%w: print prompt may be specified only once", ErrInvalidArguments)
			}
			seenPrint = true
			if index+1 >= len(args) {
				return options{}, fmt.Errorf("%w: %s requires a prompt", ErrInvalidArguments, args[index])
			}
			index++
			parsed.prompt = args[index]
		case "--session":
			if seenSession {
				return options{}, fmt.Errorf("%w: --session may be specified only once", ErrInvalidArguments)
			}
			seenSession = true
			if index+1 >= len(args) {
				return options{}, fmt.Errorf("%w: --session requires a path", ErrInvalidArguments)
			}
			index++
			parsed.sessionPath = args[index]
		case "--provider":
			if seenProvider {
				return options{}, fmt.Errorf("%w: --provider may be specified only once", ErrInvalidArguments)
			}
			seenProvider = true
			if index+1 >= len(args) {
				return options{}, fmt.Errorf("%w: --provider requires a provider ID", ErrInvalidArguments)
			}
			index++
			parsed.providerID = args[index]
		case "--model":
			if seenModel {
				return options{}, fmt.Errorf("%w: --model may be specified only once", ErrInvalidArguments)
			}
			seenModel = true
			if index+1 >= len(args) {
				return options{}, fmt.Errorf("%w: --model requires a model ID", ErrInvalidArguments)
			}
			index++
			parsed.modelID = args[index]
		case "--api-key":
			if parsed.hasAPIKey {
				return options{}, fmt.Errorf("%w: --api-key may be specified only once", ErrInvalidArguments)
			}
			parsed.hasAPIKey = true
			if index+1 >= len(args) {
				return options{}, fmt.Errorf("%w: --api-key requires a value", ErrInvalidArguments)
			}
			index++
			parsed.apiKey = args[index]
		default:
			return options{}, fmt.Errorf("%w: unsupported argument at position %d", ErrInvalidArguments, index+1)
		}
	}
	if !seenPrint || parsed.prompt == "" {
		return options{}, fmt.Errorf("%w: an explicit -p prompt is required", ErrInvalidArguments)
	}
	if !utf8.ValidString(parsed.prompt) {
		return options{}, fmt.Errorf("%w: prompt is not valid UTF-8", ErrInvalidArguments)
	}
	if seenSession && (parsed.sessionPath == "" || !utf8.ValidString(parsed.sessionPath)) {
		return options{}, fmt.Errorf("%w: session path must be non-empty valid UTF-8", ErrInvalidArguments)
	}
	if seenProvider && !validSelectorValue(parsed.providerID) {
		return options{}, fmt.Errorf("%w: provider ID must be non-empty valid UTF-8 without control characters", ErrInvalidArguments)
	}
	if seenModel && !validSelectorValue(parsed.modelID) {
		return options{}, fmt.Errorf("%w: model ID must be non-empty valid UTF-8 without control characters", ErrInvalidArguments)
	}
	if parsed.hasAPIKey && (!utf8.ValidString(parsed.apiKey) || strings.TrimSpace(parsed.apiKey) == "") {
		return options{}, fmt.Errorf("%w: API key must be non-empty valid UTF-8", ErrInvalidArguments)
	}
	if parsed.hasAPIKey && strings.ContainsFunc(parsed.apiKey, unicode.IsControl) {
		return options{}, fmt.Errorf("%w: API key contains a control character", ErrInvalidArguments)
	}
	return parsed, nil
}

func validSelectorValue(value string) bool {
	return utf8.ValidString(value) &&
		strings.TrimSpace(value) != "" &&
		!strings.ContainsFunc(value, unicode.IsControl)
}

func (o options) hasProductionSelection() bool {
	return o.providerID != "" || o.modelID != "" || o.hasAPIKey
}
