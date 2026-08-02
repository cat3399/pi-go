package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
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
	var document any
	if err := json.Unmarshal(d.parameters, &document); err != nil {
		return fmt.Errorf("%w: parameters: %v", ErrInvalidToolDefinition, err)
	}
	object, ok := document.(map[string]any)
	if !ok {
		return fmt.Errorf("%w: parameters: top-level value is not an object", ErrInvalidToolDefinition)
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
func validateStrictFunctionParameters(root map[string]any) error {
	rootType, ok := root["type"].(string)
	if !ok || rootType != "object" {
		return errors.New("root type must be object")
	}
	if _, anyOf := root["anyOf"]; anyOf {
		return errors.New("root must not use anyOf")
	}
	validator := strictSchemaValidator{
		root:    root,
		visited: make(map[string]struct{}),
		active:  make(map[string]struct{}),
	}
	return validator.validateNode(root, "#", 0)
}

const (
	maxStrictSchemaTraversalDepth = 256
	maxStrictSchemaNodes          = 10_000
)

// strictSchemaValidator owns the full schema document so every local $ref can
// be resolved exactly. Locations are canonical JSON Pointers, which also give
// visited/active stable identities for shared and recursive schema nodes.
type strictSchemaValidator struct {
	root      map[string]any
	visited   map[string]struct{}
	active    map[string]struct{}
	nodeCount int
}

func (v *strictSchemaValidator) validateNode(schema map[string]any, location string, depth int) error {
	if _, visited := v.visited[location]; visited {
		return nil
	}
	if _, active := v.active[location]; active {
		// A local ref back to an active schema is legal recursion. The active
		// invocation remains responsible for validating that schema's body.
		return nil
	}
	if depth > maxStrictSchemaTraversalDepth {
		return fmt.Errorf("%s exceeds strict schema traversal depth %d", location, maxStrictSchemaTraversalDepth)
	}
	if v.nodeCount >= maxStrictSchemaNodes {
		return fmt.Errorf("strict schema exceeds %d schema nodes", maxStrictSchemaNodes)
	}
	v.nodeCount++
	v.active[location] = struct{}{}
	defer delete(v.active, location)

	for _, keyword := range []string{"allOf", "oneOf", "not", "dependentRequired", "dependentSchemas", "if", "then", "else"} {
		if _, present := schema[keyword]; present {
			return fmt.Errorf("%s/%s is not supported in strict mode", location, strictJSONPointerEscape(keyword))
		}
	}
	rawType, typePresent := schema["type"]
	types, hasType, err := strictSchemaTypes(rawType, typePresent)
	if err != nil {
		return fmt.Errorf("%s/type: %v", location, err)
	}
	if !hasType {
		_, hasReference := schema["$ref"]
		_, hasAnyOf := schema["anyOf"]
		if !hasReference && !hasAnyOf {
			return fmt.Errorf("%s must declare type, $ref, or anyOf", location)
		}
	}
	if _, object := types["object"]; object {
		if err := v.validateObjectSchema(schema, location, depth); err != nil {
			return err
		}
	} else {
		for _, keyword := range []string{"properties", "required", "additionalProperties"} {
			if _, present := schema[keyword]; present {
				return fmt.Errorf("%s/%s requires object type", location, strictJSONPointerEscape(keyword))
			}
		}
	}
	if _, array := types["array"]; array {
		items, present := schema["items"]
		if !present {
			return fmt.Errorf("%s/items is required for array type", location)
		}
		itemSchema, err := strictChildSchema(items)
		if err != nil {
			return fmt.Errorf("%s/items: %v", location, err)
		}
		if err := v.validateNode(itemSchema, strictJSONPointerAppend(location, "items"), depth+1); err != nil {
			return err
		}
	}
	if raw, present := schema["anyOf"]; present {
		if err := v.validateAlternatives(raw, strictJSONPointerAppend(location, "anyOf"), depth); err != nil {
			return err
		}
	}
	if raw, present := schema["$defs"]; present {
		if err := v.validateDefinitions(raw, strictJSONPointerAppend(location, "$defs"), depth); err != nil {
			return err
		}
	}
	if raw, present := schema["$ref"]; present {
		reference, ok := raw.(string)
		if !ok {
			return fmt.Errorf("%s/$ref must be a string local reference", location)
		}
		target, targetLocation, err := v.resolveLocalReference(reference)
		if err != nil {
			return fmt.Errorf("%s/$ref: %v", location, err)
		}
		targetSchema, ok := target.(map[string]any)
		if !ok {
			return fmt.Errorf("%s/$ref target %q must be a schema object", location, reference)
		}
		if err := v.validateNode(targetSchema, targetLocation, depth+1); err != nil {
			return fmt.Errorf("%s/$ref target %q: %v", location, reference, err)
		}
	}
	v.visited[location] = struct{}{}
	return nil
}

func (v *strictSchemaValidator) validateObjectSchema(schema map[string]any, location string, depth int) error {
	rawAdditional, present := schema["additionalProperties"]
	if !present {
		return fmt.Errorf("%s/additionalProperties must be boolean false", location)
	}
	additional, ok := rawAdditional.(bool)
	if !ok || additional {
		return fmt.Errorf("%s/additionalProperties must be boolean false", location)
	}

	rawProperties, present := schema["properties"]
	if !present {
		return fmt.Errorf("%s/properties is required", location)
	}
	properties, ok := rawProperties.(map[string]any)
	if !ok {
		return fmt.Errorf("%s/properties must be an object", location)
	}
	rawRequired, present := schema["required"]
	if !present {
		return fmt.Errorf("%s/required must include every property", location)
	}
	required, err := strictRequiredNames(rawRequired)
	if err != nil {
		return fmt.Errorf("%s/required: %v", location, err)
	}
	seenRequired := make(map[string]struct{}, len(required))
	for _, name := range required {
		if _, duplicate := seenRequired[name]; duplicate {
			return fmt.Errorf("%s/required contains duplicate %q", location, name)
		}
		if _, declared := properties[name]; !declared {
			return fmt.Errorf("%s/required contains undeclared property %q", location, name)
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
			return fmt.Errorf("%s/required omits property %q", location, name)
		}
		child, err := strictChildSchema(properties[name])
		if err != nil {
			return fmt.Errorf("%s/properties/%s: %v", location, strictJSONPointerEscape(name), err)
		}
		childLocation := strictJSONPointerAppend(location, "properties", name)
		if err := v.validateNode(child, childLocation, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func strictSchemaTypes(raw any, present bool) (map[string]struct{}, bool, error) {
	if !present {
		return nil, false, nil
	}
	if single, ok := raw.(string); ok {
		if err := validateStrictSchemaType(single); err != nil {
			return nil, true, err
		}
		return map[string]struct{}{single: {}}, true, nil
	}
	multiple, ok := raw.([]any)
	if !ok || len(multiple) == 0 {
		return nil, true, errors.New("must be a schema type or non-empty type array")
	}
	result := make(map[string]struct{}, len(multiple))
	for _, rawCandidate := range multiple {
		candidate, ok := rawCandidate.(string)
		if !ok {
			return nil, true, errors.New("type array must contain only strings")
		}
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

func strictRequiredNames(raw any) ([]string, error) {
	values, ok := raw.([]any)
	if !ok {
		return nil, errors.New("must be an array")
	}
	names := make([]string, len(values))
	for index, value := range values {
		name, ok := value.(string)
		if !ok {
			return nil, errors.New("must be an array of strings")
		}
		names[index] = name
	}
	return names, nil
}

func strictChildSchema(raw any) (map[string]any, error) {
	schema, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("must be a schema object")
	}
	return schema, nil
}

func (v *strictSchemaValidator) validateAlternatives(raw any, location string, depth int) error {
	alternatives, ok := raw.([]any)
	if !ok || len(alternatives) == 0 {
		return fmt.Errorf("%s must be a non-empty schema array", location)
	}
	for index, alternative := range alternatives {
		schema, err := strictChildSchema(alternative)
		if err != nil {
			return fmt.Errorf("%s/%d: %v", location, index, err)
		}
		if err := v.validateNode(schema, strictJSONPointerAppend(location, strconv.Itoa(index)), depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (v *strictSchemaValidator) validateDefinitions(raw any, location string, depth int) error {
	definitions, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("%s must be an object", location)
	}
	names := make([]string, 0, len(definitions))
	for name := range definitions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		schema, err := strictChildSchema(definitions[name])
		if err != nil {
			return fmt.Errorf("%s/%s: %v", location, strictJSONPointerEscape(name), err)
		}
		if err := v.validateNode(schema, strictJSONPointerAppend(location, name), depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (v *strictSchemaValidator) resolveLocalReference(reference string) (any, string, error) {
	if !strings.HasPrefix(reference, "#") {
		return nil, "", fmt.Errorf("remote reference %q is not supported", reference)
	}
	fragment, err := url.PathUnescape(strings.TrimPrefix(reference, "#"))
	if err != nil {
		return nil, "", fmt.Errorf("reference %q has invalid percent encoding", reference)
	}
	if fragment == "" {
		return v.root, "#", nil
	}
	if !strings.HasPrefix(fragment, "/") {
		return nil, "", fmt.Errorf("reference %q is not a JSON Pointer", reference)
	}

	encodedTokens := strings.Split(strings.TrimPrefix(fragment, "/"), "/")
	if len(encodedTokens) > maxStrictSchemaTraversalDepth {
		return nil, "", fmt.Errorf("reference %q exceeds JSON Pointer depth %d", reference, maxStrictSchemaTraversalDepth)
	}
	current := any(v.root)
	location := "#"
	for _, encodedToken := range encodedTokens {
		token, err := strictJSONPointerUnescape(encodedToken)
		if err != nil {
			return nil, "", fmt.Errorf("reference %q: %v", reference, err)
		}
		location = strictJSONPointerAppend(location, token)
		switch container := current.(type) {
		case map[string]any:
			next, present := container[token]
			if !present {
				return nil, "", fmt.Errorf("reference %q target does not exist", reference)
			}
			current = next
		case []any:
			index, err := strictJSONArrayIndex(token)
			if err != nil || index >= len(container) {
				return nil, "", fmt.Errorf("reference %q has invalid array index %q", reference, token)
			}
			current = container[index]
		default:
			return nil, "", fmt.Errorf("reference %q traverses a non-container value", reference)
		}
	}
	return current, location, nil
}

func strictJSONPointerAppend(location string, tokens ...string) string {
	for _, token := range tokens {
		location += "/" + strictJSONPointerEscape(token)
	}
	return location
}

func strictJSONPointerEscape(token string) string {
	token = strings.ReplaceAll(token, "~", "~0")
	return strings.ReplaceAll(token, "/", "~1")
}

func strictJSONPointerUnescape(token string) (string, error) {
	if !strings.Contains(token, "~") {
		return token, nil
	}
	var result strings.Builder
	result.Grow(len(token))
	for index := 0; index < len(token); index++ {
		if token[index] != '~' {
			result.WriteByte(token[index])
			continue
		}
		if index+1 >= len(token) {
			return "", errors.New("JSON Pointer token ends with an incomplete '~' escape")
		}
		index++
		switch token[index] {
		case '0':
			result.WriteByte('~')
		case '1':
			result.WriteByte('/')
		default:
			return "", fmt.Errorf("JSON Pointer token contains invalid escape ~%c", token[index])
		}
	}
	return result.String(), nil
}

func strictJSONArrayIndex(token string) (int, error) {
	if token == "" || token == "-" || len(token) > 1 && token[0] == '0' {
		return 0, errors.New("invalid array index")
	}
	for _, character := range token {
		if character < '0' || character > '9' {
			return 0, errors.New("invalid array index")
		}
	}
	value, err := strconv.ParseUint(token, 10, 64)
	if err != nil || value > uint64(^uint(0)>>1) {
		return 0, errors.New("invalid array index")
	}
	return int(value), nil
}

func (d ToolDefinition) Name() string        { return d.name }
func (d ToolDefinition) Description() string { return d.description }
func (d ToolDefinition) Strict() bool        { return d.strict }
func (d ToolDefinition) ParametersJSON() []byte {
	return bytes.Clone(d.parameters)
}
