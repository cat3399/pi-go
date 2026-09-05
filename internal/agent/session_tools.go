package agent

import (
	"context"

	"github.com/cat3399/pi-go/internal/tool"
)

// sessionToolExecutor is the AgentSession-to-tool boundary. Like pi's tool
// definition wrapper, it supplies the current session context at execution
// time while retaining the executor's dispatch, preparation and scheduling.
// Agent and AgentLoop continue to operate on ordinary ToolExecutors.
type sessionToolExecutor struct {
	session  *AgentSession
	executor ToolExecutor
}

func (s *AgentSession) toolsWithSessionContext(executor ToolExecutor) ToolExecutor {
	if isNilInterface(executor) {
		return executor
	}
	return &sessionToolExecutor{session: s, executor: executor}
}

func (t *sessionToolExecutor) Name() string { return t.executor.Name() }
func (t *sessionToolExecutor) Supports(name string) bool {
	supported, err := supportsToolCall(t.executor, name)
	return err == nil && supported
}
func (t *sessionToolExecutor) PrepareArguments(name string, arguments any) (any, error) {
	if preparer, ok := t.executor.(NamedToolArgumentPreparer); ok {
		return preparer.PrepareArguments(name, arguments)
	}
	return arguments, nil
}
func (t *sessionToolExecutor) ToolExecutionMode(name string) (ToolExecutionMode, bool) {
	if executor, ok := t.executor.(ToolExecutionOverride); ok {
		return executor.ToolExecutionMode(name)
	}
	return 0, false
}
func (t *sessionToolExecutor) Execute(ctx context.Context, id string, arguments []byte, report func(ToolUpdate)) (ToolOutput, error) {
	return t.executor.Execute(t.executionContext(ctx), id, arguments, report)
}
func (t *sessionToolExecutor) ExecuteNamed(ctx context.Context, id, name string, arguments []byte, report func(ToolUpdate)) (ToolOutput, error) {
	return executeNamedToolSafely(t.executor, t.executionContext(ctx), id, name, arguments, report)
}
func (t *sessionToolExecutor) executionContext(ctx context.Context) context.Context {
	s := t.session
	s.selectionMu.RLock()
	state := s.loop.State()
	metadata := tool.BashSessionEnvironment{SessionID: s.sessionManager.SessionID(), ReasoningLevel: string(state.ThinkingLevel())}
	metadata.SessionFile, _ = s.sessionManager.SessionFile()
	if state.HasModel() {
		metadata.Provider = state.Model().Provider()
		metadata.Model = state.Model().ID()
	}
	s.selectionMu.RUnlock()
	return tool.WithBashSessionEnvironment(ctx, metadata)
}
