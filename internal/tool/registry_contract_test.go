package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"strings"
	"testing"
)

func TestBashSpecificationTimeoutSchemaMatchesDecoder(t *testing.T) {
	t.Parallel()

	schema := decodeSpecificationSchema(t, bashSpecification())
	if _, present := schema["additionalProperties"]; present {
		t.Fatal("upstream Type.Object schema unexpectedly advertises additionalProperties")
	}
	var required []string
	if err := json.Unmarshal(schema["required"], &required); err != nil {
		t.Fatal(err)
	}
	if len(required) != 1 || required[0] != "command" {
		t.Fatalf("required = %#v, want [command]", required)
	}

	timeout := schemaProperty(t, schema, "timeout")
	if got := string(timeout["type"]); got != `"number"` {
		t.Fatalf("timeout type = %s, want number", got)
	}
	if _, present := timeout["minimum"]; present {
		t.Fatal("upstream timeout schema unexpectedly advertises runtime validation")
	}
	if _, present := timeout["exclusiveMinimum"]; present {
		t.Fatal("upstream timeout schema unexpectedly advertises runtime validation")
	}
	if _, present := timeout["maximum"]; present {
		t.Fatal("upstream timeout schema unexpectedly advertises runtime validation")
	}

	cases := []struct {
		name        string
		timeoutJSON string
	}{
		{name: "integer", timeoutJSON: "1"},
		{name: "sub-nanosecond positive", timeoutJSON: "0.0000000001"},
		{name: "maximum", timeoutJSON: "2147483.647"},
		{name: "zero", timeoutJSON: "0"},
		{name: "negative", timeoutJSON: "-1"},
		{name: "above maximum", timeoutJSON: "2147483.6470000001"},
		{name: "string", timeoutJSON: `"1"`},
		{name: "null", timeoutJSON: "null"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			// TypeBox intentionally keeps the provider schema broad. Runtime
			// validation still rejects non-positive, non-finite, and overflowing
			// durations exactly as pi does.
			_, _ = DecodeBashInput([]byte(`{"command":"x","timeout":` + test.timeoutJSON + `}`))
		})
	}

	if _, err := DecodeBashInput([]byte(`{"command":"x"}`)); err != nil {
		t.Fatalf("optional timeout rejected: %v", err)
	}
	if _, err := DecodeBashInput([]byte(`{"timeout":1}`)); !errors.Is(err, ErrInvalidBashInput) {
		t.Fatalf("missing schema-required command error = %v", err)
	}
	if _, err := DecodeBashInput([]byte(`{"command":"x","extra":1}`)); err != nil {
		t.Fatalf("upstream Type.Object additional property rejected: %v", err)
	}
}

func TestBashSpecificationAdvertisesSessionEnvironmentGuideline(t *testing.T) {
	t.Parallel()
	guidelines := bashSpecification().PromptGuidelines()
	if len(guidelines) != 1 || !strings.Contains(guidelines[0], "PI_*") {
		t.Fatalf("bash prompt guidelines = %#v", guidelines)
	}
}

func TestBuiltInSpecificationsUseUpstreamNonStrictDefault(t *testing.T) {
	t.Parallel()

	specifications := append([]Specification{bashSpecification()}, filesystemSpecifications()...)
	if len(specifications) != 7 {
		t.Fatalf("built-in specification count = %d, want 7", len(specifications))
	}
	for _, specification := range specifications {
		if specification.Strict() {
			t.Fatalf("built-in specification %q advertises strict mode", specification.Name())
		}
	}
}

func TestFilesystemSpecificationsMatchUpstreamBroadSchemas(t *testing.T) {
	t.Parallel()

	readSchema := decodeSpecificationSchema(t, filesystemSpecification(t, ReadToolName))
	assertSchemaKeywordAbsent(t, readSchema, "path", "minLength")
	assertSchemaKeywordAbsent(t, readSchema, "offset", "minimum")
	assertSchemaKeywordAbsent(t, readSchema, "limit", "minimum")

	grepSchema := decodeSpecificationSchema(t, filesystemSpecification(t, GrepToolName))
	assertSchemaKeywordAbsent(t, grepSchema, "path", "minLength")
	assertSchemaKeywordAbsent(t, grepSchema, "context", "minimum")
	assertSchemaKeywordAbsent(t, grepSchema, "limit", "minimum")

	findSchema := decodeSpecificationSchema(t, filesystemSpecification(t, FindToolName))
	assertSchemaKeywordAbsent(t, findSchema, "path", "minLength")
	assertSchemaKeywordAbsent(t, findSchema, "limit", "minimum")

	lsSchema := decodeSpecificationSchema(t, filesystemSpecification(t, LsToolName))
	assertSchemaKeywordAbsent(t, lsSchema, "path", "minLength")
	assertSchemaKeywordAbsent(t, lsSchema, "limit", "minimum")

	editSchema := decodeSpecificationSchema(t, filesystemSpecification(t, EditToolName))
	edits := schemaProperty(t, editSchema, "edits")
	if _, present := edits["minItems"]; present {
		t.Fatal("upstream edits schema unexpectedly advertises minItems")
	}
	var itemSchema map[string]json.RawMessage
	if err := json.Unmarshal(edits["items"], &itemSchema); err != nil {
		t.Fatal(err)
	}
	assertSchemaKeywordAbsent(t, itemSchema, "oldText", "minLength")

	suite := newTestSuite(t)
	writeTestFile(t, suite.WorkingDir(), "notes.txt", "needle\n")
	valid := []struct {
		name string
		tool string
		raw  string
	}{
		{name: "read lower bounds", tool: ReadToolName, raw: `{"path":"notes.txt","offset":1,"limit":1}`},
		{name: "grep lower bounds", tool: GrepToolName, raw: `{"pattern":"needle","path":"notes.txt","context":0,"limit":1}`},
		{name: "find lower bound", tool: FindToolName, raw: `{"pattern":"*","limit":1}`},
		{name: "ls lower bound", tool: LsToolName, raw: `{"limit":1}`},
		{name: "grep clamps negative context", tool: GrepToolName, raw: `{"pattern":"needle","context":-1}`},
		{name: "find accepts zero limit", tool: FindToolName, raw: `{"pattern":"*","limit":0}`},
	}
	for _, test := range valid {
		t.Run(test.name, func(t *testing.T) {
			if _, err := suite.ExecuteJSON(context.Background(), test.tool, []byte(test.raw)); err != nil {
				t.Fatalf("ExecuteJSON() error = %v", err)
			}
		})
	}

	invalid := []struct {
		name string
		tool string
		raw  string
	}{
		{name: "empty edits", tool: EditToolName, raw: `{"path":"notes.txt","edits":[]}`},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if _, err := suite.ExecuteJSON(context.Background(), test.tool, []byte(test.raw)); !errors.Is(err, ErrInvalidFilesystemInput) {
				t.Fatalf("ExecuteJSON() error = %v, want ErrInvalidFilesystemInput", err)
			}
		})
	}
}

func assertSchemaKeywordAbsent(t *testing.T, schema map[string]json.RawMessage, property, keyword string) {
	t.Helper()
	value := schemaProperty(t, schema, property)
	if _, present := value[keyword]; present {
		t.Fatalf("%s.%s is present in upstream-compatible schema", property, keyword)
	}
}

func decodeSpecificationSchema(t *testing.T, specification Specification) map[string]json.RawMessage {
	t.Helper()
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(specification.ParametersJSON(), &schema); err != nil {
		t.Fatalf("decode schema for %q: %v", specification.Name(), err)
	}
	return schema
}

func schemaProperty(t *testing.T, schema map[string]json.RawMessage, name string) map[string]json.RawMessage {
	t.Helper()
	var properties map[string]map[string]json.RawMessage
	if err := json.Unmarshal(schema["properties"], &properties); err != nil {
		t.Fatalf("decode properties: %v", err)
	}
	property, ok := properties[name]
	if !ok {
		t.Fatalf("schema property %q is missing", name)
	}
	return property
}

func assertSchemaKeyword(t *testing.T, schema map[string]json.RawMessage, property, keyword, want string) {
	t.Helper()
	if got := string(schemaProperty(t, schema, property)[keyword]); got != want {
		t.Fatalf("%s.%s = %s, want %s", property, keyword, got, want)
	}
}

func filesystemSpecification(t *testing.T, name string) Specification {
	t.Helper()
	for _, specification := range filesystemSpecifications() {
		if specification.Name() == name {
			return specification
		}
	}
	t.Fatalf("filesystem specification %q is missing", name)
	return Specification{}
}

func boundedNumberSchemaAccepts(raw string, exclusiveMinimum, maximum json.RawMessage) bool {
	decoder := json.NewDecoder(bytes.NewReader([]byte(raw)))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return false
	}
	number, ok := decoded.(json.Number)
	if !ok {
		return false
	}
	value, ok := new(big.Rat).SetString(number.String())
	if !ok {
		return false
	}
	minimum, ok := new(big.Rat).SetString(string(exclusiveMinimum))
	if !ok || value.Cmp(minimum) <= 0 {
		return false
	}
	upper, ok := new(big.Rat).SetString(string(maximum))
	return ok && value.Cmp(upper) <= 0
}
