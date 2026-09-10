package tool

import (
	"encoding/json"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestTerminalSchemaRequiresAnExplicitTargetForOperations(t *testing.T) {
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
	cases := []struct {
		arguments string
		valid     bool
	}{
		{`{"action":"open"}`, true},
		{`{"action":"list"}`, true},
		{`{"action":"run","id":"term_example","command":"pwd"}`, true},
		{`{"action":"read","id":"term_example"}`, true},
		{`{"action":"write","id":"term_example","input":"yes\n"}`, true},
		{`{"action":"interrupt","id":"term_example"}`, true},
		{`{"action":"close","id":"term_example"}`, true},
		{`{"action":"run","command":"pwd"}`, false},
		{`{"action":"run","id":"","command":"pwd"}`, false},
		{`{"action":"run","id":" \t","command":"pwd"}`, false},
		{`{"action":"run","id":"term_example"}`, false},
		{`{"action":"run","id":"term_example","command":" \n"}`, false},
		{`{"action":"read"}`, false},
		{`{"action":"write","id":"term_example"}`, false},
		{`{"action":"interrupt"}`, false},
		{`{"action":"close"}`, false},
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
