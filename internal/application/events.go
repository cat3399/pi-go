package application

import (
	"errors"
	"sync"
)

const (
	defaultEventHistoryCapacity    = 4096
	defaultEventSubscriberCapacity = 512
)

var ErrEventCursorUnavailable = errors.New("application event cursor is unavailable")

// EventSubscription is a cursor-based view of the process-wide application
// event stream. Consumers must take a fresh snapshot when SubscribeEvents
// returns ErrEventCursorUnavailable.
type EventSubscription struct {
	// Replay contains the retained events immediately after the requested
	// cursor. Consumers apply it before reading Events.
	Replay   []Event
	Events   <-chan Event
	Revision uint64

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
	mu          sync.Mutex
	revision    uint64
	history     []Event
	historySize int
	subscribers map[uint64]chan Event
	nextID      uint64
	closed      bool
}

func newEventStream(historySize int) *eventStream {
	if historySize <= 0 {
		historySize = defaultEventHistoryCapacity
	}
	return &eventStream{
		historySize: historySize,
		subscribers: make(map[uint64]chan Event),
	}
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
	event = cloneEvent(event)
	s.history = append(s.history, event)
	if overflow := len(s.history) - s.historySize; overflow > 0 {
		copy(s.history, s.history[overflow:])
		s.history = s.history[:s.historySize]
	}

	for id, subscriber := range s.subscribers {
		select {
		case subscriber <- cloneEvent(event):
		default:
			// A slow transport reconnects with its last delivered cursor. Closing
			// it here keeps Agent execution independent from network backpressure.
			close(subscriber)
			delete(s.subscribers, id)
		}
	}
}

func (s *eventStream) subscribe(after uint64) (*EventSubscription, error) {
	if s == nil {
		return nil, errors.New("application event stream is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("application service is closed")
	}
	if after > s.revision {
		return nil, ErrEventCursorUnavailable
	}
	if len(s.history) != 0 {
		oldest := s.history[0].Sequence
		if oldest > 0 && after < oldest-1 {
			return nil, ErrEventCursorUnavailable
		}
	}

	replay := make([]Event, 0, len(s.history))
	for _, event := range s.history {
		if event.Sequence > after {
			replay = append(replay, cloneEvent(event))
		}
	}
	inbox := make(chan Event, defaultEventSubscriberCapacity)
	s.nextID++
	id := s.nextID
	s.subscribers[id] = inbox
	return &EventSubscription{Replay: replay, Events: inbox, Revision: s.revision, stream: s, id: id}, nil
}

func (s *eventStream) unsubscribe(id uint64) {
	if s == nil || id == 0 {
		return
	}
	s.mu.Lock()
	if subscriber := s.subscribers[id]; subscriber != nil {
		delete(s.subscribers, id)
		close(subscriber)
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
			close(subscriber)
		}
	}
	s.mu.Unlock()
}
