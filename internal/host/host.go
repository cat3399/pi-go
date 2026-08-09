package host

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
	observers []*hostObserver
}

type hostObserver struct {
	callback Observer
	inbox    chan Event
	stop     chan struct{}
	done     chan struct{}
	once     sync.Once
}

type Host struct {
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
	observers      map[uint64]*hostObserver
	nextObserverID uint64
}

func New(ctx context.Context, runtime *agentruntime.Runtime) (*Host, error) {
	if runtime == nil || runtime.Session() == nil || runtime.Services() == nil {
		return nil, fmt.Errorf("%w: runtime is required", ErrSessionUnavailable)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	hostCtx, cancel := context.WithCancel(ctx)
	h := &Host{
		runtime: runtime, ctx: hostCtx, cancel: cancel,
		eventIngress: make(chan queuedEvent, eventIngressCapacity),
		eventStop:    make(chan struct{}), eventDone: make(chan struct{}),
		observers: make(map[uint64]*hostObserver),
	}
	go h.dispatchEvents()
	if err := h.bind(runtime.Session()); err != nil {
		cancel()
		close(h.eventStop)
		<-h.eventDone
		return nil, err
	}
	runtime.SetBeforeSessionInvalidate(h.unbind)
	runtime.SetRebindSession(func(_ context.Context, replacement *agent.AgentSession) error {
		return h.bind(replacement)
	})
	return h, nil
}

func (h *Host) Runtime() *agentruntime.Runtime {
	if h == nil {
		return nil
	}
	return h.runtime
}

func (h *Host) bind(next *agent.AgentSession) error {
	if h == nil || next == nil {
		return fmt.Errorf("%w: nil AgentSession", ErrSessionUnavailable)
	}
	h.lifecycleMu.Lock()
	closed := h.closed || h.closing
	h.lifecycleMu.Unlock()
	if closed {
		return ErrClosed
	}

	h.bindMu.Lock()
	if h.unsubscribe != nil {
		h.unsubscribe()
		h.unsubscribe = nil
	}
	h.binding++
	generation := h.binding
	h.session = next
	sessionID := next.SessionManager().SessionID()
	h.unsubscribe = next.Subscribe(func(ctx context.Context, event agent.SessionEvent) {
		h.bindMu.RLock()
		current := h.binding == generation && h.session == next
		h.bindMu.RUnlock()
		if !current {
			return
		}
		h.enqueue(ctx, sessionID, AgentSessionEvent{Event: event})
	})
	h.bindMu.Unlock()
	return nil
}

func (h *Host) unbind() {
	if h == nil {
		return
	}
	h.bindMu.Lock()
	if h.unsubscribe != nil {
		h.unsubscribe()
		h.unsubscribe = nil
	}
	h.binding++
	h.session = nil
	h.bindMu.Unlock()
}

func (h *Host) currentSession() (*agent.AgentSession, uint64, error) {
	if h == nil {
		return nil, 0, ErrClosed
	}
	h.lifecycleMu.Lock()
	closed := h.closed || h.closing
	h.lifecycleMu.Unlock()
	if closed {
		return nil, 0, ErrClosed
	}
	h.bindMu.RLock()
	current, generation := h.session, h.binding
	h.bindMu.RUnlock()
	if current == nil {
		return nil, generation, ErrSessionUnavailable
	}
	return current, generation, nil
}

func (h *Host) sameBinding(session *agent.AgentSession, generation uint64) bool {
	h.bindMu.RLock()
	same := h.session == session && h.binding == generation
	h.bindMu.RUnlock()
	return same
}

func cloneEventValue(value EventValue) EventValue {
	switch value := value.(type) {
	case AgentSessionEvent:
		return AgentSessionEvent{Event: agent.CloneSessionEvent(value.Event)}
	case PromptErrorEvent, PromptDoneEvent:
		return value
	default:
		return nil
	}
}

func cloneEvent(event Event) Event {
	event.Value = cloneEventValue(event.Value)
	return event
}

func (h *Host) enqueue(ctx context.Context, sessionID string, value EventValue) {
	if h == nil || value == nil {
		return
	}
	value = cloneEventValue(value)
	if value == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.lifecycleMu.Lock()
	closed := h.closed
	h.lifecycleMu.Unlock()
	if closed {
		return
	}
	h.observerMu.RLock()
	observers := make([]*hostObserver, 0, len(h.observers))
	for _, observer := range h.observers {
		observers = append(observers, observer)
	}
	h.observerMu.RUnlock()
	select {
	case h.eventIngress <- queuedEvent{ctx: ctx, sessionID: sessionID, value: value, observers: observers}:
	case <-h.ctx.Done():
	}
}

func (h *Host) dispatchEvents() {
	defer close(h.eventDone)
	var sequence uint64
	for {
		select {
		case item := <-h.eventIngress:
			if item.barrier != nil {
				close(item.barrier)
				continue
			}
			sequence++
			event := Event{Sequence: sequence, SessionID: item.sessionID, Value: item.value}
			for _, observer := range item.observers {
				select {
				case observer.inbox <- cloneEvent(event):
				case <-observer.stop:
				case <-h.eventStop:
					return
				}
			}
		case <-h.eventStop:
			return
		}
	}
}

func runObserver(ctx context.Context, observer *hostObserver) {
	defer close(observer.done)
	for {
		select {
		case <-observer.stop:
			return
		default:
		}
		select {
		case event, ok := <-observer.inbox:
			if !ok {
				return
			}
			func() {
				defer func() { _ = recover() }()
				observer.callback(ctx, event)
			}()
		case <-observer.stop:
			return
		}
	}
}

// Subscribe registers an ordered observer. Each observer owns a small delivery
// queue so it may synchronously issue another Host command without deadlocking
// the single event sequencer. Slow observers eventually apply backpressure;
// events are never silently dropped.
func (h *Host) Subscribe(observer Observer) func() {
	if h == nil || observer == nil {
		return func() {}
	}
	h.lifecycleMu.Lock()
	if h.closed || h.closing {
		h.lifecycleMu.Unlock()
		return func() {}
	}
	registered := &hostObserver{
		callback: observer, inbox: make(chan Event, observerCapacity),
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	h.observerMu.Lock()
	h.nextObserverID++
	id := h.nextObserverID
	h.observers[id] = registered
	h.observerMu.Unlock()
	h.lifecycleMu.Unlock()
	go runObserver(h.ctx, registered)
	return func() {
		registered.once.Do(func() {
			h.observerMu.Lock()
			delete(h.observers, id)
			h.observerMu.Unlock()
			close(registered.stop)
		})
	}
}

func (h *Host) stopObservers() {
	h.observerMu.Lock()
	observers := make([]*hostObserver, 0, len(h.observers))
	for id, observer := range h.observers {
		delete(h.observers, id)
		observers = append(observers, observer)
	}
	h.observerMu.Unlock()
	for _, observer := range observers {
		// The event dispatcher has already stopped, so closing inbox is safe and
		// lets each observer drain every event accepted before the barrier.
		close(observer.inbox)
	}
	for _, observer := range observers {
		<-observer.done
	}
}

func (h *Host) eventBarrier(ctx context.Context) error {
	done := make(chan struct{})
	select {
	case h.eventIngress <- queuedEvent{barrier: done}:
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
// return means session work has settled, every previously enqueued Host event
// reached observer queues, and no later command can start.
func (h *Host) Dispose(ctx context.Context) error {
	if h == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.lifecycleMu.Lock()
	if h.closed {
		h.lifecycleMu.Unlock()
		return nil
	}
	if h.closing {
		h.lifecycleMu.Unlock()
		return ErrClosed
	}
	h.closing = true
	h.lifecycleMu.Unlock()

	if err := h.runtime.Dispose(ctx); err != nil {
		h.lifecycleMu.Lock()
		h.closing = false
		h.lifecycleMu.Unlock()
		return err
	}
	h.operations.Wait()
	if err := h.eventBarrier(ctx); err != nil {
		return err
	}
	h.lifecycleMu.Lock()
	h.closed = true
	h.closing = false
	h.lifecycleMu.Unlock()
	h.cancel()
	close(h.eventStop)
	<-h.eventDone
	h.stopObservers()
	return nil
}
