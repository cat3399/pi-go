package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrInvalidToolDefinition identifies a caller-supplied model tool contract
// that is unsafe to send to a provider. Tool execution validates arguments
// again at its own boundary; this value only describes the immutable request
// snapshot visible to a model.
var ErrInvalidToolDefinition = errors.New("invalid tool definition")

const maxOpenAIFunctionNameLength = 64

// ToolDefinition is the neutral, provider-independent function tool surface.
// ParametersJSON is a JSON Schema object (not a JSON string) and is copied at
// both construction and access boundaries so request construction cannot race
// with a caller mutating a byte slice.
type ToolDefinition struct {
	name        string
	description string
	strict      bool
	parameters  []byte
}

func NewToolDefinition(name, description string, strict bool, parametersJSON []byte) (ToolDefinition, error) {
	definition := ToolDefinition{
		name:        name,
		description: description,
		strict:      strict,
		parameters:  bytes.Clone(parametersJSON),
	}
	if err := definition.validate(); err != nil {
		return ToolDefinition{}, err
	}
	return definition, nil
}

func (d ToolDefinition) validate() error {
	if !utf8.ValidString(d.name) {
		return fmt.Errorf("%w: name must be valid UTF-8", ErrInvalidToolDefinition)
	}
	if d.name == "" {
		return fmt.Errorf("%w: name must be non-empty", ErrInvalidToolDefinition)
	}
	if len(d.name) > maxOpenAIFunctionNameLength {
		return fmt.Errorf(
			"%w: name must be at most %d characters",
			ErrInvalidToolDefinition,
			maxOpenAIFunctionNameLength,
		)
	}
	for _, character := range d.name {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '_' || character == '-' {
			continue
		}
		return fmt.Errorf(
			"%w: name may contain only ASCII letters, digits, underscore, and hyphen",
			ErrInvalidToolDefinition,
		)
	}
	if !utf8.ValidString(d.description) || strings.TrimSpace(d.description) == "" {
		return fmt.Errorf("%w: description must be non-empty valid UTF-8", ErrInvalidToolDefinition)
	}
	if !utf8.Valid(d.parameters) || len(bytes.TrimSpace(d.parameters)) == 0 {
		return fmt.Errorf("%w: parameters must be non-empty valid JSON", ErrInvalidToolDefinition)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(d.parameters, &object); err != nil || object == nil {
		if err == nil {
			err = errors.New("top-level value is not an object")
		}
		return fmt.Errorf("%w: parameters: %v", ErrInvalidToolDefinition, err)
	}
	return nil
}

func (d ToolDefinition) Name() string        { return d.name }
func (d ToolDefinition) Description() string { return d.description }
func (d ToolDefinition) Strict() bool        { return d.strict }
func (d ToolDefinition) ParametersJSON() []byte {
	return bytes.Clone(d.parameters)
}
