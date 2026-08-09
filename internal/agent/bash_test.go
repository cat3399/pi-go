package agent_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

type standaloneBashFunc func(context.Context, string, func(string)) (agent.BashResult, error)

func (function standaloneBashFunc) ExecuteBash(ctx context.Context, command string, onChunk func(string)) (agent.BashResult, error) {
	return function(ctx, command, onChunk)
}

func TestAgentSessionExecuteBashStreamsRecordsPrefixAndContextVisibility(t *testing.T) {
	code := 7
	executor := standaloneBashFunc(func(_ context.Context, command string, onChunk func(string)) (agent.BashResult, error) {
		if command != "setup aliases\nprintf hello" {
			t.Fatalf("resolved command = %q", command)
		}
		onChunk("hel")
		onChunk("lo")
		return agent.BashResult{Output: "hello", ExitCode: &code}, nil
	})
	implementation := newScriptedProvider(t, mustTextTerminal(t, "done"))
	transcript := newSessionManager(t)
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: transcript, Model: sessionTestModel(t),
		StandaloneBash: executor, BashCommandPrefix: "setup aliases",
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	var callback []string
	var updates []agent.BashExecutionUpdateEvent
	messageEnds := 0
	id := "bash-1"
	unsubscribe := runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		switch event := event.(type) {
		case agent.BashExecutionUpdateEvent:
			updates = append(updates, event)
		case agent.MessageEndEvent:
			messageEnds++
		}
	})
	defer unsubscribe()
	result, err := runtime.ExecuteBash(context.Background(), "printf hello", func(delta string) {
		callback = append(callback, delta)
	}, agent.ExecuteBashOptions{ID: &id})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == nil || *result.ExitCode != 7 || result.Output != "hello" || runtime.IsBashRunning() || runtime.HasPendingBashMessages() {
		t.Fatalf("bash result/state = %#v running=%t pending=%t", result, runtime.IsBashRunning(), runtime.HasPendingBashMessages())
	}
	if !reflect.DeepEqual(callback, []string{"hel", "lo"}) || len(updates) != 2 || updates[0].ID == nil || *updates[0].ID != id ||
		updates[0].Delta != "hel" || updates[1].Delta != "lo" || messageEnds != 0 {
		t.Fatalf("bash events = callback %#v updates %#v messageEnds=%d", callback, updates, messageEnds)
	}
	stateMessages := runtime.State().Active.Messages()
	stored, ok := stateMessages[len(stateMessages)-1].(agentmsg.BashExecution)
	if !ok || stored.Command != "printf hello" || stored.Output != "hello" || stored.ExitCode == nil || *stored.ExitCode != 7 {
		t.Fatalf("state bash message = %#v", stateMessages)
	}
	if err := runtime.RecordBashResult(context.Background(), "secret", agent.BashResult{Output: "hidden"}, agent.RecordBashOptions{ExcludeFromContext: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "next"); err != nil {
		t.Fatal(err)
	}
	requests := implementation.Requests()
	if len(requests) != 1 {
		t.Fatalf("provider requests = %d", len(requests))
	}
	messages := requests[0].Messages()
	if len(messages) != 2 {
		t.Fatalf("provider messages = %#v, want visible bash plus prompt", messages)
	}
	first, ok := messages[0].(llm.UserTextMessage)
	if !ok || len(first.Content()) != 1 || first.Content()[0].Text() != "Ran `printf hello`\n```\nhello\n```\n\nCommand exited with code 7" {
		t.Fatalf("visible bash projection = %#v", messages[0])
	}
}

func TestAgentSessionExecuteBashUsesPerCallOperationsAndLivePrefix(t *testing.T) {
	prefix := "first setup"
	var commands []string
	override := standaloneBashFunc(func(_ context.Context, command string, _ func(string)) (agent.BashResult, error) {
		commands = append(commands, command)
		code := 0
		return agent.BashResult{ExitCode: &code}, nil
	})
	defaultExecutor := standaloneBashFunc(func(context.Context, string, func(string)) (agent.BashResult, error) {
		return agent.BashResult{}, errors.New("default executor must not run")
	})
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: newSessionManager(t), Model: sessionTestModel(t),
		StandaloneBash: defaultExecutor, ResolveBashCommandPrefix: func() string { return prefix },
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	if _, err := runtime.ExecuteBash(context.Background(), "one", nil, agent.ExecuteBashOptions{Executor: override}); err != nil {
		t.Fatal(err)
	}
	prefix = "second setup"
	if _, err := runtime.ExecuteBash(context.Background(), "two", nil, agent.ExecuteBashOptions{Executor: override}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(commands, []string{"first setup\none", "second setup\ntwo"}) {
		t.Fatalf("live prefixed commands = %#v", commands)
	}
	messages := runtime.State().Active.Messages()
	if len(messages) != 2 {
		t.Fatalf("per-call bash records = %#v", messages)
	}
	first, firstOK := messages[0].(agentmsg.BashExecution)
	second, secondOK := messages[1].(agentmsg.BashExecution)
	if !firstOK || !secondOK || first.Command != "one" || second.Command != "two" {
		t.Fatalf("stored commands included runtime prefix = %#v", messages)
	}
}

func TestAgentSessionDefersBashMessageUntilStreamingRunSettlement(t *testing.T) {
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{Clock: func() time.Time { return agentTestEpoch }})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	step, err := provider.FactoryResponseStep(func(context.Context, provider.Request, uint64) (llm.AssistantTerminal, error) {
		close(started)
		<-release
		return mustTextTerminal(t, "done"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := implementation.SetResponses([]provider.ScriptStep{step}); err != nil {
		t.Fatal(err)
	}
	transcript := newSessionManager(t)
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: transcript, Model: sessionTestModel(t),
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	done := make(chan error, 1)
	go func() {
		_, runErr := runtime.Run(context.Background(), "start")
		done <- runErr
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	if err := runtime.RecordBashResult(context.Background(), "echo hi", agent.BashResult{Output: "hi"}, agent.RecordBashOptions{}); err != nil {
		t.Fatal(err)
	}
	if !runtime.HasPendingBashMessages() {
		t.Fatal("streaming bash result was not deferred")
	}
	for _, message := range runtime.State().Active.Messages() {
		if message.Role() == agentmsg.RoleBashExecution {
			t.Fatal("pending bash message entered active Agent state early")
		}
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent run did not settle")
	}
	if runtime.HasPendingBashMessages() {
		t.Fatal("settled run retained pending bash message")
	}
	messages := runtime.State().Active.Messages()
	if len(messages) != 3 || messages[0].Role() != agentmsg.RoleUser || messages[1].Role() != agentmsg.RoleAssistant || messages[2].Role() != agentmsg.RoleBashExecution {
		t.Fatalf("settled message order = %#v", messages)
	}
	durable := transcript.BuildContext().AgentMessages()
	if len(durable) != 3 || durable[2].Role() != agentmsg.RoleBashExecution {
		t.Fatalf("durable message order = %#v", durable)
	}
}

func TestAgentSessionKeepsBashDeferredAcrossRetryWait(t *testing.T) {
	enteredRetry := make(chan struct{})
	releaseRetry := make(chan struct{})
	implementation := newScriptedProvider(t, sessionHTTPFailure(t, 429), mustTextTerminal(t, "recovered"))
	transcript := newSessionManager(t)
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: transcript, Model: sessionTestModel(t),
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
		Retry: agent.RetryPolicy{MaxAttempts: 2, Sleep: func(context.Context, time.Duration) error {
			close(enteredRetry)
			<-releaseRetry
			return nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	done := make(chan error, 1)
	go func() {
		_, runErr := runtime.Run(context.Background(), "retry")
		done <- runErr
	}()
	select {
	case <-enteredRetry:
	case <-time.After(time.Second):
		t.Fatal("agent did not enter retry wait")
	}
	if err := runtime.RecordBashResult(context.Background(), "echo during-retry", agent.BashResult{Output: "during-retry"}, agent.RecordBashOptions{}); err != nil {
		t.Fatal(err)
	}
	if !runtime.HasPendingBashMessages() {
		t.Fatal("bash result completed during retry wait was not deferred")
	}
	for _, message := range runtime.State().Active.Messages() {
		if message.Role() == agentmsg.RoleBashExecution {
			t.Fatal("retry-wait bash message entered active Agent state early")
		}
	}
	close(releaseRetry)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retried agent run did not settle")
	}
	if runtime.HasPendingBashMessages() {
		t.Fatal("settled retry retained pending bash message")
	}
	state := runtime.State().Active.Messages()
	if len(state) != 3 || state[0].Role() != agentmsg.RoleUser || state[1].Role() != agentmsg.RoleAssistant || state[2].Role() != agentmsg.RoleBashExecution {
		t.Fatalf("settled retry state order = %#v", state)
	}
	durable := transcript.BuildContext().AgentMessages()
	if len(durable) != 4 || durable[0].Role() != agentmsg.RoleUser || durable[1].Role() != agentmsg.RoleAssistant ||
		durable[2].Role() != agentmsg.RoleAssistant || durable[3].Role() != agentmsg.RoleBashExecution {
		t.Fatalf("settled retry durable order = %#v", durable)
	}
}

type controlledStandaloneBash struct {
	mu       sync.Mutex
	started  map[string]chan struct{}
	releases map[string]chan struct{}
}

func (b *controlledStandaloneBash) ExecuteBash(ctx context.Context, command string, onChunk func(string)) (agent.BashResult, error) {
	b.mu.Lock()
	started, release := b.started[command], b.releases[command]
	b.mu.Unlock()
	onChunk("partial:" + command)
	close(started)
	select {
	case <-release:
		code := 0
		return agent.BashResult{Output: "complete:" + command, ExitCode: &code}, nil
	case <-ctx.Done():
		code := 0
		return agent.BashResult{Output: "partial:" + command, ExitCode: &code}, errors.New("custom operation aborted")
	}
}

func TestAgentSessionAbortBashTracksEveryConcurrentExecution(t *testing.T) {
	executor := &controlledStandaloneBash{
		started:  map[string]chan struct{}{"first": make(chan struct{}), "second": make(chan struct{})},
		releases: map[string]chan struct{}{"first": make(chan struct{}), "second": make(chan struct{})},
	}
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: newSessionManager(t), Model: sessionTestModel(t),
		StandaloneBash: executor, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	type outcome struct {
		result agent.BashResult
		err    error
	}
	firstDone, secondDone := make(chan outcome, 1), make(chan outcome, 1)
	go func() {
		result, executeErr := runtime.ExecuteBash(context.Background(), "first", nil, agent.ExecuteBashOptions{})
		firstDone <- outcome{result: result, err: executeErr}
	}()
	go func() {
		result, executeErr := runtime.ExecuteBash(context.Background(), "second", nil, agent.ExecuteBashOptions{})
		secondDone <- outcome{result: result, err: executeErr}
	}()
	for _, command := range []string{"first", "second"} {
		select {
		case <-executor.started[command]:
		case <-time.After(time.Second):
			t.Fatalf("%s bash did not start", command)
		}
	}
	close(executor.releases["first"])
	first := <-firstDone
	if first.err != nil || first.result.Cancelled || !runtime.IsBashRunning() {
		t.Fatalf("first completion = (%#v, %v), running=%t", first.result, first.err, runtime.IsBashRunning())
	}
	runtime.AbortBash()
	second := <-secondDone
	if second.err != nil || !second.result.Cancelled || second.result.ExitCode != nil || runtime.IsBashRunning() {
		t.Fatalf("second completion = (%#v, %v), running=%t", second.result, second.err, runtime.IsBashRunning())
	}
	messages := runtime.State().Active.Messages()
	if len(messages) != 2 {
		t.Fatalf("bash messages = %#v", messages)
	}
	firstMessage, firstOK := messages[0].(agentmsg.BashExecution)
	secondMessage, secondOK := messages[1].(agentmsg.BashExecution)
	if !firstOK || !secondOK || firstMessage.Command != "first" || secondMessage.Command != "second" || !secondMessage.Cancelled {
		t.Fatalf("completion order/messages = %#v", messages)
	}
}

func TestAgentSessionShutdownAbortsAndPersistsBashBeforeShutdownHook(t *testing.T) {
	started := make(chan struct{})
	executor := standaloneBashFunc(func(ctx context.Context, _ string, onChunk func(string)) (agent.BashResult, error) {
		onChunk("partial")
		close(started)
		<-ctx.Done()
		return agent.BashResult{Output: "partial"}, errors.New("aborted")
	})
	transcript := newSessionManager(t)
	hookSawCancelled := false
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: transcript, Model: sessionTestModel(t), StandaloneBash: executor,
		Hooks: agent.Hooks{SessionShutdown: func(_ context.Context, event agent.SessionShutdownHookEvent) error {
			if event.Reason != agent.ShutdownQuit {
				t.Fatalf("shutdown reason = %q", event.Reason)
			}
			messages := transcript.BuildContext().AgentMessages()
			if len(messages) == 0 {
				t.Error("shutdown hook observed no persisted bash message")
				return nil
			}
			last, ok := messages[len(messages)-1].(agentmsg.BashExecution)
			hookSawCancelled = ok && last.Cancelled && last.Output == "partial"
			return nil
		}},
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result agent.BashResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, executeErr := runtime.ExecuteBash(context.Background(), "wait", nil, agent.ExecuteBashOptions{})
		done <- outcome{result: result, err: executeErr}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("bash did not start")
	}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	result := <-done
	if result.err != nil || !result.result.Cancelled || !hookSawCancelled || runtime.IsBashRunning() {
		t.Fatalf("shutdown bash = (%#v, %v), hook=%t running=%t", result.result, result.err, hookSawCancelled, runtime.IsBashRunning())
	}
}
