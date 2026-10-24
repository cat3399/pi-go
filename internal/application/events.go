package application

import (
	"crypto/rand"
	"errors"
	"sort"
	"sync"

	"github.com/cat3399/pi-go/internal/agent"
)

const (
	defaultEventHistoryCapacity    = 4096
	defaultEventSubscriberCapacity = 512
	defaultEventHistoryBytes       = 32 << 20
)

var ErrEventCursorUnavailable = errors.New("application event cursor is unavailable")

// EventCursor identifies a position in one process stream. A sequence from a
// previous process can never validate against a restarted stream.
type EventCursor struct {
	Epoch    string
	Sequence uint64
}

type ReadEventOptions struct {
	After    *EventCursor // nil subscribes from the current position
	Sessions *[]string    // nil keeps all details; an empty list selects globals only
}

// EventSubscription is a cursor-based view of the process-wide application
// event stream. Consumers must take a fresh snapshot when SubscribeEvents
// returns ErrEventCursorUnavailable.
type EventSubscription struct {
	// Replay contains the retained events immediately after the requested
	// cursor. Consumers apply it before reading Events.
	Replay   []Event
	Events   <-chan Event
	Revision uint64
	Cursor   EventCursor

	stream *eventStream
	id     uint64
	once   sync.Once
}

func (s *EventSubscription) Close() {
	if s == nil || s.stream == nil {
		return
	}
	s.once.Do(func() { s.stream.unsubscribe(s.id) })
}

type eventStream struct {
	mu            sync.Mutex
	epoch         string
	revision      uint64
	history       []Event
	historySize   int
	historyBytes  []int
	retainedBytes int
	byteBudget    int
	replayFloor   uint64
	subscribers   map[uint64]*eventSubscriber
	nextID        uint64
	closed        bool
}

type eventSubscriber struct {
	inbox    chan Event
	sessions map[string]bool
}

func (s *eventSubscriber) accepts(event Event) bool {
	if s.sessions == nil || s.sessions[event.SessionID] {
		return true
	}
	if value, canonical := event.Value.(AgentSessionEvent); canonical {
		// Large messages, tool progress, compaction summaries and agent_end's
		// message list are details. Owner-provided live/resource notifications
		// supply global activity without synthesizing canonical Agent events.
		switch value.Event.(type) {
		case agent.AgentStartEvent, agent.AgentSettledEvent, agent.SessionInfoChangeEvent, agent.ThinkingLevelChangedEvent:
			return true
		default:
			return false
		}
	}
	return true
}

func newEventStream(historySize int) *eventStream {
	if historySize <= 0 {
		historySize = defaultEventHistoryCapacity
	}
	return &eventStream{
		epoch: rand.Text(), historySize: historySize, byteBudget: defaultEventHistoryBytes,
		subscribers: make(map[uint64]*eventSubscriber),
	}
}

func (s *eventStream) currentCursor() EventCursor {
	if s == nil {
		return EventCursor{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return EventCursor{Epoch: s.epoch, Sequence: s.revision}
}

func (s *eventStream) currentRevision() uint64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision
}

func (s *eventStream) publish(event Event) {
	if s == nil || event.Value == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}

	s.revision++
	event.Sequence = s.revision
	event.Epoch = s.epoch
	event = cloneEvent(event)
	size := estimatedEventBytes(event)
	s.history = append(s.history, event)
	s.historyBytes = append(s.historyBytes, size)
	s.retainedBytes += size
	overflow := 0
	for len(s.history)-overflow > s.historySize || s.retainedBytes > s.byteBudget {
		s.retainedBytes -= s.historyBytes[overflow]
		s.replayFloor = s.history[overflow].Sequence
		overflow++
	}
	if overflow > 0 {
		remaining := copy(s.history, s.history[overflow:])
		clear(s.history[remaining:])
		s.history = s.history[:remaining]
		copy(s.historyBytes, s.historyBytes[overflow:])
		s.historyBytes = s.historyBytes[:remaining]
	}

	for id, subscriber := range s.subscribers {
		if !subscriber.accepts(event) {
			continue
		}
		select {
		case subscriber.inbox <- cloneEvent(event):
		default:
			// A slow transport reconnects with its last delivered cursor. Closing
			// it here keeps Agent execution independent from network backpressure.
			close(subscriber.inbox)
			delete(s.subscribers, id)
		}
	}
}

func (s *eventStream) subscribe(after uint64) (*EventSubscription, error) {
	return s.subscribeRead(ReadEventOptions{After: &EventCursor{Sequence: after}}, true)
}

func (s *eventStream) subscribeRead(options ReadEventOptions, legacy bool) (*EventSubscription, error) {
	if s == nil {
		return nil, errors.New("application event stream is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("application service is closed")
	}
	after := s.revision
	if options.After != nil {
		if !legacy && options.After.Epoch != s.epoch {
			return nil, ErrEventCursorUnavailable
		}
		after = options.After.Sequence
	}
	if after > s.revision || after < s.replayFloor {
		return nil, ErrEventCursorUnavailable
	}
	subscriber := &eventSubscriber{inbox: make(chan Event, defaultEventSubscriberCapacity)}
	if options.Sessions != nil {
		subscriber.sessions = make(map[string]bool, len(*options.Sessions))
		for _, id := range *options.Sessions {
			subscriber.sessions[id] = true
		}
	}

	start := sort.Search(len(s.history), func(index int) bool { return s.history[index].Sequence > after })
	retained := s.history[start:]
	count := len(retained)
	if subscriber.sessions != nil {
		count = 0
		for _, event := range retained {
			if subscriber.accepts(event) {
				count++
			}
		}
	}
	replay := make([]Event, 0, count)
	for _, event := range retained {
		if subscriber.accepts(event) {
			replay = append(replay, cloneEvent(event))
		}
	}
	s.nextID++
	id := s.nextID
	s.subscribers[id] = subscriber
	return &EventSubscription{Replay: replay, Events: subscriber.inbox, Revision: s.revision, Cursor: EventCursor{Epoch: s.epoch, Sequence: s.revision}, stream: s, id: id}, nil
}

func (s *eventStream) unsubscribe(id uint64) {
	if s == nil || id == 0 {
		return
	}
	s.mu.Lock()
	if subscriber := s.subscribers[id]; subscriber != nil {
		delete(s.subscribers, id)
		close(subscriber.inbox)
	}
	s.mu.Unlock()
}

func (s *eventStream) close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		for id, subscriber := range s.subscribers {
			delete(s.subscribers, id)
			close(subscriber.inbox)
		}
	}
	s.mu.Unlock()
}
