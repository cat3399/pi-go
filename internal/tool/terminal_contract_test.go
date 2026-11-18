package tool

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/cat3399/pi-go/internal/terminal"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func compileTerminalSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	var document any
	if err := json.Unmarshal(terminalSpecification().ParametersJSON(), &document); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("terminal.json", document); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("terminal.json")
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

// Strict Responses backends (including the Codex backend used by GPT-6.2)
// reject combinators at the root of function parameters, which fails the
// whole request rather than only this tool.
func TestTerminalSchemaAvoidsTopLevelCombinators(t *testing.T) {
	var document map[string]any
	if err := json.Unmarshal(terminalSpecification().ParametersJSON(), &document); err != nil {
		t.Fatal(err)
	}
	if document["type"] != "object" {
		t.Fatalf("terminal schema root type = %v, want object", document["type"])
	}
	for _, keyword := range []string{"oneOf", "anyOf", "allOf", "not", "if", "then", "else", "enum"} {
		if _, present := document[keyword]; present {
			t.Fatalf("terminal schema must not use top-level %q", keyword)
		}
	}
}

func TestTerminalSchemaKeepsPropertyConstraints(t *testing.T) {
	schema := compileTerminalSchema(t)
	cases := []struct {
		arguments string
		valid     bool
	}{
		{`{"action":"open"}`, true},
		{`{"action":"list"}`, true},
		{`{"action":"run","id":"term_example","command":"pwd"}`, true},
		{`{"action":"read","id":"term_example"}`, true},
		{`{"action":"write","id":"term_example","input":"yes\n"}`, true},
		{`{"action":"write","id":"term_example","input":""}`, true},
		{`{"action":"interrupt","id":"term_example"}`, true},
		{`{"action":"close","id":"term_example"}`, true},
		{`{}`, false},
		{`{"action":"resize","id":"term_example"}`, false},
		{`{"action":"run","id":"","command":"pwd"}`, false},
		{`{"action":"run","id":" \t","command":"pwd"}`, false},
		{`{"action":"run","id":"term_example","command":" \n"}`, false},
		{`{"action":"open","unknown":true}`, false},
	}
	for _, test := range cases {
		t.Run(test.arguments, func(t *testing.T) {
			var arguments any
			if err := json.Unmarshal([]byte(test.arguments), &arguments); err != nil {
				t.Fatal(err)
			}
			if err := schema.Validate(arguments); (err == nil) != test.valid {
				t.Fatalf("valid=%t, want %t: %v", err == nil, test.valid, err)
			}
		})
	}
}

func TestTerminalToolRequiresActionSpecificArguments(t *testing.T) {
	// New is lazy; these calls fail validation before any shell is started.
	tool := terminalTool{service: terminal.New(terminal.Options{})}
	cases := []struct {
		arguments string
		want      string
	}{
		{`{"action":"run","command":"pwd"}`, "terminal id is required"},
		{`{"action":"run","id":"term_example"}`, "command is required"},
		{`{"action":"read"}`, "terminal id is required"},
		{`{"action":"write","id":"term_example"}`, "input is required for write"},
		{`{"action":"interrupt"}`, "terminal id is required"},
		{`{"action":"close"}`, "terminal id is required"},
	}
	for _, test := range cases {
		t.Run(test.arguments, func(t *testing.T) {
			result, err := tool.ExecuteJSON(context.Background(), []byte(test.arguments))
			if err == nil || err.Error() != test.want {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if result.Text != test.want {
				t.Fatalf("result text = %q, want %q", result.Text, test.want)
			}
		})
	}
}

func TestTerminalToolAcceptsExplicitEmptyWriteInput(t *testing.T) {
	tool := terminalTool{service: terminal.New(terminal.Options{})}
	_, err := tool.ExecuteJSON(context.Background(), []byte(`{"action":"write","id":"term_missing","input":""}`))
	if !errors.Is(err, terminal.ErrNotFound) {
		t.Fatalf("error = %v, want terminal.ErrNotFound after argument checks", err)
	}
}
