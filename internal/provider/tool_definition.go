package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
	if d.strict {
		if err := validateStrictFunctionParameters(object); err != nil {
			return fmt.Errorf("%w: strict parameters: %v", ErrInvalidToolDefinition, err)
		}
	}
	return nil
}

// validateStrictFunctionParameters enforces the structural invariants OpenAI
// requires when a function definition opts into strict mode. In particular,
// every object schema must close additional properties and require every
// declared property. Optional values remain representable as required nullable
// properties (for example, type ["string", "null"]).
func validateStrictFunctionParameters(root map[string]json.RawMessage) error {
	var rootType string
	if err := json.Unmarshal(root["type"], &rootType); err != nil || rootType != "object" {
		return errors.New("root type must be object")
	}
	if _, anyOf := root["anyOf"]; anyOf {
		return errors.New("root must not use anyOf")
	}
	return validateStrictSchemaNode(root, "$")
}

func validateStrictSchemaNode(schema map[string]json.RawMessage, path string) error {
	for _, keyword := range []string{"allOf", "oneOf", "not", "dependentRequired", "dependentSchemas", "if", "then", "else"} {
		if _, present := schema[keyword]; present {
			return fmt.Errorf("%s.%s is not supported in strict mode", path, keyword)
		}
	}
	types, hasType, err := strictSchemaTypes(schema["type"])
	if err != nil {
		return fmt.Errorf("%s.type: %v", path, err)
	}
	if !hasType {
		_, hasReference := schema["$ref"]
		_, hasAnyOf := schema["anyOf"]
		if !hasReference && !hasAnyOf {
			return fmt.Errorf("%s must declare type, $ref, or anyOf", path)
		}
	}
	if raw, present := schema["$ref"]; present {
		var reference string
		if err := json.Unmarshal(raw, &reference); err != nil || !strings.HasPrefix(reference, "#") {
			return fmt.Errorf("%s.$ref must be a local reference", path)
		}
	}
	if _, object := types["object"]; object {
		if err := validateStrictObjectSchema(schema, path); err != nil {
			return err
		}
	} else {
		for _, keyword := range []string{"properties", "required", "additionalProperties"} {
			if _, present := schema[keyword]; present {
				return fmt.Errorf("%s.%s requires object type", path, keyword)
			}
		}
	}
	if _, array := types["array"]; array {
		items, present := schema["items"]
		if !present {
			return fmt.Errorf("%s.items is required for array type", path)
		}
		itemSchema, err := strictChildSchema(items)
		if err != nil {
			return fmt.Errorf("%s.items: %v", path, err)
		}
		if err := validateStrictSchemaNode(itemSchema, path+".items"); err != nil {
			return err
		}
	}
	if raw, present := schema["anyOf"]; present {
		if err := validateStrictSchemaAlternatives(raw, path+".anyOf"); err != nil {
			return err
		}
	}
	if raw, present := schema["$defs"]; present {
		if err := validateStrictSchemaDefinitions(raw, path+".$defs"); err != nil {
			return err
		}
	}
	return nil
}

func validateStrictObjectSchema(schema map[string]json.RawMessage, path string) error {
	rawAdditional, present := schema["additionalProperties"]
	if !present {
		return fmt.Errorf("%s.additionalProperties must be false", path)
	}
	var additional bool
	if err := json.Unmarshal(rawAdditional, &additional); err != nil || additional {
		return fmt.Errorf("%s.additionalProperties must be false", path)
	}

	rawProperties, present := schema["properties"]
	if !present {
		return fmt.Errorf("%s.properties is required", path)
	}
	var properties map[string]json.RawMessage
	if err := json.Unmarshal(rawProperties, &properties); err != nil || properties == nil {
		return fmt.Errorf("%s.properties must be an object", path)
	}
	rawRequired, present := schema["required"]
	if !present {
		return fmt.Errorf("%s.required must include every property", path)
	}
	required, err := strictRequiredNames(rawRequired)
	if err != nil {
		return fmt.Errorf("%s.required: %v", path, err)
	}
	seenRequired := make(map[string]struct{}, len(required))
	for _, name := range required {
		if _, duplicate := seenRequired[name]; duplicate {
			return fmt.Errorf("%s.required contains duplicate %q", path, name)
		}
		if _, declared := properties[name]; !declared {
			return fmt.Errorf("%s.required contains undeclared property %q", path, name)
		}
		seenRequired[name] = struct{}{}
	}

	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, required := seenRequired[name]; !required {
			return fmt.Errorf("%s.required omits property %q", path, name)
		}
		child, err := strictChildSchema(properties[name])
		if err != nil {
			return fmt.Errorf("%s.properties[%q]: %v", path, name, err)
		}
		if err := validateStrictSchemaNode(child, fmt.Sprintf("%s.properties[%q]", path, name)); err != nil {
			return err
		}
	}
	return nil
}

func strictSchemaTypes(raw json.RawMessage) (map[string]struct{}, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		if err := validateStrictSchemaType(single); err != nil {
			return nil, true, err
		}
		return map[string]struct{}{single: {}}, true, nil
	}
	var multiple []string
	if err := json.Unmarshal(raw, &multiple); err != nil || len(multiple) == 0 {
		return nil, true, errors.New("must be a schema type or non-empty type array")
	}
	result := make(map[string]struct{}, len(multiple))
	for _, candidate := range multiple {
		if err := validateStrictSchemaType(candidate); err != nil {
			return nil, true, err
		}
		if _, duplicate := result[candidate]; duplicate {
			return nil, true, fmt.Errorf("contains duplicate type %q", candidate)
		}
		result[candidate] = struct{}{}
	}
	return result, true, nil
}

func validateStrictSchemaType(value string) error {
	switch value {
	case "null", "boolean", "object", "array", "number", "integer", "string":
		return nil
	default:
		return fmt.Errorf("unsupported schema type %q", value)
	}
}

func strictRequiredNames(raw json.RawMessage) ([]string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, errors.New("must be an array")
	}
	var names []string
	if err := json.Unmarshal(trimmed, &names); err != nil {
		return nil, errors.New("must be an array of strings")
	}
	return names, nil
}

func strictChildSchema(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(raw, &schema); err != nil || schema == nil {
		return nil, errors.New("must be a schema object")
	}
	return schema, nil
}

func validateStrictSchemaAlternatives(raw json.RawMessage, path string) error {
	var alternatives []json.RawMessage
	if err := json.Unmarshal(raw, &alternatives); err != nil || len(alternatives) == 0 {
		return fmt.Errorf("%s must be a non-empty schema array", path)
	}
	for index, alternative := range alternatives {
		schema, err := strictChildSchema(alternative)
		if err != nil {
			return fmt.Errorf("%s[%d]: %v", path, index, err)
		}
		if err := validateStrictSchemaNode(schema, fmt.Sprintf("%s[%d]", path, index)); err != nil {
			return err
		}
	}
	return nil
}

func validateStrictSchemaDefinitions(raw json.RawMessage, path string) error {
	var definitions map[string]json.RawMessage
	if err := json.Unmarshal(raw, &definitions); err != nil || definitions == nil {
		return fmt.Errorf("%s must be an object", path)
	}
	names := make([]string, 0, len(definitions))
	for name := range definitions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		schema, err := strictChildSchema(definitions[name])
		if err != nil {
			return fmt.Errorf("%s[%q]: %v", path, name, err)
		}
		if err := validateStrictSchemaNode(schema, fmt.Sprintf("%s[%q]", path, name)); err != nil {
			return err
		}
	}
	return nil
}

func (d ToolDefinition) Name() string        { return d.name }
func (d ToolDefinition) Description() string { return d.description }
func (d ToolDefinition) Strict() bool        { return d.strict }
func (d ToolDefinition) ParametersJSON() []byte {
	return bytes.Clone(d.parameters)
}
