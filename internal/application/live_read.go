package application

import "github.com/cat3399/pi-go/internal/agent"

type LiveSnapshot struct {
	Revision      uint64
	SessionID     string
	PromptRunning bool
	State         agent.SessionReadSnapshot
}

// ReadLiveSession does not open an inactive Runtime or copy historical
// messages. Its owners supply current draft identities and resource versions.
func (s *Service) ReadLiveSession(id string) (LiveSnapshot, bool, error) {
	revision := s.CurrentRevision()
	managed, active := s.active(id)
	if !active {
		return LiveSnapshot{Revision: revision, SessionID: id}, false, nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		current, binding, err := managed.session.currentSession()
		if err != nil {
			return LiveSnapshot{}, false, err
		}
		state, err := current.ReadSnapshot()
		promptRunning := managed.session.promptRunning()
		if !managed.session.sameBinding(current, binding) {
			continue
		}
		if err != nil {
			return LiveSnapshot{}, false, err
		}
		if state.History.Info.ID != id {
			return LiveSnapshot{}, false, ErrSessionChanged
		}
		return LiveSnapshot{Revision: revision, SessionID: id, PromptRunning: promptRunning, State: state}, true, nil
	}
	return LiveSnapshot{}, false, ErrSessionChanged
}
