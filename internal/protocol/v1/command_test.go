package protocolv1

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/provider"
)

func TestAbortBranchSummaryCommandRoundTripBoundary(t *testing.T) {
	command, err := DecodeCommand([]byte(`{"type":"abort_branch_summary"}`), agent.InputRPC)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := command.(application.AbortBranchSummaryCommand); !ok {
		t.Fatalf("decoded command = %T", command)
	}
	data, present, err := EncodeResult(application.AbortBranchSummaryResult{})
	if err != nil {
		t.Fatal(err)
	}
	if present || data != nil {
		t.Fatalf("encoded result = (%#v, %v), want omitted", data, present)
	}
}

func TestCoreControlCommandsDecodeAndEncode(t *testing.T) {
	tests := []struct {
		input string
		want  any
	}{
		{`{"type":"cycle_model","direction":"backward"}`, application.CycleModelCommand{}},
		{`{"type":"get_available_models"}`, application.GetAvailableModelsCommand{}},
		{`{"type":"cycle_thinking_level"}`, application.CycleThinkingLevelCommand{}},
		{`{"type":"get_available_thinking_levels"}`, application.GetAvailableThinkingLevelsCommand{}},
		{`{"type":"set_steering_mode","mode":"all"}`, application.SetSteeringModeCommand{}},
		{`{"type":"set_follow_up_mode","mode":"one-at-a-time"}`, application.SetFollowUpModeCommand{}},
		{`{"type":"abort_retry"}`, application.AbortRetryCommand{}},
	}
	for _, test := range tests {
		command, err := DecodeCommand([]byte(test.input), agent.InputRPC)
		if err != nil {
			t.Fatalf("%s: %v", test.input, err)
		}
		if fmt.Sprintf("%T", command) != fmt.Sprintf("%T", test.want) {
			t.Fatalf("%s decoded %T, want %T", test.input, command, test.want)
		}
	}
	if _, err := DecodeCommand([]byte(`{"type":"set_steering_mode","mode":"invalid"}`), agent.InputRPC); err == nil {
		t.Fatal("invalid queue mode was accepted")
	}

	level := provider.ThinkingHigh
	data, present, err := EncodeResult(application.CycleThinkingLevelResult{Level: &level})
	if err != nil || !present {
		t.Fatalf("cycle thinking encode = (%#v, %v, %v)", data, present, err)
	}
	encoded := data.(map[string]any)
	if encoded["level"] != provider.ThinkingHigh {
		t.Fatalf("cycle thinking data = %#v", encoded)
	}
}

func TestGetToolsResultRetainsSchemaGuidelinesAndSource(t *testing.T) {
	baseDir := "/workspace/tools"
	data, present, err := EncodeResult(application.GetToolsResult{Tools: []application.ToolInfo{{
		Name: "read", Description: "Read a file", Parameters: json.RawMessage(`{"type":"object"}`),
		PromptGuidelines: []string{"Use absolute paths"}, Active: true,
		SourceInfo: agent.SystemPromptSourceInfo{
			Path: "/workspace/tools/read.go", Source: "fixture", Scope: agent.SystemPromptSourceProject,
			Origin: agent.SystemPromptSourcePackage, BaseDir: &baseDir,
		},
	}}})
	if err != nil || !present {
		t.Fatalf("get_tools encode = (%#v, %v, %v)", data, present, err)
	}
	tools, ok := data.([]map[string]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("encoded tools = %#v", data)
	}
	tool := tools[0]
	if string(tool["parameters"].(json.RawMessage)) != `{"type":"object"}` ||
		tool["promptGuidelines"].([]string)[0] != "Use absolute paths" || !tool["active"].(bool) {
		t.Fatalf("encoded tool = %#v", tool)
	}
	source := tool["sourceInfo"].(map[string]any)
	if source["path"] != "/workspace/tools/read.go" || source["baseDir"] != baseDir {
		t.Fatalf("encoded source = %#v", source)
	}
}
