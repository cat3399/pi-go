package application

import (
	"context"
	"fmt"
	"sync"

	"github.com/cat3399/pi-go/internal/agent"
	agentruntime "github.com/cat3399/pi-go/internal/runtime"
)

const (
	eventIngressCapacity = 256
	observerCapacity     = 64
)

type queuedEvent struct {
	ctx       context.Context
	sessionID string
	value     EventValue
	barrier   chan struct{}
	observers []*applicationSessionObserver
}

type observedEvent struct {
	ctx   context.Context
	event Event
}

type applicationSessionObserver struct {
	callback SessionObserver
	inbox    chan observedEvent
	stop     chan struct{}
	done     chan struct{}
	once     sync.Once
}

type ApplicationSession struct {
	runtime *agentruntime.Runtime
	ctx     context.Context
	cancel  context.CancelFunc

	lifecycleMu sync.Mutex
	closing     bool
	closed      bool
	operations  sync.WaitGroup
	promptCount uint64
	nextOpID    uint64

	bindMu      sync.RWMutex
	session     *agent.AgentSession
	binding     uint64
	unsubscribe func()

	eventIngress chan queuedEvent
	eventStop    chan struct{}
	eventDone    chan struct{}

	observerMu     sync.RWMutex
	observers      map[uint64]*applicationSessionObserver
	nextObserverID uint64
}

func NewApplicationSession(ctx context.Context, runtime *agentruntime.Runtime) (*ApplicationSession, error) {
	if runtime == nil || runtime.Session() == nil || runtime.Services() == nil {
		return nil, fmt.Errorf("%w: runtime is required", ErrSessionUnavailable)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sessionCtx, cancel := context.WithCancel(ctx)
	s := &ApplicationSession{
		runtime: runtime, ctx: sessionCtx, cancel: cancel,
		eventIngress: make(chan queuedEvent, eventIngressCapacity),
		eventStop:    make(chan struct{}), eventDone: make(chan struct{}),
		observers: make(map[uint64]*applicationSessionObserver),
	}
	go s.dispatchEvents()
	if err := s.bind(runtime.Session()); err != nil {
		cancel()
		close(s.eventStop)
		<-s.eventDone
		return nil, err
	}
	runtime.SetBeforeSessionInvalidate(s.unbind)
	runtime.SetRebindSession(func(_ context.Context, replacement *agent.AgentSession) error {
		return s.bind(replacement)
	})
	return s, nil
}

func (s *ApplicationSession) Runtime() *agentruntime.Runtime {
	if s == nil {
		return nil
	}
	return s.runtime
}

func (s *ApplicationSession) bind(next *agent.AgentSession) error {
	if s == nil || next == nil {
		return fmt.Errorf("%w: nil AgentSession", ErrSessionUnavailable)
	}
	s.lifecycleMu.Lock()
	closed := s.closed || s.closing
	s.lifecycleMu.Unlock()
	if closed {
		return ErrClosed
	}

	s.bindMu.Lock()
	if s.unsubscribe != nil {
		s.unsubscribe()
		s.unsubscribe = nil
	}
	s.binding++
	generation := s.binding
	s.session = next
	sessionID := next.SessionManager().SessionID()
	s.unsubscribe = next.Subscribe(func(ctx context.Context, event agent.SessionEvent) {
		s.bindMu.RLock()
		current := s.binding == generation && s.session == next
		s.bindMu.RUnlock()
		if !current {
			return
		}
		s.enqueue(ctx, sessionID, AgentSessionEvent{Event: event})
	})
	s.bindMu.Unlock()
	return nil
}

func (s *ApplicationSession) unbind() {
	if s == nil {
		return
	}
	s.bindMu.Lock()
	if s.unsubscribe != nil {
		s.unsubscribe()
		s.unsubscribe = nil
	}
	s.binding++
	s.session = nil
	s.bindMu.Unlock()
}

func (s *ApplicationSession) currentSession() (*agent.AgentSession, uint64, error) {
	if s == nil {
		return nil, 0, ErrClosed
	}
	s.lifecycleMu.Lock()
	closed := s.closed || s.closing
	s.lifecycleMu.Unlock()
	if closed {
		return nil, 0, ErrClosed
	}
	s.bindMu.RLock()
	current, generation := s.session, s.binding
	s.bindMu.RUnlock()
	if current == nil {
		return nil, generation, ErrSessionUnavailable
	}
	return current, generation, nil
}

func (s *ApplicationSession) sameBinding(session *agent.AgentSession, generation uint64) bool {
	s.bindMu.RLock()
	same := s.session == session && s.binding == generation
	s.bindMu.RUnlock()
	return same
}

func cloneEventValue(value EventValue) EventValue {
	switch value := value.(type) {
	case AgentSessionEvent:
		return AgentSessionEvent{Event: agent.CloneSessionEvent(value.Event)}
	case SessionCatalogEvent:
		return value
	case ProjectCatalogEvent:
		return value
	case OperationEvent:
		return value
	default:
		return nil
	}
}

func cloneEvent(event Event) Event {
	event.Value = cloneEventValue(event.Value)
	return event
}

func (s *ApplicationSession) enqueue(ctx context.Context, sessionID string, value EventValue) {
	if s == nil || value == nil {
		return
	}
	value = cloneEventValue(value)
	if value == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.lifecycleMu.Lock()
	closed := s.closed
	s.lifecycleMu.Unlock()
	if closed {
		return
	}
	s.observerMu.RLock()
	observers := make([]*applicationSessionObserver, 0, len(s.observers))
	for _, observer := range s.observers {
		observers = append(observers, observer)
	}
	s.observerMu.RUnlock()
	select {
	case s.eventIngress <- queuedEvent{ctx: ctx, sessionID: sessionID, value: value, observers: observers}:
	case <-s.ctx.Done():
	}
}

func (s *ApplicationSession) dispatchEvents() {
	defer close(s.eventDone)
	var sequence uint64
	for {
		select {
		case item := <-s.eventIngress:
			if item.barrier != nil {
				close(item.barrier)
				continue
			}
			sequence++
			event := Event{Sequence: sequence, SessionID: item.sessionID, Value: item.value}
			for _, observer := range item.observers {
				select {
				case observer.inbox <- observedEvent{ctx: item.ctx, event: cloneEvent(event)}:
				case <-observer.stop:
				case <-s.eventStop:
					return
				}
			}
		case <-s.eventStop:
			return
		}
	}
}

func runApplicationSessionObserver(ctx context.Context, observer *applicationSessionObserver) {
	defer close(observer.done)
	for {
		select {
		case <-observer.stop:
			return
		default:
		}
		select {
		case observed, ok := <-observer.inbox:
			if !ok {
				return
			}
			func() {
				defer func() { _ = recover() }()
				callbackContext := observed.ctx
				if callbackContext == nil {
					callbackContext = ctx
				}
				observer.callback(callbackContext, observed.event)
			}()
		case <-observer.stop:
			return
		}
	}
}

// Subscribe registers an ordered observer. Each observer owns a small delivery
// queue so it may synchronously issue another ApplicationSession command without deadlocking
// the single event sequencer. Slow observers eventually apply backpressure;
// events are never silently dropped.
func (s *ApplicationSession) Subscribe(observer SessionObserver) func() {
	if s == nil || observer == nil {
		return func() {}
	}
	s.lifecycleMu.Lock()
	if s.closed || s.closing {
		s.lifecycleMu.Unlock()
		return func() {}
	}
	registered := &applicationSessionObserver{
		callback: observer, inbox: make(chan observedEvent, observerCapacity),
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	s.observerMu.Lock()
	s.nextObserverID++
	id := s.nextObserverID
	s.observers[id] = registered
	s.observerMu.Unlock()
	s.lifecycleMu.Unlock()
	go runApplicationSessionObserver(s.ctx, registered)
	return func() {
		registered.once.Do(func() {
			s.observerMu.Lock()
			delete(s.observers, id)
			s.observerMu.Unlock()
			close(registered.stop)
		})
	}
}

func (s *ApplicationSession) stopObservers() {
	s.observerMu.Lock()
	observers := make([]*applicationSessionObserver, 0, len(s.observers))
	for id, observer := range s.observers {
		delete(s.observers, id)
		observers = append(observers, observer)
	}
	s.observerMu.Unlock()
	for _, observer := range observers {
		// The event dispatcher has already stopped, so closing inbox is safe and
		// lets each observer drain every event accepted before the barrier.
		close(observer.inbox)
	}
	for _, observer := range observers {
		<-observer.done
	}
}

func (s *ApplicationSession) eventBarrier(ctx context.Context) error {
	done := make(chan struct{})
	select {
	case s.eventIngress <- queuedEvent{barrier: done}:
	case <-ctx.Done():
		return context.Cause(ctx)
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// Dispose owns the same terminal lifecycle as Runtime.Dispose. A successful
// return means session work has settled, every previously enqueued ApplicationSession event
// reached observer queues, and no later command can start.
func (s *ApplicationSession) Dispose(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.lifecycleMu.Lock()
	if s.closed {
		s.lifecycleMu.Unlock()
		return nil
	}
	if s.closing {
		s.lifecycleMu.Unlock()
		return ErrClosed
	}
	s.closing = true
	s.lifecycleMu.Unlock()

	if err := s.runtime.Dispose(ctx); err != nil {
		s.lifecycleMu.Lock()
		s.closing = false
		s.lifecycleMu.Unlock()
		return err
	}
	s.operations.Wait()
	if err := s.eventBarrier(ctx); err != nil {
		return err
	}
	s.lifecycleMu.Lock()
	s.closed = true
	s.closing = false
	s.lifecycleMu.Unlock()
	s.cancel()
	close(s.eventStop)
	<-s.eventDone
	s.stopObservers()
	return nil
}
