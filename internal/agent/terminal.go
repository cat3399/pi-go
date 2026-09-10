package agent

import (
	"context"

	"github.com/cat3399/pi-go/internal/terminal"
)

func (s *AgentSession) Terminal(ctx context.Context, req terminal.Request) (terminal.Result, error) {
	if err := s.rejectIfClosed(); err != nil {
		return terminal.Result{}, err
	}
	switch req.Action {
	case terminal.Open, terminal.Run, terminal.Write, terminal.Interrupt, terminal.Close:
		// Admit user mutations only between complete session runs. The low
		// Agent's streaming state excludes retry waits and other run phases.
		s.lifecycleMu.Lock()
		running := s.run != nil
		s.lifecycleMu.Unlock()
		if running {
			return terminal.Result{}, ErrBusy
		}
	}
	return s.terminals.View(ctx, req)
}

func (s *AgentSession) HasLiveTerminals() bool {
	return s != nil && s.terminals.HasLive()
}
