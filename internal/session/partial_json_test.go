package session

import (
	"reflect"
	"testing"
)

func TestParseStreamingJSONObjectMatchesPiPartialArguments(t *testing.T) {
	tests := []struct {
		name     string
		fragment string
		want     map[string]any
	}{
		{name: "empty", fragment: "", want: map[string]any{}},
		{name: "missing value", fragment: `{"text":`, want: map[string]any{}},
		{name: "partial string", fragment: `{"text":"hel`, want: map[string]any{"text": "hel"}},
		{name: "nested", fragment: `{"items":[{"name":"one"},{"name":"tw`, want: map[string]any{
			"items": []any{map[string]any{"name": "one"}, map[string]any{"name": "tw"}},
		}},
		{name: "partial literal", fragment: `{"enabled":tru`, want: map[string]any{"enabled": true}},
		{name: "partial exponent", fragment: `{"value":12e`, want: map[string]any{"value": float64(12)}},
		{name: "incomplete raw newline", fragment: "{\"text\":\"one\ntwo", want: map[string]any{}},
		{name: "incomplete invalid escape", fragment: `{"path":"a\q`, want: map[string]any{"path": "a"}},
		{name: "complete invalid escape repair", fragment: `{"path":"a\q"}`, want: map[string]any{"path": `a\q`}},
		{name: "malformed root", fragment: `wrong`, want: map[string]any{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := parseStreamingJSONObject([]byte(test.fragment)); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("parseStreamingJSONObject(%q) = %#v, want %#v", test.fragment, got, test.want)
			}
		})
	}
}
