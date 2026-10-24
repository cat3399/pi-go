package session

import (
	"context"
	"crypto/rand"
	"sync"

	"github.com/cat3399/pi-go/internal/agentmsg"
)

// ReadChange is an invalidation, not a transcript event or a durable commit.
// Subscribers may receive only the latest version of several changes. The
// immutable append log remains the source for fetching the intervening entries.
type ReadChange struct {
	SessionID string
	Version   ReadVersion
	LeafID    string
	Durable   bool
	Closed    bool
	Uncertain bool
}

// SubscribeChanges atomically installs a bounded observer and returns the
// current version. No caller callback runs under a manager/store lock and a
// slow reader never delays appends. Close and cancel close the channel.
func (m *SessionManager) SubscribeChanges() (ReadChange, <-chan ReadChange, func()) {
	updates := make(chan ReadChange, 1)
	if m == nil {
		close(updates)
		return ReadChange{Closed: true}, updates, func() {}
	}
	m.mu.Lock()
	current := m.readChangeLocked()
	if current.Closed {
		close(updates)
		m.mu.Unlock()
		return current, updates, func() {}
	}
	if m.readWatchers == nil {
		m.readWatchers = make(map[uint64]chan ReadChange)
	}
	if len(m.readWatchers) == 0 {
		m.lastReadChange = current
	}
	m.nextReadWatcher++
	id := m.nextReadWatcher
	m.readWatchers[id] = updates
	m.mu.Unlock()
	var once sync.Once
	return current, updates, func() {
		once.Do(func() {
			m.mu.Lock()
			if channel, ok := m.readWatchers[id]; ok {
				delete(m.readWatchers, id)
				close(channel)
			}
			m.mu.Unlock()
		})
	}
}

// Called with m.mu held; readVersionLocked also requires store.mu.
func (m *SessionManager) readVersionLocked() ReadVersion {
	m.readOnce.Do(func() { m.readEpoch = rand.Text() })
	return ReadVersion{Generation: m.readEpoch, Log: uint64(len(m.store.entries)), View: m.store.generation}
}

func (m *SessionManager) readChangeLocked() ReadChange {
	store := m.store
	if store == nil {
		return ReadChange{Closed: true}
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	change := ReadChange{
		SessionID: store.header.id, Version: m.readVersionLocked(),
		Durable: m.persist && m.flushed && !store.poisoned,
		Closed:  store.closed, Uncertain: store.poisoned,
	}
	if store.leaf >= 0 {
		change.LeafID = store.entries[store.leaf].id
	}
	return change
}

// Every public mutation defers this before unlocking, including error paths:
// a failed delayed flush or uncertain write can still invalidate a read.
func (m *SessionManager) notifyReadChangeLocked() {
	if len(m.readWatchers) == 0 {
		return
	}
	change := m.readChangeLocked()
	if change == m.lastReadChange {
		return
	}
	m.lastReadChange = change
	for id, channel := range m.readWatchers {
		select {
		case <-channel:
		default:
		}
		channel <- change
		if change.Closed {
			close(channel)
			delete(m.readWatchers, id)
		}
	}
}

// AppendReceipt identifies the entry accepted by the manager even when the
// first-assistant snapshot subsequently fails. Accepted does not imply durable.
// On an uncertain durable append Accepted is false and Uncertain is true: no
// entry identity is invented before storage has reconciled the outcome.
type AppendReceipt struct {
	Entry     Entry
	Version   ReadVersion
	Accepted  bool
	Durable   bool
	Uncertain bool
}

func (m *SessionManager) AppendMessageWithReceipt(ctx context.Context, message agentmsg.Message) (AppendReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	defer m.notifyReadChangeLocked()
	entry, err := m.appendAndFlushReceipt(ctx, func(s *Session) (Entry, error) {
		return s.AppendAgentMessage(ctx, message, AppendOptions{})
	})
	change := m.readChangeLocked()
	return AppendReceipt{
		Entry: entry, Version: change.Version, Accepted: entry.ID() != "",
		Durable: entry.ID() != "" && change.Durable, Uncertain: change.Uncertain,
	}, err
}
