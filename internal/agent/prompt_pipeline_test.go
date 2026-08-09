package agent_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

type promptPipelineResources struct {
	mu         sync.Mutex
	expansions map[string]string
	inputs     []string
}

func (r *promptPipelineResources) BuildSystemPrompt(names []string) (string, agent.BuildSystemPromptOptions, error) {
	return "system", agent.BuildSystemPromptOptions{SelectedTools: append([]string(nil), names...)}, nil
}

func (r *promptPipelineResources) ExpandPromptInput(text string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inputs = append(r.inputs, text)
	if expanded, ok := r.expansions[text]; ok {
		return expanded, nil
	}
	return text, nil
}

func (*promptPipelineResources) Reload(context.Context) error { return nil }

func (r *promptPipelineResources) Inputs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.inputs...)
}

func promptMessageTexts(messages []llm.ConversationMessage) []string {
	result := make([]string, 0, len(messages))
	for _, message := range messages {
		var text string
		switch value := message.(type) {
		case llm.UserTextMessage:
			for _, block := range value.Content() {
				text += block.Text()
			}
		case llm.UserContentMessage:
			for _, block := range value.Content() {
				if block, ok := block.(llm.TextBlock); ok {
					text += block.Text()
				}
			}
		}
		if text != "" {
			result = append(result, text)
		}
	}
	return result
}

func customTextInput(customType, text string, display bool) agent.CustomMessageInput {
	return agent.CustomMessageInput{CustomType: customType, StringContent: &text, Display: display}
}

func TestAgentSessionPromptPreflightMatchesCommandInputAndExpansionOrder(t *testing.T) {
	resources := &promptPipelineResources{expansions: map[string]string{"stage1:stage3": "expanded prompt"}}
	implementation := newScriptedProvider(t, mustTextTerminal(t, "done"))
	commandFailure := errors.New("command failed")
	inputFailure := errors.New("input failed")
	var commandArgs []string
	var inputOrder []string
	var extensionErrors []agent.ExtensionErrorEvent
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: newSessionManager(t), Model: sessionTestModel(t),
		Resources: resources,
		Hooks: agent.Hooks{
			Commands: []agent.ExtensionCommand{
				{Name: "run", Handler: func(_ context.Context, args string, _ *agent.AgentSession) error {
					commandArgs = append(commandArgs, args)
					return nil
				}},
				{Name: "fail", Handler: func(context.Context, string, *agent.AgentSession) error { return commandFailure }},
			},
			InputHandlers: []agent.InputHook{
				func(_ context.Context, event agent.InputEvent) (agent.InputResult, error) {
					inputOrder = append(inputOrder, "first:"+event.Text)
					if event.Text == "consume" {
						return agent.InputResult{Action: agent.InputHandled}, nil
					}
					return agent.InputResult{Action: agent.InputTransform, Text: "stage1"}, nil
				},
				func(_ context.Context, event agent.InputEvent) (agent.InputResult, error) {
					inputOrder = append(inputOrder, "error:"+event.Text)
					return agent.InputResult{}, inputFailure
				},
				func(_ context.Context, event agent.InputEvent) (agent.InputResult, error) {
					inputOrder = append(inputOrder, "last:"+event.Text)
					return agent.InputResult{Action: agent.InputTransform, Text: event.Text + ":stage3"}, nil
				},
			},
			ExtensionError: func(_ context.Context, event agent.ExtensionErrorEvent) {
				extensionErrors = append(extensionErrors, event)
			},
		},
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := runtime.Run(context.Background(), "/run hello world")
	if err != nil || !result.Handled() || !result.Succeeded() {
		t.Fatalf("command result = (%#v, %v)", result, err)
	}
	if terminal, ok := result.Terminal(); ok || terminal != nil {
		t.Fatalf("handled command manufactured terminal %#v", terminal)
	}
	if !reflect.DeepEqual(commandArgs, []string{"hello world"}) || len(inputOrder) != 0 || len(resources.Inputs()) != 0 || implementation.CallCount() != 0 {
		t.Fatalf("command preflight = args %v input %v resources %v calls %d", commandArgs, inputOrder, resources.Inputs(), implementation.CallCount())
	}

	result, err = runtime.Run(context.Background(), "/fail")
	if err != nil || !result.Handled() || implementation.CallCount() != 0 {
		t.Fatalf("failing command result = (%#v, %v), calls=%d", result, err, implementation.CallCount())
	}
	if len(extensionErrors) != 1 || extensionErrors[0].Event != "command" || !errors.Is(extensionErrors[0].Cause, commandFailure) {
		t.Fatalf("command extension errors = %#v", extensionErrors)
	}

	result, err = runtime.Run(context.Background(), "plain")
	if err != nil || !result.Succeeded() {
		t.Fatalf("transformed prompt result = (%#v, %v)", result, err)
	}
	if !reflect.DeepEqual(inputOrder, []string{"first:plain", "error:stage1", "last:stage1"}) {
		t.Fatalf("input chain = %v", inputOrder)
	}
	if !reflect.DeepEqual(resources.Inputs(), []string{"stage1:stage3"}) {
		t.Fatalf("resource inputs = %v", resources.Inputs())
	}
	if len(extensionErrors) != 2 || extensionErrors[1].Event != "input" || extensionErrors[1].HandlerIndex != 1 || !errors.Is(extensionErrors[1].Cause, inputFailure) {
		t.Fatalf("input extension errors = %#v", extensionErrors)
	}
	requests := implementation.Requests()
	if len(requests) != 1 || !reflect.DeepEqual(promptMessageTexts(requests[0].Messages()), []string{"expanded prompt"}) {
		t.Fatalf("provider requests = %#v", requests)
	}

	inputOrder = nil
	result, err = runtime.Run(context.Background(), "consume")
	if err != nil || !result.Handled() || implementation.CallCount() != 1 {
		t.Fatalf("handled input result = (%#v, %v), calls=%d", result, err, implementation.CallCount())
	}
	if !reflect.DeepEqual(inputOrder, []string{"first:consume"}) {
		t.Fatalf("handled input continued to later handlers: %v", inputOrder)
	}
}

func TestAgentSessionPromptPreflightResultMatchesOriginalAcknowledgementBoundary(t *testing.T) {
	t.Run("ordinary prompt acknowledges before Agent events and streaming state", func(t *testing.T) {
		implementation := newScriptedProvider(t, mustTextTerminal(t, "done"))
		var runtime *agent.AgentSession
		var orderMu sync.Mutex
		var order []string
		var streamingAtPreflight bool
		created, err := agent.NewSession(agent.SessionConfig{
			Provider: implementation, SessionManager: newSessionManager(t), Model: sessionTestModel(t),
			Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		runtime = created
		runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
			if event.Type() == agent.AgentStartEventType {
				orderMu.Lock()
				order = append(order, "agent_start")
				orderMu.Unlock()
			}
		})

		var callbacks []bool
		result, err := runtime.PromptWithOptions(context.Background(), "hello", agent.PromptOptions{
			PreflightResult: func(success bool) {
				callbacks = append(callbacks, success)
				streamingAtPreflight = runtime.Activity().IsStreaming
				orderMu.Lock()
				order = append(order, "preflight")
				orderMu.Unlock()
			},
		})
		if err != nil || !result.Succeeded() {
			t.Fatalf("PromptWithOptions() = (%#v, %v)", result, err)
		}
		orderMu.Lock()
		gotOrder := append([]string(nil), order...)
		orderMu.Unlock()
		if !reflect.DeepEqual(callbacks, []bool{true}) || streamingAtPreflight {
			t.Fatalf("preflight callbacks=%v streaming=%v", callbacks, streamingAtPreflight)
		}
		if !reflect.DeepEqual(gotOrder, []string{"preflight", "agent_start"}) {
			t.Fatalf("acknowledgement order = %v", gotOrder)
		}
	})

	t.Run("handled and queued prompts acknowledge without starting another run", func(t *testing.T) {
		implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{Clock: func() time.Time { return agentTestEpoch }})
		if err != nil {
			t.Fatal(err)
		}
		started := make(chan struct{})
		release := make(chan struct{})
		first, err := provider.FactoryResponseStep(func(context.Context, provider.Request, uint64) (llm.AssistantTerminal, error) {
			close(started)
			<-release
			return mustTextTerminal(t, "first"), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		second, err := provider.FixedResponseStep(mustTextTerminal(t, "second"))
		if err != nil {
			t.Fatal(err)
		}
		if err := implementation.SetResponses([]provider.ScriptStep{first, second}); err != nil {
			t.Fatal(err)
		}
		runtime, err := agent.NewSession(agent.SessionConfig{
			Provider: implementation, SessionManager: newSessionManager(t), Model: sessionTestModel(t),
			Hooks: agent.Hooks{Commands: []agent.ExtensionCommand{{Name: "handled", Handler: func(context.Context, string, *agent.AgentSession) error { return nil }}}},
			Now:   func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		var handledCallbacks []bool
		handled, err := runtime.PromptWithOptions(context.Background(), "/handled", agent.PromptOptions{
			PreflightResult: func(success bool) { handledCallbacks = append(handledCallbacks, success) },
		})
		if err != nil || !handled.Handled() || !reflect.DeepEqual(handledCallbacks, []bool{true}) || implementation.CallCount() != 0 {
			t.Fatalf("handled prompt = (%#v, %v), callbacks=%v calls=%d", handled, err, handledCallbacks, implementation.CallCount())
		}

		runDone := make(chan error, 1)
		go func() {
			_, runErr := runtime.Prompt(context.Background(), "start")
			runDone <- runErr
		}()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("initial prompt did not reach provider")
		}
		var queuedCallbacks []bool
		queued, err := runtime.PromptWithOptions(context.Background(), "queued", agent.PromptOptions{
			StreamingBehavior: agent.StreamingFollowUp,
			PreflightResult:   func(success bool) { queuedCallbacks = append(queuedCallbacks, success) },
		})
		if err != nil || !queued.Handled() || !reflect.DeepEqual(queuedCallbacks, []bool{true}) {
			t.Fatalf("queued prompt = (%#v, %v), callbacks=%v", queued, err, queuedCallbacks)
		}
		close(release)
		select {
		case err := <-runDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("queued prompt did not settle")
		}
	})

	t.Run("preflight rejection reports false exactly once", func(t *testing.T) {
		runtime, err := agent.NewSession(agent.SessionConfig{
			Provider: newScriptedProvider(t), SessionManager: newSessionManager(t), Model: sessionTestModel(t),
			Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		var callbacks []bool
		_, err = runtime.PromptWithOptions(context.Background(), "rejected", agent.PromptOptions{
			StreamingBehavior: agent.StreamingBehavior("invalid"),
			PreflightResult:   func(success bool) { callbacks = append(callbacks, success) },
		})
		if err == nil || !reflect.DeepEqual(callbacks, []bool{false}) || runtime.Activity().IsStreaming {
			t.Fatalf("rejected prompt error=%v callbacks=%v activity=%#v", err, callbacks, runtime.Activity())
		}
	})
}

func TestAgentSessionPromptQueuesProcessedInputAndRunsCommandsWhileStreaming(t *testing.T) {
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{Clock: func() time.Time { return agentTestEpoch }})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	first, err := provider.FactoryResponseStep(func(context.Context, provider.Request, uint64) (llm.AssistantTerminal, error) {
		close(started)
		<-release
		return mustTextTerminal(t, "first done"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.FixedResponseStep(mustTextTerminal(t, "queue done"))
	if err != nil {
		t.Fatal(err)
	}
	if err := implementation.SetResponses([]provider.ScriptStep{first, second}); err != nil {
		t.Fatal(err)
	}
	resources := &promptPipelineResources{expansions: map[string]string{"hook:queued": "expanded queued"}}
	var inputEvents []agent.InputEvent
	var commandArgs []string
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: newSessionManager(t), Model: sessionTestModel(t), Resources: resources,
		Hooks: agent.Hooks{
			Commands: []agent.ExtensionCommand{{Name: "now", Handler: func(_ context.Context, args string, _ *agent.AgentSession) error {
				commandArgs = append(commandArgs, args)
				return nil
			}}},
			InputHandlers: []agent.InputHook{func(_ context.Context, event agent.InputEvent) (agent.InputResult, error) {
				inputEvents = append(inputEvents, event)
				if event.Text == "queued" {
					return agent.InputResult{Action: agent.InputTransform, Text: "hook:queued"}, nil
				}
				return agent.InputResult{Action: agent.InputContinue}, nil
			}},
		},
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	type runOutcome struct {
		result agent.Result
		err    error
	}
	done := make(chan runOutcome, 1)
	go func() {
		result, runErr := runtime.Run(context.Background(), "start")
		done <- runOutcome{result: result, err: runErr}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("initial prompt did not reach provider")
	}

	commandResult, err := runtime.Run(context.Background(), "/now immediately")
	if err != nil || !commandResult.Handled() || !reflect.DeepEqual(commandArgs, []string{"immediately"}) {
		t.Fatalf("streaming command = (%#v, %v), args=%v", commandResult, err, commandArgs)
	}
	queued, err := runtime.PromptWithOptions(context.Background(), "queued", agent.PromptOptions{StreamingBehavior: agent.StreamingFollowUp})
	if err != nil || !queued.Handled() {
		t.Fatalf("streaming prompt = (%#v, %v)", queued, err)
	}
	if err := runtime.FollowUp("/now cannot-queue"); !errors.Is(err, agent.ErrInvalidQueueMessage) {
		t.Fatalf("queued extension command error = %v", err)
	}
	if len(inputEvents) != 2 || inputEvents[0].Text != "start" || inputEvents[0].StreamingBehavior != "" ||
		inputEvents[1].Text != "queued" || inputEvents[1].StreamingBehavior != agent.StreamingFollowUp {
		t.Fatalf("input events = %#v", inputEvents)
	}
	close(release)
	select {
	case outcome := <-done:
		if outcome.err != nil || !outcome.result.Succeeded() {
			t.Fatalf("initial run = (%#v, %v)", outcome.result, outcome.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued run did not settle")
	}
	requests := implementation.Requests()
	if len(requests) != 2 {
		t.Fatalf("provider requests = %d", len(requests))
	}
	texts := promptMessageTexts(requests[1].Messages())
	if len(texts) == 0 || texts[len(texts)-1] != "expanded queued" {
		t.Fatalf("queued provider context = %v", texts)
	}
}

func TestAgentSessionSendUserMessageSkipsCommandsAndResourceExpansion(t *testing.T) {
	resources := &promptPipelineResources{expansions: map[string]string{"extension:/cmd raw": "must not expand"}}
	implementation := newScriptedProvider(t, mustTextTerminal(t, "done"))
	var commandCalls int
	var source agent.InputSource
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: newSessionManager(t), Model: sessionTestModel(t), Resources: resources,
		Hooks: agent.Hooks{
			Commands: []agent.ExtensionCommand{{Name: "cmd", Handler: func(context.Context, string, *agent.AgentSession) error {
				commandCalls++
				return nil
			}}},
			InputHandlers: []agent.InputHook{func(_ context.Context, event agent.InputEvent) (agent.InputResult, error) {
				source = event.Source
				return agent.InputResult{Action: agent.InputTransform, Text: "extension:" + event.Text}, nil
			}},
		},
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.SendUserMessage(context.Background(), "/cmd raw", agent.UserMessageOptions{})
	if err != nil || !result.Succeeded() || commandCalls != 0 || source != agent.InputExtension || len(resources.Inputs()) != 0 {
		t.Fatalf("SendUserMessage = (%#v, %v), commands=%d source=%q resources=%v", result, err, commandCalls, source, resources.Inputs())
	}
	requests := implementation.Requests()
	if len(requests) != 1 || !reflect.DeepEqual(promptMessageTexts(requests[0].Messages()), []string{"extension:/cmd raw"}) {
		t.Fatalf("extension user request = %#v", requests)
	}
}

func TestAgentSessionCustomMessageIdleAndNextTurnOrdering(t *testing.T) {
	implementation := newScriptedProvider(t, mustTextTerminal(t, "done"))
	before, err := agentmsg.NewCustomText("before", "before context", false, nil, agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	var promptRoles []agentmsg.Role
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: newSessionManager(t), Model: sessionTestModel(t),
		Hooks: agent.Hooks{BeforeAgentStart: func(_ context.Context, event agent.BeforeAgentStartEvent) (agent.BeforeAgentStartResult, error) {
			promptRoles = make([]agentmsg.Role, len(event.PromptMessages))
			for index, message := range event.PromptMessages {
				promptRoles[index] = message.Role()
			}
			return agent.BeforeAgentStartResult{ExtraMessages: []agentmsg.Message{before}}, nil
		}},
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	var observed []string
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		switch event := event.(type) {
		case agent.MessageStartEvent:
			observed = append(observed, "start:"+string(event.Message.Role()))
		case agent.MessageEndEvent:
			observed = append(observed, "end:"+string(event.Message.Role()))
		}
	})

	idle, err := runtime.SendCustomMessage(context.Background(), customTextInput("idle", "idle context", true), agent.CustomMessageOptions{})
	if err != nil || !idle.Handled() || implementation.CallCount() != 0 {
		t.Fatalf("idle custom = (%#v, %v), calls=%d", idle, err, implementation.CallCount())
	}
	if !reflect.DeepEqual(observed, []string{"start:custom", "end:custom"}) {
		t.Fatalf("idle custom events = %v", observed)
	}
	if messages := runtime.State().Active.Messages(); len(messages) != 1 || messages[0].Role() != agentmsg.RoleCustom {
		t.Fatalf("idle custom state = %#v", messages)
	}
	if entries := runtime.SessionManager().Entries(); len(entries) != 1 || entries[0].Type() != "custom_message" {
		t.Fatalf("idle custom entries = %#v", entries)
	}

	next, err := runtime.SendCustomMessage(context.Background(), customTextInput("pending", "pending context", true), agent.CustomMessageOptions{DeliverAs: agent.DeliverCustomNextTurn})
	if err != nil || !next.Handled() || len(runtime.State().Active.Messages()) != 1 || len(runtime.SessionManager().Entries()) != 1 {
		t.Fatalf("next-turn staging = (%#v, %v), messages=%d entries=%d", next, err, len(runtime.State().Active.Messages()), len(runtime.SessionManager().Entries()))
	}
	result, err := runtime.Run(context.Background(), "normal")
	if err != nil || !result.Succeeded() {
		t.Fatalf("prompt with next-turn = (%#v, %v)", result, err)
	}
	if !reflect.DeepEqual(promptRoles, []agentmsg.Role{agentmsg.RoleUser, agentmsg.RoleCustom}) {
		t.Fatalf("before_agent_start prompt roles = %v", promptRoles)
	}
	messages := runtime.State().Active.Messages()
	wantRoles := []agentmsg.Role{agentmsg.RoleCustom, agentmsg.RoleUser, agentmsg.RoleCustom, agentmsg.RoleCustom, agentmsg.RoleAssistant}
	gotRoles := make([]agentmsg.Role, len(messages))
	for index, message := range messages {
		gotRoles[index] = message.Role()
	}
	if !reflect.DeepEqual(gotRoles, wantRoles) {
		t.Fatalf("agent message order = %v, want %v", gotRoles, wantRoles)
	}
	requests := implementation.Requests()
	if len(requests) != 1 || !reflect.DeepEqual(promptMessageTexts(requests[0].Messages()), []string{"idle context", "normal", "pending context", "before context"}) {
		t.Fatalf("provider custom context = %#v", requests)
	}
}

func TestAgentSessionNextTurnMessageSurvivesRejectedPromptPreflight(t *testing.T) {
	implementation := newScriptedProvider(t, mustTextTerminal(t, "done"))
	accessFailure := &agent.ModelAccessError{Message: "temporarily unavailable"}
	var accessCalls int
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: newSessionManager(t), Model: sessionTestModel(t),
		ValidateModelAccess: func(context.Context, provider.Model) error {
			accessCalls++
			if accessCalls == 1 {
				return accessFailure
			}
			return nil
		},
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.SendCustomMessage(context.Background(), customTextInput("pending", "keep pending", false), agent.CustomMessageOptions{DeliverAs: agent.DeliverCustomNextTurn}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "rejected"); !errors.Is(err, agent.ErrModelAccess) {
		t.Fatalf("rejected prompt error = %v", err)
	}
	if implementation.CallCount() != 0 || len(runtime.State().Active.Messages()) != 0 {
		t.Fatalf("rejected prompt changed runtime: calls=%d messages=%d", implementation.CallCount(), len(runtime.State().Active.Messages()))
	}
	result, err := runtime.Run(context.Background(), "accepted")
	if err != nil || !result.Succeeded() {
		t.Fatalf("accepted prompt = (%#v, %v)", result, err)
	}
	requests := implementation.Requests()
	if len(requests) != 1 || !reflect.DeepEqual(promptMessageTexts(requests[0].Messages()), []string{"accepted", "keep pending"}) {
		t.Fatalf("retained next-turn request = %#v", requests)
	}
}

func TestAgentSessionCustomMessageTriggerAndStreamingDelivery(t *testing.T) {
	t.Run("idle trigger bypasses ordinary prompt preflight", func(t *testing.T) {
		implementation := newScriptedProvider(t, mustTextTerminal(t, "triggered"))
		var beforeCalls int
		runtime, err := agent.NewSession(agent.SessionConfig{
			Provider: implementation, SessionManager: newSessionManager(t), Model: sessionTestModel(t),
			Hooks: agent.Hooks{BeforeAgentStart: func(context.Context, agent.BeforeAgentStartEvent) (agent.BeforeAgentStartResult, error) {
				beforeCalls++
				return agent.BeforeAgentStartResult{}, nil
			}},
			Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		result, err := runtime.SendCustomMessage(context.Background(), customTextInput("trigger", "run now", true), agent.CustomMessageOptions{
			TriggerTurn: true, DeliverAs: agent.DeliverCustomFollowUp,
		})
		if err != nil || !result.Succeeded() || result.Handled() || beforeCalls != 0 {
			t.Fatalf("trigger custom = (%#v, %v), before=%d", result, err, beforeCalls)
		}
		messages := runtime.State().Active.Messages()
		if len(messages) != 2 || messages[0].Role() != agentmsg.RoleCustom || messages[1].Role() != agentmsg.RoleAssistant {
			t.Fatalf("trigger state = %#v", messages)
		}
		requests := implementation.Requests()
		if len(requests) != 1 || !reflect.DeepEqual(promptMessageTexts(requests[0].Messages()), []string{"run now"}) {
			t.Fatalf("trigger provider context = %#v", requests)
		}
	})

	t.Run("active trigger follows deliverAs queue", func(t *testing.T) {
		implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{Clock: func() time.Time { return agentTestEpoch }})
		if err != nil {
			t.Fatal(err)
		}
		started := make(chan struct{})
		release := make(chan struct{})
		first, err := provider.FactoryResponseStep(func(context.Context, provider.Request, uint64) (llm.AssistantTerminal, error) {
			close(started)
			<-release
			return mustTextTerminal(t, "first"), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		second, err := provider.FixedResponseStep(mustTextTerminal(t, "second"))
		if err != nil {
			t.Fatal(err)
		}
		if err := implementation.SetResponses([]provider.ScriptStep{first, second}); err != nil {
			t.Fatal(err)
		}
		runtime, err := agent.NewSession(agent.SessionConfig{
			Provider: implementation, SessionManager: newSessionManager(t), Model: sessionTestModel(t),
			Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, runErr := runtime.Run(context.Background(), "start")
			done <- runErr
		}()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("initial custom-delivery prompt did not start")
		}
		queued, err := runtime.SendCustomMessage(context.Background(), customTextInput("queued", "after current", false), agent.CustomMessageOptions{
			TriggerTurn: true, DeliverAs: agent.DeliverCustomFollowUp,
		})
		if err != nil || !queued.Handled() {
			t.Fatalf("queued custom = (%#v, %v)", queued, err)
		}
		close(release)
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("custom follow-up did not settle")
		}
		requests := implementation.Requests()
		if len(requests) != 2 {
			t.Fatalf("custom follow-up calls = %d", len(requests))
		}
		texts := promptMessageTexts(requests[1].Messages())
		if len(texts) == 0 || texts[len(texts)-1] != "after current" {
			t.Fatalf("custom follow-up context = %v", texts)
		}
	})
}

func TestAgentSessionDuplicateCommandsUseResolvedInvocationNames(t *testing.T) {
	var called []string
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: newSessionManager(t), Model: sessionTestModel(t),
		Hooks: agent.Hooks{Commands: []agent.ExtensionCommand{
			{Name: "same", Handler: func(context.Context, string, *agent.AgentSession) error { called = append(called, "first"); return nil }},
			{Name: "same", Handler: func(context.Context, string, *agent.AgentSession) error {
				called = append(called, "second")
				return nil
			}},
		}},
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 2; index++ {
		result, err := runtime.Run(context.Background(), fmt.Sprintf("/same:%d", index))
		if err != nil || !result.Handled() {
			t.Fatalf("resolved command %d = (%#v, %v)", index, result, err)
		}
	}
	if !reflect.DeepEqual(called, []string{"first", "second"}) {
		t.Fatalf("resolved commands = %v", called)
	}
}
