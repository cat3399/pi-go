package agent

import (
	"context"
	"fmt"
	"sync"

	"github.com/cat3399/pi-go/internal/session"
)

// MessageReceipt relates one Agent-assigned transient identity to the exact
// manager append. It is read metadata, separate from canonical session events.
type MessageReceipt struct {
	MessageID string
	EntryID   string
	Version   session.ReadVersion
	Accepted  bool
	Durable   bool
	Uncertain bool
}

type SessionObservation struct {
	Event     SessionEvent
	Version   ReadVersion
	MessageID string
	Receipt   *MessageReceipt
	Activity  *ActivityReadSnapshot
}

type sessionReadObserver struct {
	id       uint64
	observer func(context.Context, SessionObservation)
}

// SessionReadSnapshot keeps owner boundaries explicit. Agent is one lock-
// consistent, history-free read. History is sampled while message receipt
// publication is excluded, so no acknowledged message is still Pending.
// Activity and Controls are independent product resources, not a global
// transaction with Agent state or external settings.
type SessionReadSnapshot struct {
	Agent           ReadSnapshot
	History         session.ReadSnapshot
	Activity        SessionActivity
	ActivityVersion ResourceRevision
	BashVersion     ResourceRevision
	Controls        RuntimeControlSettings
	BashRunning     bool
}

func (s *AgentSession) ReadSnapshot() (SessionReadSnapshot, error) {
	if s == nil || s.loop == nil || s.sessionManager == nil {
		return SessionReadSnapshot{}, fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	activity := s.ReadActivity()
	bashRunning, bashVersion := s.readBashRunning()
	result := SessionReadSnapshot{Activity: activity.State, ActivityVersion: activity.Version, Controls: s.resolvedRuntimeSettings(), BashRunning: bashRunning, BashVersion: bashVersion}
	s.transcriptReadMu.Lock()
	defer s.transcriptReadMu.Unlock()
	result.Agent = s.loop.ReadSnapshot()
	var err error
	result.History, err = s.sessionManager.ReadSnapshot(session.SnapshotOptions{})
	return result, err
}

func (s *AgentSession) SubscribeObserved(observer func(context.Context, SessionObservation)) func() {
	if s == nil || observer == nil {
		return func() {}
	}
	s.lifecycleMu.Lock()
	if s.closed || s.closing {
		s.unlockLifecycle()
		return func() {}
	}
	s.mu.Lock()
	s.nextObserver++
	id := s.nextObserver
	s.readObservers = append(s.readObservers, sessionReadObserver{id: id, observer: observer})
	s.mu.Unlock()
	s.unlockLifecycle()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			for index, entry := range s.readObservers {
				if entry.id == id {
					copy(s.readObservers[index:], s.readObservers[index+1:])
					s.readObservers[len(s.readObservers)-1] = sessionReadObserver{}
					s.readObservers = s.readObservers[:len(s.readObservers)-1]
					break
				}
			}
			s.mu.Unlock()
		})
	}
}

func (s *AgentSession) emitReadObservation(ctx context.Context, observed SessionObservation) {
	s.mu.RLock()
	observers := append([]sessionReadObserver(nil), s.readObservers...)
	s.mu.RUnlock()
	for _, entry := range observers {
		copy := observed
		copy.Event = cloneSessionEvent(observed.Event)
		if observed.Receipt != nil {
			receipt := *observed.Receipt
			copy.Receipt = &receipt
		}
		if observed.Activity != nil {
			activity := *observed.Activity
			copy.Activity = &activity
		}
		entry.observer(ctx, copy)
	}
}

func (s *AgentSession) emitMessageReceipt(ctx context.Context, metadata Observation, receipt session.AppendReceipt) {
	if metadata.MessageID == "" {
		return
	}
	s.emitReadObservation(ctx, SessionObservation{
		Version: metadata.Version, MessageID: metadata.MessageID,
		Receipt: &MessageReceipt{
			MessageID: metadata.MessageID, EntryID: receipt.Entry.ID(), Version: receipt.Version,
			Accepted: receipt.Accepted, Durable: receipt.Durable, Uncertain: receipt.Uncertain,
		},
	})
}
