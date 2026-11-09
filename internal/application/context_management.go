package application

import (
	"context"
	"fmt"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/session"
)

const (
	CommandContextChange CommandType = "context_change"
	CommandContextStatus CommandType = "context_status"
)

type ContextChangeCommand struct{ Request agent.ContextChangeRequest }

func (ContextChangeCommand) Type() CommandType   { return CommandContextChange }
func (ContextChangeCommand) applicationCommand() {}

type ContextStatusCommand struct{}

func (ContextStatusCommand) Type() CommandType   { return CommandContextStatus }
func (ContextStatusCommand) applicationCommand() {}

type ContextChangeResult struct{ Result agent.ContextChangeResult }

func (ContextChangeResult) CommandType() CommandType  { return CommandContextChange }
func (ContextChangeResult) applicationCommandResult() {}

type ContextStatusResult struct{ Status agent.ContextStatus }

func (ContextStatusResult) CommandType() CommandType  { return CommandContextStatus }
func (ContextStatusResult) applicationCommandResult() {}

// SessionContextAccess is a borrowed typed view of one already-owned session.
// It does not create a Runtime, hold a second transcript, or control lifecycle.
// The CLI's local connection uses the same commands as ApplicationSession.
type SessionContextAccess struct{ current *agent.AgentSession }

func BorrowSessionContext(current *agent.AgentSession) SessionContextAccess {
	return SessionContextAccess{current: current}
}

func (a SessionContextAccess) SessionID() string { return a.current.SessionManager().SessionID() }
func (a SessionContextAccess) Info() SessionInfo {
	return discoveredSessionInfo(a.current.SessionManager().Summary(), nil)
}
func (a SessionContextAccess) ReadHistoryView(query session.HistoryViewQuery) (session.HistoryView, error) {
	return a.current.SessionManager().ReadHistoryView(query)
}
func (a SessionContextAccess) Dispatch(ctx context.Context, command Command) (CommandResult, error) {
	switch value := command.(type) {
	case ContextChangeCommand:
		result, err := a.current.RequestContextChange(ctx, value.Request)
		return ContextChangeResult{Result: result}, err
	case ContextStatusCommand:
		status, err := a.current.ContextStatus()
		return ContextStatusResult{Status: status}, err
	default:
		return nil, fmt.Errorf("%w: unsupported context command", ErrInvalidCommand)
	}
}

func (s *Service) ReadHistoryView(id string, query session.HistoryViewQuery) (session.HistoryView, error) {
	var result session.HistoryView
	err := s.readSessionResource(id, func(reader sessionResourceReader) (string, error) {
		var err error
		result, err = reader.ReadHistoryView(query)
		return result.SessionID, err
	})
	return result, err
}

// ReadHistoryFile is an explicit read-only source selection. It never claims
// the session writer or creates a Runtime for a historical file.
func ReadHistoryFile(path string, query session.HistoryViewQuery) (session.HistoryView, error) {
	index, err := session.OpenReadIndex(path)
	if err != nil {
		return session.HistoryView{}, err
	}
	return index.ReadHistoryView(query)
}
