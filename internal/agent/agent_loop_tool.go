package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// AgentLoopToolAdapter makes an existing raw ToolExecutor usable by the
// structured AgentLoop tool boundary. Prepare is an optional extension seam;
// JSON Schema validation runs after it.
type AgentLoopToolAdapter struct {
	definition provider.ToolDefinition
	executor   ToolExecutor
	prepare    func(any) (any, error)
}

func NewAgentLoopToolAdapter(definition provider.ToolDefinition, executor ToolExecutor, prepare func(any) (any, error)) (*AgentLoopToolAdapter, error) {
	if isNilInterface(executor) {
		return nil, fmt.Errorf("%w: tool executor is required", ErrInvalidConfig)
	}
	supported, err := supportsToolCall(executor, definition.Name())
	if err != nil || !supported {
		if err == nil {
			err = fmt.Errorf("executor does not support %q", definition.Name())
		}
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	return &AgentLoopToolAdapter{definition: definition, executor: executor, prepare: prepare}, nil
}

func (t *AgentLoopToolAdapter) Definition() provider.ToolDefinition { return t.definition }
func (t *AgentLoopToolAdapter) PrepareArguments(arguments any) (any, error) {
	if t.prepare == nil {
		return arguments, nil
	}
	return t.prepare(arguments)
}
func (t *AgentLoopToolAdapter) ExecutionMode() ToolExecutionMode {
	if override, ok := t.executor.(ToolExecutionOverride); ok {
		if mode, set := override.ToolExecutionMode(t.definition.Name()); set {
			return mode
		}
	}
	return ToolExecutionParallel
}
func (t *AgentLoopToolAdapter) Execute(ctx context.Context, toolCallID string, arguments any, report func(ToolUpdate)) (ToolOutput, error) {
	raw, err := json.Marshal(arguments)
	if err != nil {
		return ToolOutput{}, err
	}
	return executeNamedToolSafely(t.executor, ctx, toolCallID, t.definition.Name(), raw, report)
}

func agentLoopToolDefinitions(tools []AgentLoopTool) []provider.ToolDefinition {
	definitions := make([]provider.ToolDefinition, 0, len(tools))
	for _, tool := range tools {
		if !isNilInterface(tool) {
			definitions = append(definitions, tool.Definition())
		}
	}
	return definitions
}

func findAgentLoopTool(tools []AgentLoopTool, name string) AgentLoopTool {
	for _, tool := range tools {
		if !isNilInterface(tool) && tool.Definition().Name() == name {
			return tool
		}
	}
	return nil
}

func effectiveAgentLoopToolExecutionMode(global ToolExecutionMode, tools []AgentLoopTool) ToolExecutionMode {
	if global != ToolExecutionParallel {
		return ToolExecutionSequential
	}
	for _, tool := range tools {
		if isNilInterface(tool) {
			continue
		}
		if override, ok := tool.(AgentLoopToolExecutionOverride); ok && override.ExecutionMode() != ToolExecutionParallel {
			return ToolExecutionSequential
		}
	}
	return ToolExecutionParallel
}

func effectiveAgentLoopToolExecutionModeForCalls(global ToolExecutionMode, tools []AgentLoopTool, calls []llm.ToolCallBlock) ToolExecutionMode {
	selected := make([]AgentLoopTool, 0, len(calls))
	for _, call := range calls {
		if tool := findAgentLoopTool(tools, call.Name()); tool != nil {
			selected = append(selected, tool)
		}
	}
	return effectiveAgentLoopToolExecutionMode(global, selected)
}

func decodeAgentLoopToolArguments(raw []byte) (any, error) {
	var arguments any
	if err := json.Unmarshal(raw, &arguments); err != nil {
		return nil, err
	}
	return arguments, nil
}

func validateAndCoerceAgentLoopArguments(definition provider.ToolDefinition, arguments any) (any, error) {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return nil, fmt.Errorf("Validation failed for tool %q: arguments are not JSON-like: %w", definition.Name(), err)
	}
	var clone any
	if err := json.Unmarshal(encoded, &clone); err != nil {
		return nil, fmt.Errorf("Validation failed for tool %q: %w", definition.Name(), err)
	}
	var schema any
	if err := json.Unmarshal(definition.ParametersJSON(), &schema); err != nil {
		return nil, fmt.Errorf("Validation failed for tool %q: invalid schema: %w", definition.Name(), err)
	}
	coerced := coerceAgentLoopSchema(clone, schema)
	validator, err := compiledAgentLoopSchema(definition.ParametersJSON())
	if err != nil {
		return nil, fmt.Errorf("Validation failed for tool %q: invalid schema: %w", definition.Name(), err)
	}
	if err := validator.Validate(coerced); err != nil {
		received, _ := json.MarshalIndent(arguments, "", "  ")
		return nil, fmt.Errorf("Validation failed for tool %q:\n  - %s\n\nReceived arguments:\n%s", definition.Name(), err, received)
	}
	return coerced, nil
}

func coerceAgentLoopSchema(value any, rawSchema any) any {
	schema, ok := rawSchema.(map[string]any)
	if !ok {
		return value
	}
	next := value
	if allOf, ok := schema["allOf"].([]any); ok {
		for _, nested := range allOf {
			next = coerceAgentLoopSchema(next, nested)
		}
	}
	if anyOf, ok := schema["anyOf"].([]any); ok {
		next = coerceAgentLoopUnion(next, anyOf)
	}
	if oneOf, ok := schema["oneOf"].([]any); ok {
		next = coerceAgentLoopUnion(next, oneOf)
	}

	types := agentLoopSchemaTypes(schema["type"])
	matchesUnionMember := len(types) > 1 && agentLoopValueMatchesAnyType(next, types)
	if len(types) > 0 && !matchesUnionMember {
		for _, typ := range types {
			if candidate, changed := coerceAgentLoopPrimitive(next, typ); changed {
				next = candidate
				break
			}
		}
	}

	if containsType(types, "object") {
		if object, ok := next.(map[string]any); ok {
			properties, _ := schema["properties"].(map[string]any)
			for name, propertySchema := range properties {
				if property, exists := object[name]; exists {
					object[name] = coerceAgentLoopSchema(property, propertySchema)
				}
			}
			if additionalSchema, ok := schema["additionalProperties"].(map[string]any); ok {
				for name, property := range object {
					if _, defined := properties[name]; !defined {
						object[name] = coerceAgentLoopSchema(property, additionalSchema)
					}
				}
			}
		}
	}
	if containsType(types, "array") {
		if array, ok := next.([]any); ok {
			switch items := schema["items"].(type) {
			case []any:
				for index := 0; index < len(array) && index < len(items); index++ {
					array[index] = coerceAgentLoopSchema(array[index], items[index])
				}
			case map[string]any:
				for index := range array {
					array[index] = coerceAgentLoopSchema(array[index], items)
				}
			}
		}
	}
	return next
}

func coerceAgentLoopUnion(value any, schemas []any) any {
	for _, schema := range schemas {
		candidate, err := cloneJSONValue(value)
		if err != nil {
			continue
		}
		candidate = coerceAgentLoopSchema(candidate, schema)
		validator, err := compileAgentLoopSchemaDocument(schema)
		if err == nil && validator.Validate(candidate) == nil {
			return candidate
		}
	}
	return value
}

func cloneJSONValue(value any) (any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var clone any
	err = json.Unmarshal(raw, &clone)
	return clone, err
}

var agentLoopSchemaCache sync.Map

type rejectingAgentLoopSchemaLoader struct{}

func (rejectingAgentLoopSchemaLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema reference %q is not allowed", url)
}

func compiledAgentLoopSchema(raw []byte) (*jsonschema.Schema, error) {
	key := string(raw)
	if cached, ok := agentLoopSchemaCache.Load(key); ok {
		return cached.(*jsonschema.Schema), nil
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	compiled, err := compileAgentLoopSchemaDocument(document)
	if err != nil {
		return nil, err
	}
	actual, _ := agentLoopSchemaCache.LoadOrStore(key, compiled)
	return actual.(*jsonschema.Schema), nil
}

func compileAgentLoopSchemaDocument(document any) (*jsonschema.Schema, error) {
	compiler := jsonschema.NewCompiler()
	// TypeBox 1.x targets 2020-12. validation.ts also accepts the legacy tuple
	// form items: [...], so schema documents using that form select draft 2019.
	if agentLoopSchemaUsesLegacyTupleItems(document) {
		compiler.DefaultDraft(jsonschema.Draft2019)
	} else {
		compiler.DefaultDraft(jsonschema.Draft2020)
	}
	compiler.UseLoader(rejectingAgentLoopSchemaLoader{})
	const resource = "urn:pi-go:agent-loop:tool-schema"
	if err := compiler.AddResource(resource, document); err != nil {
		return nil, err
	}
	return compiler.Compile(resource)
}

func agentLoopSchemaUsesLegacyTupleItems(value any) bool {
	switch value := value.(type) {
	case map[string]any:
		if _, legacy := value["items"].([]any); legacy {
			return true
		}
		for _, nested := range value {
			if agentLoopSchemaUsesLegacyTupleItems(nested) {
				return true
			}
		}
	case []any:
		for _, nested := range value {
			if agentLoopSchemaUsesLegacyTupleItems(nested) {
				return true
			}
		}
	}
	return false
}

func agentLoopSchemaTypes(raw any) []string {
	switch value := raw.(type) {
	case string:
		return []string{value}
	case []any:
		result := make([]string, 0, len(value))
		for _, candidate := range value {
			if item, ok := candidate.(string); ok {
				result = append(result, item)
			}
		}
		return result
	default:
		return nil
	}
}

func agentLoopValueMatchesAnyType(value any, types []string) bool {
	for _, typ := range types {
		if agentLoopValueMatchesType(value, typ) {
			return true
		}
	}
	return false
}

func agentLoopValueMatchesType(value any, typ string) bool {
	switch typ {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		_, ok := value.(float64)
		return ok
	case "integer":
		number, ok := value.(float64)
		return ok && math.Trunc(number) == number
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	default:
		return false
	}
}

func coerceAgentLoopPrimitive(value any, typ string) (any, bool) {
	switch typ {
	case "number", "integer":
		var number float64
		switch typed := value.(type) {
		case string:
			if strings.TrimSpace(typed) == "" {
				return nil, false
			}
			parsed, ok := parseAgentLoopNumber(typed)
			if !ok {
				return nil, false
			}
			number = parsed
		case bool:
			if typed {
				number = 1
			}
		case nil:
			number = 0
		default:
			return nil, false
		}
		if typ == "integer" && math.Trunc(number) != number {
			return nil, false
		}
		return number, true
	case "boolean":
		switch typed := value.(type) {
		case nil:
			return false, true
		case string:
			if typed == "true" {
				return true, true
			}
			if typed == "false" {
				return false, true
			}
		case float64:
			if typed == 1 {
				return true, true
			}
			if typed == 0 {
				return false, true
			}
		}
	case "string":
		switch typed := value.(type) {
		case nil:
			return "", true
		case bool:
			return strconv.FormatBool(typed), true
		case float64:
			return formatAgentLoopNumber(typed), true
		}
	case "null":
		switch typed := value.(type) {
		case string:
			if typed == "" {
				return nil, true
			}
		case float64:
			if typed == 0 {
				return nil, true
			}
		case bool:
			if !typed {
				return nil, true
			}
		}
	}
	return nil, false
}

func parseAgentLoopNumber(value string) (float64, bool) {
	trimmed := strings.TrimSpace(value)
	if parsed, err := strconv.ParseFloat(trimmed, 64); err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0) {
		return parsed, true
	}
	if len(trimmed) > 2 && trimmed[0] == '0' {
		base := 0
		switch trimmed[1] {
		case 'x', 'X':
			base = 16
		case 'b', 'B':
			base = 2
		case 'o', 'O':
			base = 8
		}
		if base != 0 {
			parsed, err := strconv.ParseUint(trimmed[2:], base, 64)
			if err == nil {
				return float64(parsed), true
			}
		}
	}
	return 0, false
}

func formatAgentLoopNumber(value float64) string {
	if value == 0 {
		return "0"
	}
	abs := math.Abs(value)
	if abs >= 1e21 || abs < 1e-6 {
		formatted := strconv.FormatFloat(value, 'e', -1, 64)
		parts := strings.SplitN(formatted, "e", 2)
		exponent, err := strconv.Atoi(parts[1])
		if err == nil {
			return fmt.Sprintf("%se%+d", parts[0], exponent)
		}
		return formatted
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func containsType(types []string, want string) bool {
	for _, typ := range types {
		if typ == want {
			return true
		}
	}
	return false
}

func executeAgentLoopToolSafely(tool AgentLoopTool, ctx context.Context, id string, arguments any, report func(ToolUpdate)) (output ToolOutput, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("tool panicked: %s", safeValueText(recovered))
			output = ToolOutput{Text: safeErrorText(err)}
		}
	}()
	return tool.Execute(ctx, id, arguments, report)
}

func prepareAgentLoopToolArgumentsSafely(preparer AgentLoopToolArgumentPreparer, arguments any) (result any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("tool argument preparation panicked: %s", safeValueText(recovered))
		}
	}()
	return preparer.PrepareArguments(arguments)
}

func callAgentLoopBeforeToolHook(hook AgentLoopBeforeToolCallHook, ctx context.Context, input AgentLoopBeforeToolCallContext) (result AgentLoopBeforeToolCallResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("before tool hook panicked: %s", safeValueText(recovered))
		}
	}()
	return hook(ctx, input)
}

func callAgentLoopAfterToolHook(hook AgentLoopAfterToolCallHook, ctx context.Context, input AgentLoopAfterToolCallContext) (result AgentLoopAfterToolCallResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("after tool hook panicked: %s", safeValueText(recovered))
		}
	}()
	return hook(ctx, input)
}
