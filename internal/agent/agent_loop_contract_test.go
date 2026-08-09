package agent_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

func TestAgentLoopInvocationStateIsIsolatedAcrossRepeatedAndConcurrentRuns(t *testing.T) {
	base := mustLoopModel(t, "base", provider.CostRates{})
	modelA := mustLoopModel(t, "model-a", provider.CostRates{})
	modelB := mustLoopModel(t, "model-b", provider.CostRates{})
	definition := mustLoopDefinition(t, "echo", `{"type":"object"}`)
	tool := &loopContractTool{definition: definition, execute: func(context.Context, string, any, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		return agent.ToolOutput{Text: "ok"}, nil
	}}
	call := mustLoopCall(t, "call-1", "echo", `{}`)

	var requestMu sync.Mutex
	var requests []provider.Request
	runtimeProvider := loopProviderFunc(func(ctx context.Context, request provider.Request) provider.EventStream {
		requestMu.Lock()
		requests = append(requests, request)
		requestMu.Unlock()
		messages := request.Messages()
		if len(messages) != 0 && messages[len(messages)-1].Role() == llm.RoleToolResult {
			return loopTerminalEventStream(ctx, request, mustLoopTextMessage(t, request.Model(), "done", llm.FinishStop, 3))
		}
		return loopTerminalEventStream(ctx, request, mustLoopToolMessage(t, request.Model(), llm.FinishToolUse, call))
	})
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: runtimeProvider, Model: base,
		PrepareNextTurn: func(_ context.Context, input agent.AgentLoopTurnContext) (*agent.AgentLoopTurnUpdate, error) {
			first := input.Context.Messages[0].(agentmsg.LLM).Conversation().(llm.UserTextMessage).Content()[0].Text()
			model := modelA
			if first == "b" {
				model = modelB
			}
			thinking := provider.ThinkingHigh
			return &agent.AgentLoopTurnUpdate{Model: &model, ThinkingLevel: &thinking}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, prompt := range []string{"a", "a"} {
		if _, err := loop.Run(context.Background(), []agentmsg.Message{mustLoopUser(t, prompt, 1)}, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{tool}}); err != nil {
			t.Fatal(err)
		}
	}
	requestMu.Lock()
	firstFour := append([]provider.Request(nil), requests...)
	requests = nil
	requestMu.Unlock()
	if len(firstFour) != 4 || !firstFour[0].Model().Equal(base) || !firstFour[1].Model().Equal(modelA) || !firstFour[2].Model().Equal(base) || !firstFour[3].Model().Equal(modelA) {
		t.Fatalf("repeated models = %v", loopRequestModelIDs(firstFour))
	}

	var wait sync.WaitGroup
	errorsOut := make(chan error, 2)
	for _, prompt := range []string{"a", "b"} {
		prompt := prompt
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, runErr := loop.Run(context.Background(), []agentmsg.Message{mustLoopUser(t, prompt, 1)}, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{tool}})
			errorsOut <- runErr
		}()
	}
	wait.Wait()
	close(errorsOut)
	for runErr := range errorsOut {
		if runErr != nil {
			t.Fatal(runErr)
		}
	}
	requestMu.Lock()
	concurrentRequests := append([]provider.Request(nil), requests...)
	requestMu.Unlock()
	modelsByPrompt := map[string][]string{}
	for _, request := range concurrentRequests {
		prompt := request.Messages()[0].(llm.UserTextMessage).Content()[0].Text()
		modelsByPrompt[prompt] = append(modelsByPrompt[prompt], request.Model().ID())
	}
	assertLoopStrings(t, modelsByPrompt["a"], []string{"base", "model-a"})
	assertLoopStrings(t, modelsByPrompt["b"], []string{"base", "model-b"})
}

func TestAgentLoopRunAllowsEmptyPromptBatch(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{Provider: mustLoopProvider(t, mustLoopTextMessage(t, model, "ok", llm.FinishStop, 1)), Model: model})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop.Run(context.Background(), nil, agent.AgentLoopContext{})
	if err != nil {
		t.Fatal(err)
	}
	assertLoopStrings(t, loopRoles(result.Messages), []string{"assistant"})
}

func TestAgentLoopProcessesMessagesBeforeContextEventsAndResults(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	var startTexts, eventTexts []string
	textOf := func(message agentmsg.Message) string {
		standard, ok := message.(agentmsg.LLM)
		if !ok {
			if _, partial := message.(agentmsg.AssistantPartial); partial {
				return "assistant-partial"
			}
			return fmt.Sprintf("%T", message)
		}
		conversation := standard.Conversation()
		switch value := conversation.(type) {
		case llm.UserTextMessage:
			return value.Content()[0].Text()
		case llm.AssistantTextMessage:
			return value.Content()[0].Text()
		default:
			return fmt.Sprintf("%T", conversation)
		}
	}
	runtimeProvider := loopProviderFunc(func(ctx context.Context, request provider.Request) provider.EventStream {
		if got := request.Messages()[0].(llm.UserTextMessage).Content()[0].Text(); got != "processed user" {
			t.Fatalf("provider user = %q", got)
		}
		return loopTerminalEventStream(ctx, request, mustLoopTextMessage(t, model, "raw assistant", llm.FinishStop, 2))
	})
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: runtimeProvider,
		Model:    model,
		ProcessMessage: func(_ context.Context, message agentmsg.Message) (agentmsg.Message, error) {
			switch message.Role() {
			case agentmsg.RoleUser:
				return mustLoopUser(t, "processed user", 1), nil
			case agentmsg.RoleAssistant:
				wrapped, wrapErr := agentmsg.NewLLM(mustLoopTextMessage(t, model, "processed assistant", llm.FinishStop, 2))
				return wrapped, wrapErr
			default:
				return message, nil
			}
		},
		Emit: func(_ context.Context, event agent.AgentEvent) error {
			if messageStart, ok := event.(agent.MessageStartEvent); ok {
				startTexts = append(startTexts, textOf(messageStart.Message))
			}
			if messageEnd, ok := event.(agent.MessageEndEvent); ok {
				eventTexts = append(eventTexts, textOf(messageEnd.Message))
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop.Run(context.Background(), []agentmsg.Message{mustLoopUser(t, "raw user", 1)}, agent.AgentLoopContext{})
	if err != nil {
		t.Fatal(err)
	}
	assertLoopStrings(t, startTexts, []string{"raw user", "assistant-partial"})
	assertLoopStrings(t, eventTexts, []string{"processed user", "processed assistant"})
	if got := []string{textOf(result.Messages[0]), textOf(result.Messages[1])}; got[0] != "processed user" || got[1] != "processed assistant" {
		t.Fatalf("result messages = %v", got)
	}
}

func TestAgentLoopRejectsInvalidProcessedMessageAsRawLoopError(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: mustLoopProvider(t, mustLoopTextMessage(t, model, "assistant", llm.FinishStop, 2)),
		Model:    model,
		ProcessMessage: func(_ context.Context, message agentmsg.Message) (agentmsg.Message, error) {
			if message.Role() == agentmsg.RoleAssistant {
				return mustLoopUser(t, "wrong role", 2), nil
			}
			return message, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = loop.Run(context.Background(), []agentmsg.Message{mustLoopUser(t, "go", 1)}, agent.AgentLoopContext{})
	if !errors.Is(err, agent.ErrInvariant) {
		t.Fatalf("Run error = %v, want ErrInvariant", err)
	}
}

func TestAgentLoopToolPrepareValidateBeforeMutationAndRawEventIdentity(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	definition := mustLoopDefinition(t, "edit", `{"type":"object","properties":{"value":{"type":"number"}},"required":["value"],"additionalProperties":false}`)
	call := mustLoopCall(t, "call-1", "edit", `{"legacy":"42"}`)
	var beforeArgs, executeArgs, afterArgs any
	var eventArguments []string
	tool := &loopContractTool{
		definition: definition,
		prepare: func(arguments any) (any, error) {
			legacy := arguments.(map[string]any)["legacy"]
			return map[string]any{"value": legacy}, nil
		},
		execute: func(_ context.Context, _ string, arguments any, _ func(agent.ToolUpdate)) (agent.ToolOutput, error) {
			executeArgs = arguments
			return agent.ToolOutput{Text: "ok"}, nil
		},
	}
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: loopToolScenarioProvider(t, call), Model: model, ToolExecution: agent.ToolExecutionSequential,
		BeforeToolCall: func(_ context.Context, input agent.AgentLoopBeforeToolCallContext) (agent.AgentLoopBeforeToolCallResult, error) {
			beforeArgs = input.Arguments.(map[string]any)["value"]
			input.Arguments.(map[string]any)["value"] = "not revalidated"
			return agent.AgentLoopBeforeToolCallResult{}, nil
		},
		AfterToolCall: func(_ context.Context, input agent.AgentLoopAfterToolCallContext) (agent.AgentLoopAfterToolCallResult, error) {
			afterArgs = input.Arguments
			return agent.AgentLoopAfterToolCallResult{}, nil
		},
		Emit: func(_ context.Context, event agent.AgentEvent) error {
			switch value := event.(type) {
			case agent.ToolExecutionStartEvent:
				eventArguments = append(eventArguments, string(value.Arguments))
			case agent.ToolExecutionEndEvent:
				eventArguments = append(eventArguments, string(value.Arguments))
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Run(context.Background(), []agentmsg.Message{mustLoopUser(t, "go", 1)}, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{tool}}); err != nil {
		t.Fatal(err)
	}
	if beforeArgs != float64(42) {
		t.Fatalf("before args = %#v", beforeArgs)
	}
	if executeArgs.(map[string]any)["value"] != "not revalidated" || afterArgs.(map[string]any)["value"] != "not revalidated" {
		t.Fatalf("execute=%#v after=%#v", executeArgs, afterArgs)
	}
	assertLoopStrings(t, eventArguments, []string{`{"legacy":"42"}`, `{"legacy":"42"}`})
}

func TestAgentLoopBeforeReplacementIsAdoptedWithoutSecondSchemaValidation(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	definition := mustLoopDefinition(t, "edit", `{"type":"object","properties":{"value":{"type":"number"}},"required":["value"],"additionalProperties":false}`)
	call := mustLoopCall(t, "call-1", "edit", `{"value":1}`)
	var executed, observed any
	tool := &loopContractTool{
		definition: definition,
		execute: func(_ context.Context, _ string, arguments any, _ func(agent.ToolUpdate)) (agent.ToolOutput, error) {
			executed = arguments
			return agent.ToolOutput{Text: "ok"}, nil
		},
	}
	replacement := any(map[string]any{"value": "schema-invalid extension mutation"})
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: loopToolScenarioProvider(t, call), Model: model,
		BeforeToolCall: func(context.Context, agent.AgentLoopBeforeToolCallContext) (agent.AgentLoopBeforeToolCallResult, error) {
			return agent.AgentLoopBeforeToolCallResult{Arguments: &replacement}, nil
		},
		AfterToolCall: func(_ context.Context, input agent.AgentLoopAfterToolCallContext) (agent.AgentLoopAfterToolCallResult, error) {
			observed = input.Arguments
			return agent.AgentLoopAfterToolCallResult{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop.Run(context.Background(), nil, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	if tool.calls.Load() != 1 || result.ToolExecutions != 1 || executed.(map[string]any)["value"] != "schema-invalid extension mutation" || observed.(map[string]any)["value"] != "schema-invalid extension mutation" {
		t.Fatalf("replacement executed=%#v observed=%#v calls=%d", executed, observed, tool.calls.Load())
	}
}

func TestAgentLoopCompleteJSONSchemaValidationAndCoercion(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	schema := `{
		"type":"object",
		"allOf":[{"properties":{"name":{"pattern":"^[A-Z]+$"}}}],
		"properties":{
			"name":{"type":"string","maxLength":4},
			"timeout":{"type":"number","exclusiveMinimum":0},
			"edits":{"type":"array","minItems":1,"maxItems":2,"items":{"type":"integer"}},
			"tuple":{"type":"array","minItems":2,"maxItems":2,"items":[{"type":"integer"},{"type":"boolean"}]},
			"extras":{"type":"object","additionalProperties":{"type":"integer"}},
			"choice":{"oneOf":[{"type":"string","const":"fixed"},{"type":"integer","minimum":2}]},
			"union":{"anyOf":[{"type":"integer","minimum":3},{"type":"boolean"}]},
			"enabled":{"type":"boolean"},
			"tag":{"$ref":"#/$defs/tag"}
		},
		"required":["name","timeout","edits","tuple","extras","choice","union","enabled","tag"],
		"additionalProperties":false,
		"$defs":{"tag":{"type":"string","pattern":"^tag-[0-9]+$"}}
	}`
	definition := mustLoopDefinition(t, "complex", schema)
	raw := `{"name":"AB","timeout":"1.5","edits":["2"],"tuple":["3","true"],"extras":{"x":"4"},"choice":"fixed","union":"5","enabled":null,"tag":"tag-7"}`
	call := mustLoopCall(t, "call-1", "complex", raw)
	var before any
	tool := &loopContractTool{definition: definition}
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: loopToolScenarioProvider(t, call), Model: model,
		BeforeToolCall: func(_ context.Context, input agent.AgentLoopBeforeToolCallContext) (agent.AgentLoopBeforeToolCallResult, error) {
			before = input.Arguments
			return agent.AgentLoopBeforeToolCallResult{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Run(context.Background(), nil, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{tool}}); err != nil {
		t.Fatal(err)
	}
	arguments := before.(map[string]any)
	if arguments["timeout"] != 1.5 || arguments["union"] != float64(5) {
		t.Fatalf("scalar coercion = %#v", arguments)
	}
	if arguments["enabled"] != false {
		t.Fatalf("null boolean coercion = %#v", arguments["enabled"])
	}
	if tuple := arguments["tuple"].([]any); tuple[0] != float64(3) || tuple[1] != true {
		t.Fatalf("tuple coercion = %#v", tuple)
	}
	if arguments["extras"].(map[string]any)["x"] != float64(4) || arguments["edits"].([]any)[0] != float64(2) {
		t.Fatalf("nested coercion = %#v", arguments)
	}
}

func TestAgentLoopBeforeHookOnlySeesFullySchemaValidArguments(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	tests := []struct {
		name   string
		schema string
		raw    string
	}{
		{name: "pattern", schema: `{"type":"object","properties":{"value":{"type":"string","pattern":"^[A-Z]+$"}},"required":["value"]}`, raw: `{"value":"lower"}`},
		{name: "minItems", schema: `{"type":"object","properties":{"value":{"type":"array","minItems":1,"items":{"type":"string"}}},"required":["value"]}`, raw: `{"value":[]}`},
		{name: "exclusiveMinimum", schema: `{"type":"object","properties":{"value":{"type":"number","exclusiveMinimum":0}},"required":["value"]}`, raw: `{"value":0}`},
		{name: "const", schema: `{"type":"object","properties":{"value":{"const":"fixed"}},"required":["value"]}`, raw: `{"value":"other"}`},
		{name: "additionalProperties schema", schema: `{"type":"object","additionalProperties":{"type":"integer"}}`, raw: `{"value":"not-an-integer"}`},
		{name: "oneOf ambiguity", schema: `{"type":"object","properties":{"value":{"oneOf":[{"type":"number"},{"type":"integer"}]}},"required":["value"]}`, raw: `{"value":2}`},
		{name: "$ref", schema: `{"type":"object","properties":{"value":{"$ref":"#/$defs/value"}},"required":["value"],"$defs":{"value":{"type":"string","maxLength":2}}}`, raw: `{"value":"long"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			definition := mustLoopDefinition(t, "check", test.schema)
			call := mustLoopCall(t, "call-1", "check", test.raw)
			tool := &loopContractTool{definition: definition}
			var beforeCalls atomic.Uint32
			loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
				Provider: loopToolScenarioProvider(t, call), Model: model,
				BeforeToolCall: func(context.Context, agent.AgentLoopBeforeToolCallContext) (agent.AgentLoopBeforeToolCallResult, error) {
					beforeCalls.Add(1)
					return agent.AgentLoopBeforeToolCallResult{}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := loop.Run(context.Background(), nil, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{tool}})
			if err != nil {
				t.Fatal(err)
			}
			if beforeCalls.Load() != 0 || tool.calls.Load() != 0 {
				t.Fatalf("before=%d execute=%d", beforeCalls.Load(), tool.calls.Load())
			}
			if !result.Messages[1].(agentmsg.LLM).Conversation().(llm.ToolResultMessage).IsError() {
				t.Fatal("schema failure was not an immediate error result")
			}
		})
	}
}

func TestAgentLoopImmediateToolFailuresSkipExecuteAndAfter(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	baseDefinition := mustLoopDefinition(t, "echo", `{"type":"object","properties":{"items":{"type":"array"}},"required":["items"]}`)
	tests := []struct {
		name   string
		call   llm.ToolCallBlock
		tool   *loopContractTool
		before agent.AgentLoopBeforeToolCallHook
	}{
		{name: "not found", call: mustLoopCall(t, "call-1", "missing", `{}`)},
		{name: "prepare throws", call: mustLoopCall(t, "call-1", "echo", `{"items":[]}`), tool: &loopContractTool{definition: baseDefinition, prepare: func(any) (any, error) { return nil, errors.New("prepare failed") }}},
		{name: "prepare panics", call: mustLoopCall(t, "call-1", "echo", `{"items":[]}`), tool: &loopContractTool{definition: baseDefinition, prepare: func(any) (any, error) { panic("prepare failed") }}},
		{name: "validation fails", call: mustLoopCall(t, "call-1", "echo", `{"items":"bad"}`), tool: &loopContractTool{definition: baseDefinition}},
		{name: "before blocks", call: mustLoopCall(t, "call-1", "echo", `{"items":[]}`), tool: &loopContractTool{definition: baseDefinition}, before: func(context.Context, agent.AgentLoopBeforeToolCallContext) (agent.AgentLoopBeforeToolCallResult, error) {
			return agent.AgentLoopBeforeToolCallResult{Block: true, Reason: "blocked"}, nil
		}},
		{name: "before throws", call: mustLoopCall(t, "call-1", "echo", `{"items":[]}`), tool: &loopContractTool{definition: baseDefinition}, before: func(context.Context, agent.AgentLoopBeforeToolCallContext) (agent.AgentLoopBeforeToolCallResult, error) {
			return agent.AgentLoopBeforeToolCallResult{}, errors.New("before failed")
		}},
		{name: "before panics", call: mustLoopCall(t, "call-1", "echo", `{"items":[]}`), tool: &loopContractTool{definition: baseDefinition}, before: func(context.Context, agent.AgentLoopBeforeToolCallContext) (agent.AgentLoopBeforeToolCallResult, error) {
			panic("before failed")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var afterCalls atomic.Uint32
			loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
				Provider: loopToolScenarioProvider(t, test.call), Model: model,
				BeforeToolCall: test.before,
				AfterToolCall: func(context.Context, agent.AgentLoopAfterToolCallContext) (agent.AgentLoopAfterToolCallResult, error) {
					afterCalls.Add(1)
					return agent.AgentLoopAfterToolCallResult{}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			var tools []agent.AgentLoopTool
			if test.tool != nil {
				tools = []agent.AgentLoopTool{test.tool}
			}
			result, err := loop.Run(context.Background(), []agentmsg.Message{mustLoopUser(t, "go", 1)}, agent.AgentLoopContext{Tools: tools})
			if err != nil {
				t.Fatal(err)
			}
			if test.tool != nil && test.tool.calls.Load() != 0 {
				t.Fatalf("execute calls = %d", test.tool.calls.Load())
			}
			if afterCalls.Load() != 0 {
				t.Fatalf("after calls = %d", afterCalls.Load())
			}
			toolResult := result.Messages[2].(agentmsg.LLM).Conversation().(llm.ToolResultMessage)
			if !toolResult.IsError() {
				t.Fatal("immediate failure was not marked as error")
			}
			if string(toolResult.Details()) != `{}` {
				t.Fatalf("synthetic details = %s", toolResult.Details())
			}
		})
	}
}

func TestAgentLoopExecuteFailureStillRunsAfterAndAppliesFieldOverrides(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	definition := mustLoopDefinition(t, "echo", `{"type":"object"}`)
	call := mustLoopCall(t, "call-1", "echo", `{}`)
	replacementUsage, err := llm.NewUsage(llm.UsageSpec{Output: 2})
	if err != nil {
		t.Fatal(err)
	}
	tool := &loopContractTool{definition: definition, execute: func(context.Context, string, any, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		return agent.ToolOutput{Text: "discarded", Details: map[string]any{"old": true}, AddedToolNames: []string{"old"}, Terminate: true}, errors.New("execute failed")
	}}
	emptyContent := []llm.ToolResultContentBlock{}
	newDetails := any(map[string]any{"new": true})
	isError := false
	terminate := false
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: loopToolScenarioProvider(t, call), Model: model,
		AfterToolCall: func(_ context.Context, input agent.AgentLoopAfterToolCallContext) (agent.AgentLoopAfterToolCallResult, error) {
			if !input.IsError || input.Result.Text != "execute failed" || input.Result.Usage != nil || len(input.Result.AddedToolNames) != 0 {
				t.Fatalf("after input = %#v", input)
			}
			if details, ok := input.Result.Details.(map[string]any); !ok || len(details) != 0 {
				t.Fatalf("after synthetic details = %#v", input.Result.Details)
			}
			return agent.AgentLoopAfterToolCallResult{Content: &emptyContent, Details: &newDetails, IsError: &isError, Usage: &replacementUsage, Terminate: &terminate}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop.Run(context.Background(), []agentmsg.Message{mustLoopUser(t, "go", 1)}, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	message := result.Messages[2].(agentmsg.LLM).Conversation().(llm.ToolResultContentMessage)
	if message.IsError() || message.Content() == nil || len(message.Content()) != 0 || string(message.Details()) != `{"new":true}` {
		t.Fatalf("tool result = %#v details=%s", message, message.Details())
	}
	usage, ok := message.Usage()
	if !ok || usage.Output() != 2 || len(message.AddedToolNames()) != 0 {
		t.Fatalf("metadata usage=%#v names=%v", usage, message.AddedToolNames())
	}
}

func TestAgentLoopAfterHookNullAndOmittedFieldsPreserveExecutedMetadata(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	definition := mustLoopDefinition(t, "echo", `{"type":"object"}`)
	call := mustLoopCall(t, "call-1", "echo", `{}`)
	usage, err := llm.NewUsage(llm.UsageSpec{Input: 7})
	if err != nil {
		t.Fatal(err)
	}
	tool := &loopContractTool{definition: definition, execute: func(context.Context, string, any, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		return agent.ToolOutput{Text: "executed", Details: map[string]any{"old": true}, Usage: &usage, AddedToolNames: []string{"old"}}, nil
	}}
	var nullContent []llm.ToolResultContentBlock
	nullDetails := any(nil)
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: loopToolScenarioProvider(t, call), Model: model,
		AfterToolCall: func(context.Context, agent.AgentLoopAfterToolCallContext) (agent.AgentLoopAfterToolCallResult, error) {
			return agent.AgentLoopAfterToolCallResult{Content: &nullContent, Details: &nullDetails}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop.Run(context.Background(), nil, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	message := result.Messages[1].(agentmsg.LLM).Conversation().(llm.ToolResultMessage)
	gotUsage, ok := message.Usage()
	if !ok || gotUsage.Input() != 7 || message.Content()[0].Text() != "executed" || string(message.Details()) != `{"old":true}` || len(message.AddedToolNames()) != 1 || message.AddedToolNames()[0] != "old" {
		t.Fatalf("result details=%s usage=%#v names=%v", message.Details(), gotUsage, message.AddedToolNames())
	}
}

func TestAgentLoopAfterFailureReplacesExecutedResult(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	definition := mustLoopDefinition(t, "echo", `{"type":"object"}`)
	call := mustLoopCall(t, "call-1", "echo", `{}`)
	tool := &loopContractTool{definition: definition, execute: func(context.Context, string, any, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		return agent.ToolOutput{Text: "executed"}, nil
	}}
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: loopToolScenarioProvider(t, call), Model: model,
		AfterToolCall: func(context.Context, agent.AgentLoopAfterToolCallContext) (agent.AgentLoopAfterToolCallResult, error) {
			return agent.AgentLoopAfterToolCallResult{}, errors.New("after failed")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop.Run(context.Background(), []agentmsg.Message{mustLoopUser(t, "go", 1)}, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	message := result.Messages[2].(agentmsg.LLM).Conversation().(llm.ToolResultMessage)
	if !message.IsError() || message.Content()[0].Text() != "after failed" {
		t.Fatalf("message = %#v", message)
	}
	if string(message.Details()) != `{}` {
		t.Fatalf("details = %s", message.Details())
	}
}

func TestAgentLoopToolAndAfterPanicsBecomeErrorResults(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	definition := mustLoopDefinition(t, "echo", `{"type":"object"}`)
	call := mustLoopCall(t, "call-1", "echo", `{}`)
	tests := []struct {
		name  string
		tool  *loopContractTool
		after agent.AgentLoopAfterToolCallHook
	}{
		{name: "execute", tool: &loopContractTool{definition: definition, execute: func(context.Context, string, any, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
			panic("execute failed")
		}}},
		{name: "after", tool: &loopContractTool{definition: definition}, after: func(context.Context, agent.AgentLoopAfterToolCallContext) (agent.AgentLoopAfterToolCallResult, error) {
			panic("after failed")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{Provider: loopToolScenarioProvider(t, call), Model: model, AfterToolCall: test.after})
			if err != nil {
				t.Fatal(err)
			}
			result, err := loop.Run(context.Background(), nil, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{test.tool}})
			if err != nil {
				t.Fatal(err)
			}
			message := result.Messages[1].(agentmsg.LLM).Conversation().(llm.ToolResultMessage)
			if !message.IsError() {
				t.Fatalf("message = %#v", message)
			}
			if string(message.Details()) != `{}` {
				t.Fatalf("details = %s", message.Details())
			}
		})
	}
}

func TestAgentLoopParallelPreflightCompletionAndResultOrdering(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	slowDefinition := mustLoopDefinition(t, "slow", `{"type":"object"}`)
	fastDefinition := mustLoopDefinition(t, "fast", `{"type":"object"}`)
	calls := []llm.ToolCallBlock{
		mustLoopCall(t, "slow-call", "slow", `{}`),
		mustLoopCall(t, "missing-call", "missing", `{}`),
		mustLoopCall(t, "fast-call", "fast", `{}`),
	}
	releaseSlow := make(chan struct{})
	preflightDone := atomic.Bool{}
	var preflightOrder []string
	var preflightMu sync.Mutex
	newTool := func(definition provider.ToolDefinition, slow bool) *loopContractTool {
		return &loopContractTool{
			definition: definition,
			prepare: func(arguments any) (any, error) {
				preflightMu.Lock()
				preflightOrder = append(preflightOrder, definition.Name())
				if definition.Name() == "fast" {
					preflightDone.Store(true)
				}
				preflightMu.Unlock()
				return arguments, nil
			},
			execute: func(context.Context, string, any, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
				if !preflightDone.Load() {
					t.Error("execute started before source-order preflight completed")
				}
				if slow {
					<-releaseSlow
				}
				return agent.ToolOutput{Text: definition.Name()}, nil
			},
		}
	}
	slow := newTool(slowDefinition, true)
	fast := newTool(fastDefinition, false)
	var endOrder, resultOrder []string
	var hookContextRoles [][]string
	providerRuntime := loopProviderFunc(func(ctx context.Context, request provider.Request) provider.EventStream {
		messages := request.Messages()
		if len(messages) != 0 && messages[len(messages)-1].Role() == llm.RoleToolResult {
			return loopTerminalEventStream(ctx, request, mustLoopTextMessage(t, request.Model(), "done", llm.FinishStop, 3))
		}
		return loopTerminalEventStream(ctx, request, mustLoopToolMessage(t, request.Model(), llm.FinishToolUse, calls...))
	})
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: providerRuntime, Model: model, ToolExecution: agent.ToolExecutionParallel,
		BeforeToolCall: func(_ context.Context, input agent.AgentLoopBeforeToolCallContext) (agent.AgentLoopBeforeToolCallResult, error) {
			hookContextRoles = append(hookContextRoles, loopRoles(input.Context.Messages))
			return agent.AgentLoopBeforeToolCallResult{}, nil
		},
		Emit: func(_ context.Context, event agent.AgentEvent) error {
			switch value := event.(type) {
			case agent.ToolExecutionEndEvent:
				endOrder = append(endOrder, value.ToolCallID)
				if value.ToolCallID == "fast-call" {
					close(releaseSlow)
				}
			case agent.MessageEndEvent:
				if wrapped, ok := value.Message.(agentmsg.LLM); ok {
					switch message := wrapped.Conversation().(type) {
					case llm.ToolResultMessage:
						resultOrder = append(resultOrder, message.ToolCallID())
					case llm.ToolResultContentMessage:
						resultOrder = append(resultOrder, message.ToolCallID())
					}
				}
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Run(context.Background(), []agentmsg.Message{mustLoopUser(t, "go", 1)}, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{slow, fast}}); err != nil {
		t.Fatal(err)
	}
	assertLoopStrings(t, preflightOrder, []string{"slow", "fast"})
	assertLoopStrings(t, endOrder, []string{"missing-call", "fast-call", "slow-call"})
	assertLoopStrings(t, resultOrder, []string{"slow-call", "missing-call", "fast-call"})
	for _, roles := range hookContextRoles {
		assertLoopStrings(t, roles, []string{"user", "assistant"})
	}
}

func TestAgentLoopSequentialToolOverrideSerializesWholeBatch(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	sequentialDefinition := mustLoopDefinition(t, "sequential", `{"type":"object"}`)
	parallelDefinition := mustLoopDefinition(t, "parallel", `{"type":"object"}`)
	var active, maximum atomic.Int32
	makeTool := func(definition provider.ToolDefinition, mode agent.ToolExecutionMode) *loopContractTool {
		return &loopContractTool{definition: definition, mode: mode, execute: func(context.Context, string, any, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
			current := active.Add(1)
			for {
				seen := maximum.Load()
				if current <= seen || maximum.CompareAndSwap(seen, current) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			active.Add(-1)
			return agent.ToolOutput{Text: "ok"}, nil
		}}
	}
	sequential := makeTool(sequentialDefinition, agent.ToolExecutionSequential)
	parallel := makeTool(parallelDefinition, agent.ToolExecutionParallel)
	calls := []llm.ToolCallBlock{mustLoopCall(t, "one", "parallel", `{}`), mustLoopCall(t, "two", "sequential", `{}`)}
	providerRuntime := loopProviderFunc(func(ctx context.Context, request provider.Request) provider.EventStream {
		messages := request.Messages()
		if len(messages) != 0 && messages[len(messages)-1].Role() == llm.RoleToolResult {
			return loopTerminalEventStream(ctx, request, mustLoopTextMessage(t, request.Model(), "done", llm.FinishStop, 3))
		}
		return loopTerminalEventStream(ctx, request, mustLoopToolMessage(t, request.Model(), llm.FinishToolUse, calls...))
	})
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{Provider: providerRuntime, Model: model, ToolExecution: agent.ToolExecutionParallel})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Run(context.Background(), []agentmsg.Message{mustLoopUser(t, "go", 1)}, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{parallel, sequential}}); err != nil {
		t.Fatal(err)
	}
	if maximum.Load() != 1 {
		t.Fatalf("maximum concurrent executions = %d", maximum.Load())
	}
}

func TestAgentLoopTypedNilToolIsIgnoredByEffectiveExecutionMode(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	definition := mustLoopDefinition(t, "work", `{"type":"object"}`)
	tool := &loopContractTool{definition: definition}
	var typedNil *loopContractTool
	providerRuntime := loopProviderFunc(func(ctx context.Context, request provider.Request) provider.EventStream {
		definitions := request.Tools()
		if len(definitions) != 1 || definitions[0].Name() != "work" || !request.ParallelToolCalls() {
			t.Fatalf("request tools=%v parallel=%v", definitions, request.ParallelToolCalls())
		}
		return loopTerminalEventStream(ctx, request, mustLoopTextMessage(t, request.Model(), "done", llm.FinishStop, 2))
	})
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{Provider: providerRuntime, Model: model, ToolExecution: agent.ToolExecutionParallel})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Run(context.Background(), nil, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{typedNil, tool}}); err != nil {
		t.Fatal(err)
	}
}

func TestAgentLoopToolUpdatesSettleBeforeEndAndLateUpdatesAreIgnored(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	definition := mustLoopDefinition(t, "echo", `{"type":"object"}`)
	call := mustLoopCall(t, "call-1", "echo", `{}`)
	updateEntered := make(chan struct{})
	releaseUpdate := make(chan struct{})
	late := make(chan struct{})
	lateDone := make(chan struct{})
	tool := &loopContractTool{definition: definition, execute: func(_ context.Context, _ string, _ any, report func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		go report(agent.ToolUpdate{Text: "accepted"})
		<-updateEntered
		go func() {
			<-late
			report(agent.ToolUpdate{Text: "late"})
			close(lateDone)
		}()
		return agent.ToolOutput{Text: "done"}, nil
	}}
	var eventOrder []string
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: loopToolScenarioProvider(t, call), Model: model,
		Emit: func(_ context.Context, event agent.AgentEvent) error {
			switch event.(type) {
			case agent.ToolExecutionUpdateEvent:
				eventOrder = append(eventOrder, "update")
				close(updateEntered)
				<-releaseUpdate
			case agent.ToolExecutionEndEvent:
				eventOrder = append(eventOrder, "end")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		<-updateEntered
		close(releaseUpdate)
	}()
	if _, err := loop.Run(context.Background(), []agentmsg.Message{mustLoopUser(t, "go", 1)}, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{tool}}); err != nil {
		t.Fatal(err)
	}
	close(late)
	<-lateDone
	assertLoopStrings(t, eventOrder, []string{"update", "end"})
}

func TestAgentLoopSynchronousToolReportDoesNotWaitForBlockedSink(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	definition := mustLoopDefinition(t, "echo", `{"type":"object"}`)
	call := mustLoopCall(t, "call-1", "echo", `{}`)
	sinkEntered := make(chan struct{})
	releaseSink := make(chan struct{})
	reportReturned := make(chan struct{})
	releaseTool := make(chan struct{})
	tool := &loopContractTool{definition: definition, execute: func(_ context.Context, _ string, _ any, report func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		report(agent.ToolUpdate{Text: "update"})
		close(reportReturned)
		<-releaseTool
		return agent.ToolOutput{Text: "done"}, nil
	}}
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: loopToolScenarioProvider(t, call), Model: model,
		Emit: func(_ context.Context, event agent.AgentEvent) error {
			if _, ok := event.(agent.ToolExecutionUpdateEvent); ok {
				close(sinkEntered)
				<-releaseSink
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() {
		_, runErr := loop.Run(context.Background(), nil, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{tool}})
		runDone <- runErr
	}()
	<-sinkEntered
	select {
	case <-reportReturned:
	case <-time.After(time.Second):
		t.Fatal("synchronous report blocked on event sink")
	}
	close(releaseSink)
	close(releaseTool)
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
}

func TestAgentLoopInvocationDispatcherSerializesParallelToolUpdates(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	definition := mustLoopDefinition(t, "work", `{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`)
	calls := []llm.ToolCallBlock{
		mustLoopCall(t, "call-1", "work", `{"id":"a"}`),
		mustLoopCall(t, "call-2", "work", `{"id":"b"}`),
	}
	start := make(chan struct{})
	var ready atomic.Uint32
	tool := &loopContractTool{definition: definition, execute: func(_ context.Context, _ string, arguments any, report func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		id := arguments.(map[string]any)["id"].(string)
		if ready.Add(1) == 2 {
			close(start)
		}
		<-start
		for index := 1; index <= 3; index++ {
			report(agent.ToolUpdate{Text: fmt.Sprintf("%s-%d", id, index)})
		}
		return agent.ToolOutput{Text: id}, nil
	}}
	var active atomic.Int32
	var concurrent atomic.Bool
	var mu sync.Mutex
	updates := map[string][]string{}
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: loopProviderFunc(func(ctx context.Context, request provider.Request) provider.EventStream {
			if len(request.Messages()) > 0 && request.Messages()[len(request.Messages())-1].Role() == llm.RoleToolResult {
				return loopTerminalEventStream(ctx, request, mustLoopTextMessage(t, request.Model(), "done", llm.FinishStop, 4))
			}
			return loopTerminalEventStream(ctx, request, mustLoopToolMessage(t, request.Model(), llm.FinishToolUse, calls...))
		}), Model: model,
		Emit: func(_ context.Context, event agent.AgentEvent) error {
			update, ok := event.(agent.ToolExecutionUpdateEvent)
			if !ok {
				return nil
			}
			if active.Add(1) != 1 {
				concurrent.Store(true)
			}
			time.Sleep(time.Millisecond)
			mu.Lock()
			updates[update.ToolCallID] = append(updates[update.ToolCallID], update.PartialResult.Text)
			mu.Unlock()
			active.Add(-1)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Run(context.Background(), nil, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{tool}}); err != nil {
		t.Fatal(err)
	}
	if concurrent.Load() {
		t.Fatal("event sink was invoked concurrently within one invocation")
	}
	assertLoopStrings(t, updates["call-1"], []string{"a-1", "a-2", "a-3"})
	assertLoopStrings(t, updates["call-2"], []string{"b-1", "b-2", "b-3"})
}

func TestAgentLoopToolUpdateSinkErrorIsNotDropped(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	definition := mustLoopDefinition(t, "echo", `{"type":"object"}`)
	call := mustLoopCall(t, "call-1", "echo", `{}`)
	sinkErr := errors.New("update sink failed")
	toolSettled := atomic.Bool{}
	tool := &loopContractTool{definition: definition, execute: func(_ context.Context, _ string, _ any, report func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		report(agent.ToolUpdate{Text: "update"})
		toolSettled.Store(true)
		return agent.ToolOutput{Text: "done"}, nil
	}}
	var agentEnded, toolEnded bool
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: loopToolScenarioProvider(t, call), Model: model,
		Emit: func(_ context.Context, event agent.AgentEvent) error {
			switch event.(type) {
			case agent.ToolExecutionUpdateEvent:
				return sinkErr
			case agent.ToolExecutionEndEvent:
				toolEnded = true
			case agent.AgentEndEvent:
				agentEnded = true
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = loop.Run(context.Background(), []agentmsg.Message{mustLoopUser(t, "go", 1)}, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{tool}})
	if !errors.Is(err, sinkErr) || !toolSettled.Load() || toolEnded || agentEnded {
		t.Fatalf("err=%v settled=%v toolEnd=%v agentEnd=%v", err, toolSettled.Load(), toolEnded, agentEnded)
	}
}

func TestAgentLoopUpdateErrorStillSettlesLaterAcceptedUpdates(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	definition := mustLoopDefinition(t, "echo", `{"type":"object"}`)
	call := mustLoopCall(t, "call-1", "echo", `{}`)
	sinkErr := errors.New("first update failed")
	secondEntered := make(chan struct{})
	releaseSecond := make(chan struct{})
	tool := &loopContractTool{definition: definition, execute: func(_ context.Context, _ string, _ any, report func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		report(agent.ToolUpdate{Text: "first"})
		report(agent.ToolUpdate{Text: "second"})
		return agent.ToolOutput{Text: "done"}, nil
	}}
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: loopToolScenarioProvider(t, call), Model: model,
		Emit: func(_ context.Context, event agent.AgentEvent) error {
			update, ok := event.(agent.ToolExecutionUpdateEvent)
			if !ok {
				return nil
			}
			if update.PartialResult.Text == "first" {
				return sinkErr
			}
			close(secondEntered)
			<-releaseSecond
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() {
		_, runErr := loop.Run(context.Background(), nil, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{tool}})
		runDone <- runErr
	}()
	<-secondEntered
	select {
	case err := <-runDone:
		t.Fatalf("Run returned before second accepted update settled: %v", err)
	default:
	}
	close(releaseSecond)
	if err := <-runDone; !errors.Is(err, sinkErr) {
		t.Fatalf("Run error = %v", err)
	}
}

func TestAgentLoopParallelCallbackFailureWaitsForSiblingSettlement(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	definition := mustLoopDefinition(t, "work", `{"type":"object","properties":{"kind":{"type":"string"}},"required":["kind"]}`)
	first := mustLoopCall(t, "call-1", "work", `{"kind":"fail"}`)
	second := mustLoopCall(t, "call-2", "work", `{"kind":"wait"}`)
	release := make(chan struct{})
	siblingStarted := make(chan struct{})
	siblingSettled := make(chan struct{})
	tool := &loopContractTool{definition: definition, execute: func(_ context.Context, _ string, arguments any, update func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		if arguments.(map[string]any)["kind"] == "fail" {
			<-siblingStarted
			update(agent.ToolUpdate{Text: "break sink"})
			return agent.ToolOutput{Text: "failed"}, nil
		}
		close(siblingStarted)
		<-release
		close(siblingSettled)
		return agent.ToolOutput{Text: "settled"}, nil
	}}
	sinkErr := errors.New("update sink failed")
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: mustLoopProvider(t, mustLoopToolMessage(t, model, llm.FinishToolUse, first, second)), Model: model,
		Emit: func(_ context.Context, event agent.AgentEvent) error {
			if _, ok := event.(agent.ToolExecutionUpdateEvent); ok {
				return sinkErr
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, runErr := loop.Run(context.Background(), nil, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{tool}})
		result <- runErr
	}()
	select {
	case err := <-result:
		t.Fatalf("Run returned before sibling settled: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-result; !errors.Is(err, sinkErr) {
		t.Fatalf("Run error = %v", err)
	}
	select {
	case <-siblingSettled:
	default:
		t.Fatal("Run returned without sibling settlement")
	}
}

func TestAgentLoopExecutesStopToolCallsButNeverFailureToolBlocks(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	definition := mustLoopDefinition(t, "echo", `{"type":"object"}`)
	call := mustLoopCall(t, "call-1", "echo", `{}`)
	tool := &loopContractTool{definition: definition}

	stopProviderCalls := atomic.Uint32{}
	stopProvider := loopProviderFunc(func(ctx context.Context, request provider.Request) provider.EventStream {
		if stopProviderCalls.Add(1) == 1 {
			return loopTerminalEventStream(ctx, request, mustLoopToolMessage(t, request.Model(), llm.FinishStop, call))
		}
		return loopTerminalEventStream(ctx, request, mustLoopTextMessage(t, request.Model(), "done", llm.FinishStop, 3))
	})
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{Provider: stopProvider, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Run(context.Background(), []agentmsg.Message{mustLoopUser(t, "go", 1)}, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{tool}}); err != nil {
		t.Fatal(err)
	}
	if tool.calls.Load() != 1 {
		t.Fatalf("stop tool calls = %d", tool.calls.Load())
	}

	failure, err := llm.NewFailure("provider failed", nil)
	if err != nil {
		t.Fatal(err)
	}
	failureTerminal, err := llm.NewAssistantFailureMessageWithBlocksAndMetadata([]llm.AssistantBlock{call}, llm.FinishError, failure, llm.Usage{}, time.UnixMilli(2), loopProvenance(model), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	failureLoop, err := agent.NewAgentLoop(agent.AgentLoopConfig{Provider: mustLoopProvider(t, failureTerminal), Model: model})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failureLoop.Run(context.Background(), []agentmsg.Message{mustLoopUser(t, "go", 1)}, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{tool}}); err != nil {
		t.Fatal(err)
	}
	if tool.calls.Load() != 1 {
		t.Fatalf("failure tool block executed: calls=%d", tool.calls.Load())
	}
}

func TestAgentLoopPreservesProviderUsageAndCostExactly(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{Input: 999, Output: 999})
	wantCost := llm.Cost{Input: 1.25, Output: 2.5, CacheRead: 3.75, CacheWrite: 4.5, Total: 12}
	usage, err := llm.NewUsage(llm.UsageSpec{Input: 10, Output: 20, CacheRead: 30, CacheWrite: 40, Cost: &wantCost})
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := llm.NewAssistantTextMessage([]llm.TextBlock{mustLoopText(t, "done")}, llm.FinishStop, usage, time.UnixMilli(2), loopProvenance(model))
	if err != nil {
		t.Fatal(err)
	}
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{Provider: mustLoopProvider(t, terminal), Model: model})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop.Run(context.Background(), nil, agent.AgentLoopContext{})
	if err != nil {
		t.Fatal(err)
	}
	got := result.Terminal.Usage()
	if got.Input() != 10 || got.Output() != 20 || got.CacheRead() != 30 || got.CacheWrite() != 40 || got.Cost() != wantCost {
		t.Fatalf("usage was repriced: tokens=%d/%d/%d/%d cost=%#v", got.Input(), got.Output(), got.CacheRead(), got.CacheWrite(), got.Cost())
	}
}

func TestAgentLoopPartialTransportFailurePreservesCompletedContent(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	provenance := loopProvenance(model)
	start, err := llm.NewStartEvent(provenance, time.UnixMilli(1))
	if err != nil {
		t.Fatal(err)
	}
	textStart, err := llm.NewTextStartEvent(0)
	if err != nil {
		t.Fatal(err)
	}
	delta, err := llm.NewTextDeltaEvent(0, "partial")
	if err != nil {
		t.Fatal(err)
	}
	textEnd, err := llm.NewTextEndEvent(0, "partial")
	if err != nil {
		t.Fatal(err)
	}
	transportErr := errors.New("transport broke")
	runtimeProvider := loopProviderFunc(func(context.Context, provider.Request) provider.EventStream {
		return &loopSliceStream{events: []llm.StreamEvent{start, textStart, delta, textEnd}, err: transportErr}
	})
	var starts, ends int
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: runtimeProvider, Model: model,
		Emit: func(_ context.Context, event agent.AgentEvent) error {
			switch event.(type) {
			case agent.MessageStartEvent:
				starts++
			case agent.MessageEndEvent:
				ends++
			}
			return nil
		},
		Now: func() time.Time { return time.UnixMilli(2) },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop.Run(context.Background(), nil, agent.AgentLoopContext{})
	if err != nil {
		t.Fatal(err)
	}
	failure := result.Terminal.(llm.AssistantFailureMessage)
	if len(failure.Blocks()) != 1 || failure.Blocks()[0].(llm.TextBlock).Text() != "partial" {
		t.Fatalf("failure blocks = %#v", failure.Blocks())
	}
	if starts != 1 || ends != 1 {
		t.Fatalf("message lifecycle starts=%d ends=%d", starts, ends)
	}
}

func TestAgentLoopIgnoresContentEventsBeforeNoStartTerminal(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	delta, err := llm.NewTextDeltaEvent(0, "bad")
	if err != nil {
		t.Fatal(err)
	}
	done, err := llm.NewDoneEvent(llm.FinishStop, llm.Usage{}, time.UnixMilli(2), loopProvenance(model))
	if err != nil {
		t.Fatal(err)
	}
	runtimeProvider := loopProviderFunc(func(context.Context, provider.Request) provider.EventStream {
		return &loopSliceStream{events: []llm.StreamEvent{delta, done}}
	})
	var eventTypes []agent.AgentEventType
	var updates uint32
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: runtimeProvider, Model: model,
		Emit: func(_ context.Context, event agent.AgentEvent) error {
			eventTypes = append(eventTypes, event.Type())
			if _, ok := event.(agent.MessageUpdateEvent); ok {
				updates++
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop.Run(context.Background(), nil, agent.AgentLoopContext{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Terminal.FinishReason() != llm.FinishStop || updates != 0 {
		t.Fatalf("terminal = %#v updates=%d", result.Terminal, updates)
	}
	assertLoopEventTypes(t, eventTypes, []agent.AgentEventType{
		agent.AgentStartEventType, agent.TurnStartEventType,
		agent.MessageStartEventType, agent.MessageEndEventType,
		agent.TurnEndEventType, agent.AgentEndEventType,
	})
}

func TestAgentLoopCallbackAndEventSinkErrorsRemainRaw(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	callbackErr := errors.New("prepare failed")
	var ended bool
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: mustLoopProvider(t, mustLoopTextMessage(t, model, "done", llm.FinishStop, 2)), Model: model,
		PrepareNextTurn: func(context.Context, agent.AgentLoopTurnContext) (*agent.AgentLoopTurnUpdate, error) {
			return nil, callbackErr
		},
		Emit: func(_ context.Context, event agent.AgentEvent) error {
			if _, ok := event.(agent.AgentEndEvent); ok {
				ended = true
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Run(context.Background(), nil, agent.AgentLoopContext{}); !errors.Is(err, callbackErr) || ended {
		t.Fatalf("prepare err=%v agentEnded=%v", err, ended)
	}

	sinkErr := errors.New("turn end sink failed")
	loop, err = agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: mustLoopProvider(t, mustLoopTextMessage(t, model, "done", llm.FinishStop, 2)), Model: model,
		Emit: func(_ context.Context, event agent.AgentEvent) error {
			if _, ok := event.(agent.TurnEndEvent); ok {
				return sinkErr
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Run(context.Background(), nil, agent.AgentLoopContext{}); !errors.Is(err, sinkErr) {
		t.Fatalf("sink error = %v", err)
	}
}

func TestAgentLoopEventSinkPanicReturnsRunError(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	var ended bool
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: mustLoopProvider(t, mustLoopTextMessage(t, model, "unused", llm.FinishStop, 2)), Model: model,
		Emit: func(_ context.Context, event agent.AgentEvent) error {
			switch event.(type) {
			case agent.TurnStartEvent:
				panic("sink exploded")
			case agent.AgentEndEvent:
				ended = true
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = loop.Run(context.Background(), nil, agent.AgentLoopContext{})
	if err == nil || !strings.Contains(err.Error(), "agent loop event sink panicked: sink exploded") || ended {
		t.Fatalf("err=%v agentEnded=%v", err, ended)
	}
}

func TestAgentLoopEventSinkCanReenterSameLoop(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	runtimeProvider := loopProviderFunc(func(ctx context.Context, request provider.Request) provider.EventStream {
		return loopTerminalEventStream(ctx, request, mustLoopTextMessage(t, request.Model(), "done", llm.FinishStop, 2))
	})
	var loop *agent.AgentLoop
	reentered := false
	var nestedErr error
	var err error
	loop, err = agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: runtimeProvider, Model: model,
		Emit: func(ctx context.Context, event agent.AgentEvent) error {
			if _, ok := event.(agent.AgentStartEvent); ok && !reentered {
				reentered = true
				_, nestedErr = loop.Run(ctx, nil, agent.AgentLoopContext{})
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Run(context.Background(), nil, agent.AgentLoopContext{}); err != nil {
		t.Fatal(err)
	}
	if !reentered || nestedErr != nil {
		t.Fatalf("reentered=%v nestedErr=%v", reentered, nestedErr)
	}
}

func TestAgentLoopTerminateStillRunsTurnCallbacksAndQueuedMessages(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	definition := mustLoopDefinition(t, "finish", `{"type":"object"}`)
	call := mustLoopCall(t, "call-1", "finish", `{}`)
	tool := &loopContractTool{definition: definition, execute: func(context.Context, string, any, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		return agent.ToolOutput{Text: "finished", Terminate: true}, nil
	}}
	steering := mustLoopUser(t, "steering", 4)
	followUp := mustLoopUser(t, "follow-up", 6)
	var callbackOrder []string
	steeringPolls := 0
	followUpPolls := 0
	providerCalls := 0
	runtimeProvider := loopProviderFunc(func(ctx context.Context, request provider.Request) provider.EventStream {
		providerCalls++
		if providerCalls == 1 {
			return loopTerminalEventStream(ctx, request, mustLoopToolMessage(t, request.Model(), llm.FinishToolUse, call))
		}
		return loopTerminalEventStream(ctx, request, mustLoopTextMessage(t, request.Model(), "done", llm.FinishStop, int64(7+providerCalls)))
	})
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: runtimeProvider, Model: model,
		PrepareNextTurn: func(context.Context, agent.AgentLoopTurnContext) (*agent.AgentLoopTurnUpdate, error) {
			callbackOrder = append(callbackOrder, "prepare")
			return nil, nil
		},
		ShouldStopAfterTurn: func(context.Context, agent.AgentLoopTurnContext) (bool, error) {
			callbackOrder = append(callbackOrder, "stop")
			return false, nil
		},
		GetSteeringMessages: func(context.Context) ([]agentmsg.Message, error) {
			callbackOrder = append(callbackOrder, "steering")
			steeringPolls++
			if steeringPolls == 2 {
				return []agentmsg.Message{steering}, nil
			}
			return nil, nil
		},
		GetFollowUpMessages: func(context.Context) ([]agentmsg.Message, error) {
			callbackOrder = append(callbackOrder, "followup")
			followUpPolls++
			if followUpPolls == 1 {
				return []agentmsg.Message{followUp}, nil
			}
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop.Run(context.Background(), []agentmsg.Message{mustLoopUser(t, "start", 1)}, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	if providerCalls != 3 {
		t.Fatalf("provider calls = %d, want 3", providerCalls)
	}
	assertLoopStrings(t, loopRoles(result.Messages), []string{"user", "assistant", "toolResult", "user", "assistant", "user", "assistant"})
	assertLoopStrings(t, callbackOrder, []string{
		"steering",
		"prepare", "stop", "steering",
		"prepare", "stop", "steering", "followup",
		"prepare", "stop", "steering", "followup",
	})
}

func TestAgentLoopPrepareNextTurnUpdatesContextBeforeStopAndSteeringDrain(t *testing.T) {
	model := mustLoopModel(t, "prepare-stop", provider.CostRates{})
	queued := atomic.Bool{}
	providerCalls := atomic.Uint32{}
	runtimeProvider := loopProviderFunc(func(ctx context.Context, request provider.Request) provider.EventStream {
		providerCalls.Add(1)
		queued.Store(true)
		return loopTerminalEventStream(ctx, request, mustLoopTextMessage(t, request.Model(), "done", llm.FinishStop, 2))
	})
	var callbackOrder []string
	prepareCalls := 0
	steeringPolls := 0
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: runtimeProvider, Model: model,
		PrepareNextTurn: func(_ context.Context, input agent.AgentLoopTurnContext) (*agent.AgentLoopTurnUpdate, error) {
			callbackOrder = append(callbackOrder, "prepare")
			prepareCalls++
			if !queued.Load() {
				t.Error("PrepareNextTurn ran before the provider made steering available")
			}
			replacement := input.Context
			replacement.SystemPrompt = "prepared-marker"
			return &agent.AgentLoopTurnUpdate{Context: &replacement}, nil
		},
		ShouldStopAfterTurn: func(_ context.Context, input agent.AgentLoopTurnContext) (bool, error) {
			callbackOrder = append(callbackOrder, "stop")
			if input.Context.SystemPrompt != "prepared-marker" {
				t.Errorf("ShouldStopAfterTurn context = %q", input.Context.SystemPrompt)
				return false, nil
			}
			return true, nil
		},
		GetSteeringMessages: func(context.Context) ([]agentmsg.Message, error) {
			callbackOrder = append(callbackOrder, "steering")
			steeringPolls++
			if queued.Load() {
				return []agentmsg.Message{mustLoopUser(t, "must remain queued", 3)}, nil
			}
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop.Run(context.Background(), []agentmsg.Message{mustLoopUser(t, "start", 1)}, agent.AgentLoopContext{SystemPrompt: "initial"})
	if err != nil {
		t.Fatal(err)
	}
	if providerCalls.Load() != 1 || prepareCalls != 1 || steeringPolls != 1 {
		t.Fatalf("provider/prepare/steering = %d/%d/%d, want 1/1/1", providerCalls.Load(), prepareCalls, steeringPolls)
	}
	if result.Context.SystemPrompt != "prepared-marker" || len(result.Messages) != 2 {
		t.Fatalf("result context/messages = %q/%#v", result.Context.SystemPrompt, result.Messages)
	}
	assertLoopStrings(t, callbackOrder, []string{"steering", "prepare", "stop"})
}

func TestAgentLoopPrepareNextTurnRunsAfterFinalTurnWithoutContinuation(t *testing.T) {
	model := mustLoopModel(t, "prepare-final", provider.CostRates{})
	prepareCalls := 0
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: mustLoopProvider(t, mustLoopTextMessage(t, model, "done", llm.FinishStop, 2)), Model: model,
		PrepareNextTurn: func(_ context.Context, input agent.AgentLoopTurnContext) (*agent.AgentLoopTurnUpdate, error) {
			prepareCalls++
			replacement := input.Context
			replacement.SystemPrompt = "final-side-effect"
			return &agent.AgentLoopTurnUpdate{Context: &replacement}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop.Run(context.Background(), nil, agent.AgentLoopContext{SystemPrompt: "initial"})
	if err != nil {
		t.Fatal(err)
	}
	if prepareCalls != 1 || result.Context.SystemPrompt != "final-side-effect" {
		t.Fatalf("final PrepareNextTurn = calls %d context %q", prepareCalls, result.Context.SystemPrompt)
	}
}

func TestAgentLoopPrepareReplacesDynamicRequestStateWithoutPersistingTransform(t *testing.T) {
	firstModel := mustLoopModel(t, "first", provider.CostRates{})
	secondModel := mustLoopModel(t, "second", provider.CostRates{})
	firstDefinition := mustLoopDefinition(t, "first_tool", `{"type":"object"}`)
	secondDefinition := mustLoopDefinition(t, "second_tool", `{"type":"object"}`)
	firstTool := &loopContractTool{definition: firstDefinition}
	secondTool := &loopContractTool{definition: secondDefinition}
	call := mustLoopCall(t, "call-1", "first_tool", `{}`)
	transformed := mustLoopUser(t, "request-only", 9)

	var requests []provider.Request
	runtimeProvider := loopProviderFunc(func(ctx context.Context, request provider.Request) provider.EventStream {
		requests = append(requests, request)
		if len(requests) == 1 {
			return loopTerminalEventStream(ctx, request, mustLoopToolMessage(t, request.Model(), llm.FinishToolUse, call))
		}
		return loopTerminalEventStream(ctx, request, mustLoopTextMessage(t, request.Model(), "done", llm.FinishStop, 10))
	})
	prepareCalls := 0
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: runtimeProvider, Model: firstModel, ThinkingLevel: provider.ThinkingLow,
		TransformContext: func(_ context.Context, messages []agentmsg.Message) ([]agentmsg.Message, error) {
			return append(messages, transformed), nil
		},
		PrepareNextTurn: func(_ context.Context, input agent.AgentLoopTurnContext) (*agent.AgentLoopTurnUpdate, error) {
			prepareCalls++
			if prepareCalls != 1 {
				return nil, nil
			}
			replacement := input.Context
			replacement.SystemPrompt = "replacement"
			replacement.Tools = []agent.AgentLoopTool{secondTool}
			thinking := provider.ThinkingHigh
			return &agent.AgentLoopTurnUpdate{Context: &replacement, Model: &secondModel, ThinkingLevel: &thinking}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop.Run(context.Background(), []agentmsg.Message{mustLoopUser(t, "start", 1)}, agent.AgentLoopContext{SystemPrompt: "initial", Tools: []agent.AgentLoopTool{firstTool}})
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d", len(requests))
	}
	if !requests[0].Model().Equal(firstModel) || requests[0].ThinkingLevel() != provider.ThinkingLow || requests[0].SystemPrompt() != "initial" || requests[0].Tools()[0].Name() != "first_tool" {
		t.Fatalf("first request = %#v", requests[0])
	}
	if !requests[1].Model().Equal(secondModel) || requests[1].ThinkingLevel() != provider.ThinkingHigh || requests[1].SystemPrompt() != "replacement" || requests[1].Tools()[0].Name() != "second_tool" {
		t.Fatalf("second request = %#v", requests[1])
	}
	for _, request := range requests {
		messages := request.Messages()
		if messages[len(messages)-1].(llm.UserTextMessage).Content()[0].Text() != "request-only" {
			t.Fatalf("transform missing from request: %#v", messages)
		}
	}
	for _, message := range result.Context.Messages {
		if conversation, ok := message.(agentmsg.LLM); ok {
			if user, ok := conversation.Conversation().(llm.UserTextMessage); ok && user.Content()[0].Text() == "request-only" {
				t.Fatal("request-only transform leaked into loop context")
			}
		}
	}
}

func TestAgentLoopCancellationDuringPreflightSkipsExecute(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	definition := mustLoopDefinition(t, "work", `{"type":"object"}`)
	call := mustLoopCall(t, "call-1", "work", `{}`)
	tests := []struct {
		name      string
		configure func(context.CancelCauseFunc, *loopContractTool) agent.AgentLoopBeforeToolCallHook
	}{
		{name: "before", configure: func(cancel context.CancelCauseFunc, _ *loopContractTool) agent.AgentLoopBeforeToolCallHook {
			return func(context.Context, agent.AgentLoopBeforeToolCallContext) (agent.AgentLoopBeforeToolCallResult, error) {
				cancel(agent.ErrAgentAborted)
				return agent.AgentLoopBeforeToolCallResult{}, nil
			}
		}},
		{name: "preparer without before", configure: func(cancel context.CancelCauseFunc, tool *loopContractTool) agent.AgentLoopBeforeToolCallHook {
			tool.prepare = func(arguments any) (any, error) {
				cancel(agent.ErrAgentAborted)
				return arguments, nil
			}
			return nil
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			tool := &loopContractTool{definition: definition}
			before := test.configure(cancel, tool)
			loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{Provider: loopToolScenarioProvider(t, call), Model: model, BeforeToolCall: before})
			if err != nil {
				t.Fatal(err)
			}
			result, err := loop.Run(ctx, nil, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{tool}})
			if err != nil {
				t.Fatal(err)
			}
			if tool.calls.Load() != 0 || result.Messages[1].(agentmsg.LLM).Conversation().(llm.ToolResultMessage).IsError() == false {
				t.Fatalf("execute=%d messages=%v", tool.calls.Load(), loopRoles(result.Messages))
			}
			if details := result.Messages[1].(agentmsg.LLM).Conversation().(llm.ToolResultMessage).Details(); string(details) != `{}` {
				t.Fatalf("details = %s", details)
			}
		})
	}
}

func TestAgentLoopParallelPreflightStopsScanningAfterImmediateCancellation(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	definition := mustLoopDefinition(t, "work", `{"type":"object"}`)
	calls := []llm.ToolCallBlock{
		mustLoopCall(t, "call-1", "missing", `{}`),
		mustLoopCall(t, "call-2", "work", `{}`),
		mustLoopCall(t, "call-3", "work", `{}`),
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	tool := &loopContractTool{definition: definition}
	var started []string
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: mustLoopProvider(t, mustLoopToolMessage(t, model, llm.FinishToolUse, calls...)), Model: model,
		ToolExecution: agent.ToolExecutionParallel,
		Emit: func(_ context.Context, event agent.AgentEvent) error {
			switch value := event.(type) {
			case agent.ToolExecutionStartEvent:
				started = append(started, value.ToolCallID)
			case agent.ToolExecutionEndEvent:
				cancel(agent.ErrAgentAborted)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop.Run(ctx, nil, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	assertLoopStrings(t, started, []string{"call-1"})
	if tool.calls.Load() != 0 || result.ToolExecutions != 0 {
		t.Fatalf("execute=%d result=%#v", tool.calls.Load(), result)
	}
	toolResult := result.Messages[1].(agentmsg.LLM).Conversation().(llm.ToolResultMessage)
	if !toolResult.IsError() || string(toolResult.Details()) != `{}` {
		t.Fatalf("tool result=%#v details=%s", toolResult, toolResult.Details())
	}
}

func TestAgentLoopParallelExecutesEarlierPreparedCallWithCancelledContext(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	definition := mustLoopDefinition(t, "work", `{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`)
	calls := []llm.ToolCallBlock{
		mustLoopCall(t, "call-1", "work", `{"id":"first"}`),
		mustLoopCall(t, "call-2", "work", `{"id":"cancel"}`),
		mustLoopCall(t, "call-3", "work", `{"id":"unscanned"}`),
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	var beforeCalls atomic.Uint32
	var started []string
	var executedID string
	var sawCancelled atomic.Bool
	tool := &loopContractTool{definition: definition, execute: func(ctx context.Context, _ string, arguments any, _ func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		executedID = arguments.(map[string]any)["id"].(string)
		sawCancelled.Store(context.Cause(ctx) != nil)
		return agent.ToolOutput{Text: "executed"}, nil
	}}
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: mustLoopProvider(t, mustLoopToolMessage(t, model, llm.FinishToolUse, calls...)), Model: model,
		BeforeToolCall: func(_ context.Context, input agent.AgentLoopBeforeToolCallContext) (agent.AgentLoopBeforeToolCallResult, error) {
			if beforeCalls.Add(1) == 2 {
				if input.Arguments.(map[string]any)["id"] != "cancel" {
					t.Fatalf("second preflight arguments = %#v", input.Arguments)
				}
				cancel(agent.ErrAgentAborted)
			}
			return agent.AgentLoopBeforeToolCallResult{}, nil
		},
		Emit: func(_ context.Context, event agent.AgentEvent) error {
			if value, ok := event.(agent.ToolExecutionStartEvent); ok {
				started = append(started, value.ToolCallID)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop.Run(ctx, nil, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	assertLoopStrings(t, started, []string{"call-1", "call-2"})
	if beforeCalls.Load() != 2 || tool.calls.Load() != 1 || result.ToolExecutions != 1 || executedID != "first" || !sawCancelled.Load() {
		t.Fatalf("before=%d execute=%d executedID=%q cancelled=%v result=%#v", beforeCalls.Load(), tool.calls.Load(), executedID, sawCancelled.Load(), result)
	}
}

func TestAgentLoopPreCancelledMissingToolWinsBeforeScanStops(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	definition := mustLoopDefinition(t, "work", `{"type":"object"}`)
	calls := []llm.ToolCallBlock{
		mustLoopCall(t, "call-1", "missing", `{}`),
		mustLoopCall(t, "call-2", "work", `{}`),
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(agent.ErrAgentAborted)
	var started []string
	providerCalls := atomic.Uint32{}
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: loopProviderFunc(func(ctx context.Context, request provider.Request) provider.EventStream {
			call := providerCalls.Add(1)
			if call == 1 {
				return &loopSliceStream{events: loopToolUseEvents(t, model, calls...)}
			}
			if call != 2 || context.Cause(ctx) == nil {
				t.Fatalf("provider call %d context cause = %v", call, context.Cause(ctx))
			}
			event, eventErr := llm.NewErrorEvent(llm.FinishAborted, "cancelled", llm.Usage{}, time.UnixMilli(3), loopProvenance(request.Model()))
			if eventErr != nil {
				t.Fatal(eventErr)
			}
			return &loopSliceStream{events: []llm.StreamEvent{event}}
		}), Model: model,
		Emit: func(_ context.Context, event agent.AgentEvent) error {
			if value, ok := event.(agent.ToolExecutionStartEvent); ok {
				started = append(started, value.ToolCallID)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	tool := &loopContractTool{definition: definition}
	result, err := loop.Run(ctx, nil, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	assertLoopStrings(t, started, []string{"call-1"})
	toolResult := result.Messages[1].(agentmsg.LLM).Conversation().(llm.ToolResultMessage)
	if providerCalls.Load() != 2 || tool.calls.Load() != 0 || toolResult.Content()[0].Text() != "Tool missing not found" || result.Terminal.FinishReason() != llm.FinishAborted {
		t.Fatalf("provider=%d execute=%d result=%#v", providerCalls.Load(), tool.calls.Load(), toolResult)
	}
}

func TestAgentLoopAbortWinsBlockButNotBeforeHookError(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	definition := mustLoopDefinition(t, "work", `{"type":"object"}`)
	call := mustLoopCall(t, "call-1", "work", `{}`)
	hookErr := errors.New("before failed")
	tests := []struct {
		name     string
		hook     func(context.CancelCauseFunc) agent.AgentLoopBeforeToolCallHook
		wantText string
		wantErr  error
	}{
		{name: "abort wins block", hook: func(cancel context.CancelCauseFunc) agent.AgentLoopBeforeToolCallHook {
			return func(context.Context, agent.AgentLoopBeforeToolCallContext) (agent.AgentLoopBeforeToolCallResult, error) {
				cancel(agent.ErrAgentAborted)
				return agent.AgentLoopBeforeToolCallResult{Block: true, Reason: "blocked"}, nil
			}
		}, wantText: "Operation aborted", wantErr: agent.ErrAgentAborted},
		{name: "hook error wins abort", hook: func(cancel context.CancelCauseFunc) agent.AgentLoopBeforeToolCallHook {
			return func(context.Context, agent.AgentLoopBeforeToolCallContext) (agent.AgentLoopBeforeToolCallResult, error) {
				cancel(agent.ErrAgentAborted)
				return agent.AgentLoopBeforeToolCallResult{Block: true, Reason: "blocked"}, hookErr
			}
		}, wantText: hookErr.Error(), wantErr: hookErr},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			var endErr error
			loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
				Provider: loopToolScenarioProvider(t, call), Model: model, BeforeToolCall: test.hook(cancel),
				Emit: func(_ context.Context, event agent.AgentEvent) error {
					if value, ok := event.(agent.ToolExecutionEndEvent); ok {
						endErr = value.Err
					}
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			tool := &loopContractTool{definition: definition}
			result, err := loop.Run(ctx, nil, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{tool}})
			if err != nil {
				t.Fatal(err)
			}
			toolResult := result.Messages[1].(agentmsg.LLM).Conversation().(llm.ToolResultMessage)
			if tool.calls.Load() != 0 || toolResult.Content()[0].Text() != test.wantText || !errors.Is(endErr, test.wantErr) {
				t.Fatalf("execute=%d text=%q endErr=%v", tool.calls.Load(), toolResult.Content()[0].Text(), endErr)
			}
		})
	}
}

func TestAgentLoopCancellationAfterToolDoesNotOverwriteResult(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	definition := mustLoopDefinition(t, "work", `{"type":"object"}`)
	call := mustLoopCall(t, "call-1", "work", `{}`)
	ctx, cancel := context.WithCancelCause(context.Background())
	tool := &loopContractTool{definition: definition, execute: func(context.Context, string, any, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		return agent.ToolOutput{Text: "actual result"}, nil
	}}
	var afterCalls atomic.Uint32
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: loopToolScenarioProvider(t, call), Model: model,
		AfterToolCall: func(context.Context, agent.AgentLoopAfterToolCallContext) (agent.AgentLoopAfterToolCallResult, error) {
			afterCalls.Add(1)
			cancel(agent.ErrAgentAborted)
			return agent.AgentLoopAfterToolCallResult{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop.Run(ctx, nil, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	toolResult := result.Messages[1].(agentmsg.LLM).Conversation().(llm.ToolResultMessage)
	if afterCalls.Load() != 1 || toolResult.IsError() || toolResult.Content()[0].Text() != "actual result" {
		t.Fatalf("after=%d result=%#v", afterCalls.Load(), toolResult)
	}
}

func TestAgentLoopClonesConfiguredStreamOptionsAtConstruction(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	maxTokens := uint64(10)
	stream := provider.StreamOptions{Headers: map[string]string{"x-test": "original"}, MaxTokens: &maxTokens}
	providerRuntime := loopProviderFunc(func(ctx context.Context, request provider.Request) provider.EventStream {
		options := request.StreamOptions()
		if options.Headers["x-test"] != "original" || options.MaxTokens == nil || *options.MaxTokens != 10 {
			t.Fatalf("stream options = %#v", options)
		}
		return loopTerminalEventStream(ctx, request, mustLoopTextMessage(t, request.Model(), "done", llm.FinishStop, 2))
	})
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{Provider: providerRuntime, Model: model, Stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	stream.Headers["x-test"] = "mutated"
	maxTokens = 99
	if _, err := loop.Run(context.Background(), nil, agent.AgentLoopContext{}); err != nil {
		t.Fatal(err)
	}
}

func TestAgentLoopPreCancelledContextStillCallsProvider(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(agent.ErrAgentAborted)
	var calls atomic.Uint32
	runtimeProvider := loopProviderFunc(func(ctx context.Context, request provider.Request) provider.EventStream {
		calls.Add(1)
		if context.Cause(ctx) == nil {
			t.Fatal("provider did not receive cancelled context")
		}
		event, err := llm.NewErrorEvent(llm.FinishAborted, "cancelled", llm.Usage{}, time.UnixMilli(2), loopProvenance(request.Model()))
		if err != nil {
			t.Fatal(err)
		}
		return &loopSliceStream{events: []llm.StreamEvent{event}}
	})
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{Provider: runtimeProvider, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop.Run(ctx, nil, agent.AgentLoopContext{})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || result.ProviderTurns != 1 || result.Terminal.FinishReason() != llm.FinishAborted {
		t.Fatalf("calls=%d result=%#v", calls.Load(), result)
	}
}

func TestAgentLoopNoStartTerminalLifecycle(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	tests := []struct {
		name   string
		event  func(t *testing.T) llm.StreamEvent
		finish llm.FinishReason
	}{
		{name: "done", finish: llm.FinishStop, event: func(t *testing.T) llm.StreamEvent {
			event, err := llm.NewDoneEvent(llm.FinishStop, llm.Usage{}, time.UnixMilli(2), loopProvenance(model))
			if err != nil {
				t.Fatal(err)
			}
			return event
		}},
		{name: "error", finish: llm.FinishError, event: func(t *testing.T) llm.StreamEvent {
			event, err := llm.NewErrorEvent(llm.FinishError, "failed", llm.Usage{}, time.UnixMilli(2), loopProvenance(model))
			if err != nil {
				t.Fatal(err)
			}
			return event
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var events []agent.AgentEventType
			loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
				Provider: loopProviderFunc(func(context.Context, provider.Request) provider.EventStream {
					return &loopSliceStream{events: []llm.StreamEvent{test.event(t)}}
				}), Model: model,
				Emit: func(_ context.Context, event agent.AgentEvent) error {
					events = append(events, event.Type())
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := loop.Run(context.Background(), nil, agent.AgentLoopContext{})
			if err != nil {
				t.Fatal(err)
			}
			if result.Terminal.FinishReason() != test.finish {
				t.Fatalf("finish = %s", result.Terminal.FinishReason())
			}
			assertLoopEventTypes(t, events, []agent.AgentEventType{
				agent.AgentStartEventType, agent.TurnStartEventType,
				agent.MessageStartEventType, agent.MessageEndEventType,
				agent.TurnEndEventType, agent.AgentEndEventType,
			})
		})
	}
}

func TestAgentLoopTransportFailureUsesCanonicalActiveBlocks(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	tests := []struct {
		name       string
		events     func(t *testing.T) []llm.StreamEvent
		wantBlocks int
		assert     func(t *testing.T, block llm.AssistantBlock)
	}{
		{name: "thinking", wantBlocks: 1, events: func(t *testing.T) []llm.StreamEvent {
			start := newLoopStartEvent(t, model)
			thinkingStart, _ := llm.NewThinkingStartEvent(0)
			thinkingDelta, _ := llm.NewThinkingDeltaEvent(0, "active plan")
			return []llm.StreamEvent{start, thinkingStart, thinkingDelta}
		}, assert: func(t *testing.T, block llm.AssistantBlock) {
			if thinking, ok := block.(llm.ThinkingBlock); !ok || thinking.Thinking() != "active plan" {
				t.Fatalf("block = %#v", block)
			}
		}},
		{name: "complete active tool", wantBlocks: 1, events: func(t *testing.T) []llm.StreamEvent {
			start := newLoopStartEvent(t, model)
			toolStart, _ := llm.NewToolCallStartEvent(0, "call-1", "inspect")
			toolDelta, _ := llm.NewToolCallDeltaEvent(0, []byte(`{"path":"README.md"}`))
			return []llm.StreamEvent{start, toolStart, toolDelta}
		}, assert: func(t *testing.T, block llm.AssistantBlock) {
			if call, ok := block.(llm.ToolCallBlock); !ok || call.Name() != "inspect" {
				t.Fatalf("block = %#v", block)
			}
		}},
		{name: "incomplete active tool", wantBlocks: 0, events: func(t *testing.T) []llm.StreamEvent {
			start := newLoopStartEvent(t, model)
			toolStart, _ := llm.NewToolCallStartEvent(0, "call-1", "inspect")
			toolDelta, _ := llm.NewToolCallDeltaEvent(0, []byte(`{"path":`))
			return []llm.StreamEvent{start, toolStart, toolDelta}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transportErr := errors.New("transport failed")
			loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
				Provider: loopProviderFunc(func(context.Context, provider.Request) provider.EventStream {
					return &loopSliceStream{events: test.events(t), err: transportErr}
				}), Model: model,
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := loop.Run(context.Background(), nil, agent.AgentLoopContext{})
			if err != nil {
				t.Fatal(err)
			}
			failure := result.Terminal.(llm.AssistantFailureMessage)
			if len(failure.Blocks()) != test.wantBlocks {
				t.Fatalf("blocks = %#v", failure.Blocks())
			}
			if test.assert != nil {
				test.assert(t, failure.Blocks()[0])
			}
		})
	}
}

func TestAgentLoopTransformConvertAndDynamicAPIKeyOrderEachTurn(t *testing.T) {
	firstModel, err := provider.NewModel(provider.ModelSpec{Provider: "provider-a", API: "scripted", ID: "first", Name: "first", Input: []provider.InputKind{provider.InputText}, ContextWindow: 8192, MaxTokens: 2048})
	if err != nil {
		t.Fatal(err)
	}
	secondModel, err := provider.NewModel(provider.ModelSpec{Provider: "provider-b", API: "scripted", ID: "second", Name: "second", Input: []provider.InputKind{provider.InputText}, ContextWindow: 8192, MaxTokens: 2048})
	if err != nil {
		t.Fatal(err)
	}
	definition := mustLoopDefinition(t, "echo", `{"type":"object"}`)
	call := mustLoopCall(t, "call-1", "echo", `{}`)
	tool := &loopContractTool{definition: definition}
	var order []string
	var requests []provider.Request
	runtimeProvider := loopProviderFunc(func(ctx context.Context, request provider.Request) provider.EventStream {
		order = append(order, "provider:"+request.Model().Provider())
		requests = append(requests, request)
		if len(requests) == 1 {
			return loopTerminalEventStream(ctx, request, mustLoopToolMessage(t, request.Model(), llm.FinishToolUse, call))
		}
		return loopTerminalEventStream(ctx, request, mustLoopTextMessage(t, request.Model(), "done", llm.FinishStop, 4))
	})
	prepareCalls := 0
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: runtimeProvider, Model: firstModel, Stream: provider.StreamOptions{APIKey: "static-key"},
		TransformContext: func(_ context.Context, messages []agentmsg.Message) ([]agentmsg.Message, error) {
			order = append(order, "transform")
			return messages, nil
		},
		ConvertToLLM: func(_ context.Context, messages []agentmsg.Message) ([]llm.ConversationMessage, error) {
			order = append(order, "convert")
			return agentmsg.ConvertToLLM(messages)
		},
		GetAPIKey: func(_ context.Context, providerName string) (string, error) {
			order = append(order, "key:"+providerName)
			if providerName == "provider-a" {
				return "dynamic-key", nil
			}
			return "", nil
		},
		PrepareNextTurn: func(context.Context, agent.AgentLoopTurnContext) (*agent.AgentLoopTurnUpdate, error) {
			prepareCalls++
			if prepareCalls == 1 {
				return &agent.AgentLoopTurnUpdate{Model: &secondModel}, nil
			}
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Run(context.Background(), nil, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{tool}}); err != nil {
		t.Fatal(err)
	}
	assertLoopStrings(t, order, []string{
		"transform", "convert", "key:provider-a", "provider:provider-a",
		"transform", "convert", "key:provider-b", "provider:provider-b",
	})
	if requests[0].StreamOptions().APIKey != "dynamic-key" || requests[1].StreamOptions().APIKey != "static-key" {
		t.Fatalf("keys = %q, %q", requests[0].StreamOptions().APIKey, requests[1].StreamOptions().APIKey)
	}
}

func TestAgentLoopPreservesRichImageToolResult(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	definition := mustLoopDefinition(t, "image", `{"type":"object"}`)
	call := mustLoopCall(t, "call-1", "image", `{}`)
	image, err := llm.NewImageDataBlock("image/png", []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	tool := &loopContractTool{definition: definition, execute: func(context.Context, string, any, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		return agent.ToolOutput{Content: []llm.ToolResultContentBlock{image}}, nil
	}}
	var sawImage bool
	runtimeProvider := loopProviderFunc(func(ctx context.Context, request provider.Request) provider.EventStream {
		messages := request.Messages()
		if len(messages) > 0 {
			if result, ok := messages[len(messages)-1].(llm.ToolResultContentMessage); ok {
				content := result.Content()
				_, sawImage = content[0].(llm.ImageBlock)
				return loopTerminalEventStream(ctx, request, mustLoopTextMessage(t, request.Model(), "done", llm.FinishStop, 4))
			}
		}
		return loopTerminalEventStream(ctx, request, mustLoopToolMessage(t, request.Model(), llm.FinishToolUse, call))
	})
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{Provider: runtimeProvider, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop.Run(context.Background(), nil, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	if !sawImage {
		t.Fatal("provider did not receive image tool result")
	}
	toolResult := result.Messages[1].(agentmsg.LLM).Conversation().(llm.ToolResultContentMessage)
	if len(toolResult.Content()) != 1 {
		t.Fatalf("content = %#v", toolResult.Content())
	}
}

func TestAgentLoopTextOnlyLengthDoesNotAutoContinue(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	var calls atomic.Uint32
	runtimeProvider := loopProviderFunc(func(ctx context.Context, request provider.Request) provider.EventStream {
		calls.Add(1)
		return loopTerminalEventStream(ctx, request, mustLoopTextMessage(t, request.Model(), "truncated", llm.FinishLength, 2))
	})
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{Provider: runtimeProvider, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop.Run(context.Background(), nil, agent.AgentLoopContext{})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || result.Terminal.FinishReason() != llm.FinishLength || result.ProviderTurns != 1 {
		t.Fatalf("calls=%d result=%#v", calls.Load(), result)
	}
}

func TestAgentLoopMixedTerminateAndValidationFailureContinues(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	definition := mustLoopDefinition(t, "echo", `{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}`)
	calls := []llm.ToolCallBlock{
		mustLoopCall(t, "call-1", "echo", `{"value":"ok"}`),
		mustLoopCall(t, "call-2", "echo", `{"value":[]}`),
	}
	tool := &loopContractTool{definition: definition, execute: func(context.Context, string, any, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		return agent.ToolOutput{Text: "done", Terminate: true}, nil
	}}
	var providerCalls atomic.Uint32
	runtimeProvider := loopProviderFunc(func(ctx context.Context, request provider.Request) provider.EventStream {
		if providerCalls.Add(1) == 1 {
			return loopTerminalEventStream(ctx, request, mustLoopToolMessage(t, request.Model(), llm.FinishToolUse, calls...))
		}
		return loopTerminalEventStream(ctx, request, mustLoopTextMessage(t, request.Model(), "continued", llm.FinishStop, 4))
	})
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{Provider: runtimeProvider, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop.Run(context.Background(), nil, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	if providerCalls.Load() != 2 || tool.calls.Load() != 1 || len(result.Messages) != 4 {
		t.Fatalf("provider=%d execute=%d roles=%v", providerCalls.Load(), tool.calls.Load(), loopRoles(result.Messages))
	}
}

func TestAgentLoopContinueTailContract(t *testing.T) {
	model := mustLoopModel(t, "loop", provider.CostRates{})
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{Provider: mustLoopProvider(t), Model: model})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Continue(context.Background(), agent.AgentLoopContext{}); !errors.Is(err, agent.ErrCannotContinue) {
		t.Fatalf("empty error = %v", err)
	}
	assistant := mustLoopTextMessage(t, model, "tail", llm.FinishStop, 1)
	wrappedAssistant, _ := agentmsg.NewLLM(assistant)
	if _, err := loop.Continue(context.Background(), agent.AgentLoopContext{Messages: []agentmsg.Message{wrappedAssistant}}); !errors.Is(err, agent.ErrCannotContinue) {
		t.Fatalf("assistant error = %v", err)
	}

	custom, err := agentmsg.NewCustomText("notice", "custom tail", true, nil, time.UnixMilli(1))
	if err != nil {
		t.Fatal(err)
	}
	var sawCustom bool
	customLoop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		Provider: loopProviderFunc(func(ctx context.Context, request provider.Request) provider.EventStream {
			messages := request.Messages()
			sawCustom = len(messages) == 1 && messages[0].Role() == llm.RoleUser
			return loopTerminalEventStream(ctx, request, mustLoopTextMessage(t, request.Model(), "done", llm.FinishStop, 2))
		}), Model: model,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := customLoop.Continue(context.Background(), agent.AgentLoopContext{Messages: []agentmsg.Message{custom}})
	if err != nil {
		t.Fatal(err)
	}
	if !sawCustom || len(result.Messages) != 1 || len(result.Context.Messages) != 2 {
		t.Fatalf("sawCustom=%v result=%#v", sawCustom, result)
	}
}

type loopContractTool struct {
	definition provider.ToolDefinition
	mode       agent.ToolExecutionMode
	prepare    func(any) (any, error)
	execute    func(context.Context, string, any, func(agent.ToolUpdate)) (agent.ToolOutput, error)
	calls      atomic.Uint32
}

func (t *loopContractTool) Definition() provider.ToolDefinition { return t.definition }
func (t *loopContractTool) ExecutionMode() agent.ToolExecutionMode {
	if t.mode == 0 {
		return agent.ToolExecutionParallel
	}
	return t.mode
}
func (t *loopContractTool) PrepareArguments(arguments any) (any, error) {
	if t.prepare == nil {
		return arguments, nil
	}
	return t.prepare(arguments)
}
func (t *loopContractTool) Execute(ctx context.Context, id string, arguments any, update func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	t.calls.Add(1)
	if t.execute == nil {
		return agent.ToolOutput{Text: "ok"}, nil
	}
	return t.execute(ctx, id, arguments, update)
}

type loopProviderFunc func(context.Context, provider.Request) provider.EventStream

func (f loopProviderFunc) Stream(ctx context.Context, request provider.Request) provider.EventStream {
	return f(ctx, request)
}

type loopSliceStream struct {
	events []llm.StreamEvent
	next   int
	err    error
	closed bool
}

func (s *loopSliceStream) Next() (llm.StreamEvent, error) {
	if s.next < len(s.events) {
		event := s.events[s.next]
		s.next++
		return event, nil
	}
	if s.err != nil {
		err := s.err
		s.err = nil
		return nil, err
	}
	return nil, io.EOF
}
func (s *loopSliceStream) Close() error { s.closed = true; return nil }

func loopToolUseEvents(t *testing.T, model provider.Model, calls ...llm.ToolCallBlock) []llm.StreamEvent {
	t.Helper()
	start, err := llm.NewStartEvent(loopProvenance(model), time.UnixMilli(1))
	if err != nil {
		t.Fatal(err)
	}
	events := []llm.StreamEvent{start}
	for index, call := range calls {
		blockStart, err := llm.NewToolCallStartEvent(index, call.ID(), call.Name())
		if err != nil {
			t.Fatal(err)
		}
		delta, err := llm.NewToolCallDeltaEvent(index, call.ArgumentsJSON())
		if err != nil {
			t.Fatal(err)
		}
		blockEnd, err := llm.NewToolCallEndEvent(index, call)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, blockStart, delta, blockEnd)
	}
	done, err := llm.NewDoneEvent(llm.FinishToolUse, llm.Usage{}, time.UnixMilli(2), loopProvenance(model))
	if err != nil {
		t.Fatal(err)
	}
	return append(events, done)
}

func loopTerminalEventStream(ctx context.Context, request provider.Request, terminal llm.AssistantTerminal) provider.EventStream {
	rebound, err := llm.WithAssistantProvenance(terminal, loopProvenance(request.Model()))
	if err != nil {
		panic(err)
	}
	scripted, err := provider.NewScriptedProvider(provider.ScriptedConfig{ChunkRunes: 2, Clock: func() time.Time { return time.UnixMilli(10) }})
	if err != nil {
		panic(err)
	}
	step, err := provider.FixedResponseStep(rebound)
	if err != nil {
		panic(err)
	}
	if err := scripted.SetResponses([]provider.ScriptStep{step}); err != nil {
		panic(err)
	}
	return scripted.Stream(ctx, request)
}

func loopToolScenarioProvider(t *testing.T, call llm.ToolCallBlock) provider.Provider {
	t.Helper()
	return loopProviderFunc(func(ctx context.Context, request provider.Request) provider.EventStream {
		messages := request.Messages()
		if len(messages) != 0 && messages[len(messages)-1].Role() == llm.RoleToolResult {
			return loopTerminalEventStream(ctx, request, mustLoopTextMessage(t, request.Model(), "done", llm.FinishStop, 3))
		}
		return loopTerminalEventStream(ctx, request, mustLoopToolMessage(t, request.Model(), llm.FinishToolUse, call))
	})
}

func mustLoopDefinition(t *testing.T, name, schema string) provider.ToolDefinition {
	t.Helper()
	definition, err := provider.NewToolDefinition(name, name, false, []byte(schema))
	if err != nil {
		t.Fatal(err)
	}
	return definition
}

func newLoopStartEvent(t *testing.T, model provider.Model) llm.StartEvent {
	t.Helper()
	event, err := llm.NewStartEvent(loopProvenance(model), time.UnixMilli(1))
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func loopRequestModelIDs(requests []provider.Request) []string {
	result := make([]string, len(requests))
	for index, request := range requests {
		result[index] = request.Model().ID()
	}
	return result
}
