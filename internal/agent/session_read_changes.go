package agent

import (
	"crypto/rand"
	"sync"
)

// SessionReadVersion is invalidation metadata for independently owned product
// resources. It does not turn those resources into a global atomic snapshot.
type SessionReadVersion struct {
	Generation string
	Activity   uint64
	Bash       uint64
	Controls   uint64
}

type ResourceRevision struct {
	Generation string
	Revision   uint64
}

type ActivityReadSnapshot struct {
	Version ResourceRevision
	State   SessionActivity
}

type sessionReadChanges struct {
	mu       sync.Mutex
	version  SessionReadVersion
	next     uint64
	watchers map[uint64]chan SessionReadVersion
}

func (s *AgentSession) readChangesVersionLocked() SessionReadVersion {
	if s.readChanges.version.Generation == "" {
		s.readChanges.version.Generation = rand.Text()
	}
	return s.readChanges.version
}

// Called with the relevant owner's lock held. It only delivers coalescible
// version metadata; it never calls observers or reads another state owner.
func (s *AgentSession) notifyReadChange(resource string) {
	s.readChanges.mu.Lock()
	defer s.readChanges.mu.Unlock()
	switch resource {
	case "activity":
		s.readChanges.version.Activity++
	case "bash":
		s.readChanges.version.Bash++
	case "controls":
		s.readChanges.version.Controls++
	}
	version := s.readChangesVersionLocked()
	for _, watcher := range s.readChanges.watchers {
		select {
		case <-watcher:
		default:
		}
		watcher <- version
	}
}

func (s *AgentSession) SubscribeReadChanges() (SessionReadVersion, <-chan SessionReadVersion, func()) {
	s.readChanges.mu.Lock()
	defer s.readChanges.mu.Unlock()
	if s.readChanges.watchers == nil {
		s.readChanges.watchers = make(map[uint64]chan SessionReadVersion)
	}
	s.readChanges.next++
	id := s.readChanges.next
	watcher := make(chan SessionReadVersion, 1)
	s.readChanges.watchers[id] = watcher
	var once sync.Once
	return s.readChangesVersionLocked(), watcher, func() {
		once.Do(func() {
			s.readChanges.mu.Lock()
			delete(s.readChanges.watchers, id)
			close(watcher)
			s.readChanges.mu.Unlock()
		})
	}
}

func (s *AgentSession) SubscribeConfigurationChanges() (ReadVersion, <-chan ReadVersion, func()) {
	return s.loop.SubscribeConfigurationChanges()
}

func (s *AgentSession) activityLocked() SessionActivity {
	if s.run == nil {
		return SessionActivity{Phase: PhaseIdle}
	}
	return SessionActivity{Phase: s.run.phase, IsStreaming: s.run.agentRunActive,
		IsCompacting: s.run.compaction != nil || s.run.branchCancellation != nil,
		RetryAttempt: s.run.retryAttempt, RetryWaiting: s.run.retryCancel != nil}
}

// Every lifecycle unlock checks the small projection while still holding its
// owning lock. This includes retryCancel changes after auto_retry_start and
// settlement after agent_end, neither of which has a matching canonical event.
func (s *AgentSession) unlockLifecycle() {
	s.updateReadActivityLocked()
	s.lifecycleMu.Unlock()
}

func (s *AgentSession) updateReadActivityLocked() {
	current := s.activityLocked()
	if current != s.lastReadActivity || s.closed != s.lastReadClosed {
		s.lastReadActivity, s.lastReadClosed = current, s.closed
		s.notifyReadChange("activity")
	}
}

func (s *AgentSession) ReadActivity() ActivityReadSnapshot {
	if s == nil {
		return ActivityReadSnapshot{State: SessionActivity{Phase: PhaseIdle}}
	}
	s.lifecycleMu.Lock()
	defer s.unlockLifecycle()
	s.updateReadActivityLocked()
	s.readChanges.mu.Lock()
	version := s.readChangesVersionLocked()
	s.readChanges.mu.Unlock()
	return ActivityReadSnapshot{State: s.activityLocked(), Version: ResourceRevision{Generation: version.Generation, Revision: version.Activity}}
}

func (s *AgentSession) readBashRunning() (bool, ResourceRevision) {
	s.bashMu.Lock()
	defer s.bashMu.Unlock()
	s.readChanges.mu.Lock()
	version := s.readChangesVersionLocked()
	s.readChanges.mu.Unlock()
	return len(s.bashRuns) != 0, ResourceRevision{Generation: version.Generation, Revision: version.Bash}
}
