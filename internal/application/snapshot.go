package application

import "errors"

// Snapshot is the surface-neutral application catalog and event cursor. Project
// counts and activity are derived from the same session list that it returns.
type Snapshot struct {
	Revision          uint64
	AgentDir          string
	DefaultCWD        string
	Sessions          []SessionInfo
	Projects          []ProjectInfo
	RunningSessionIDs []string
}

func (s *Service) Snapshot() (Snapshot, error) {
	if s == nil {
		return Snapshot{}, errors.New("application service is unavailable")
	}
	// Events published during the read remain replayable by every surface.
	revision := s.CurrentRevision()
	sessions, err := s.ListSessions()
	if err != nil {
		return Snapshot{}, err
	}
	projects, err := s.projectsForSessions(sessions)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{
		Revision: revision, AgentDir: s.AgentDir(), DefaultCWD: s.DefaultCWD(),
		Sessions: sessions, Projects: projects, RunningSessionIDs: s.RunningIDs(),
	}, nil
}
