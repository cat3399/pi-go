package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"strconv"
	"sync"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/provider"
)

// ReadVersion validates Agent-owned resources. Generation changes when the
// Agent is replaced; configuration and queue revisions are independent of
// token updates. These identities are not persisted in conversation messages.
type ReadVersion struct {
	Generation    string
	State         uint64
	Configuration uint64
	Queue         uint64
}

type ReadMessage struct {
	ID      string
	Message agentmsg.Message
}

type ReadToolProgress struct {
	ToolCallID string
	ToolName   string
	Arguments  json.RawMessage
	Update     *ToolUpdate
}

// ReadSnapshot projects current state without copying the settled history.
// Pending messages reference finalized Agent messages that its owning host
// has not acknowledged yet. AgentSession acknowledges the manager's actual
// append receipt; a consumer never guesses the association from message text.
type ReadSnapshot struct {
	Version       ReadVersion
	Phase         Phase
	RunID         uint64
	Turn          uint32
	IsStreaming   bool
	MessageCount  int
	Streaming     *ReadMessage
	Pending       []ReadMessage
	ToolsRunning  []ReadToolProgress
	ErrorMessage  string
	Model         provider.Model
	HasModel      bool
	ThinkingLevel provider.ThinkingLevel
	SystemPrompt  string
	Tools         []provider.ToolDefinition
	Steering      []agentmsg.Message
	FollowUp      []agentmsg.Message
	SteeringMode  QueueMode
	FollowUpMode  QueueMode
	Queue         QueueState
}

// Observation adds read metadata to the canonical event without changing the
// event union, listener ordering or the AgentLoop/provider contract.
type Observation struct {
	Event     AgentEvent
	Version   ReadVersion
	MessageID string
}

type pendingReadMessage struct {
	id    string
	index int
}

type agentReadState struct {
	version     ReadVersion
	nextMessage uint64
	messageID   string
	pending     []pendingReadMessage
	nextWatcher uint64
	watchers    map[uint64]chan ReadVersion
}

func (a *Agent) readVersionLocked() ReadVersion {
	if a.read.version.Generation == "" {
		a.read.version.Generation = rand.Text()
	}
	return a.read.version
}

func (a *Agent) touchReadLocked(configuration, queue bool) {
	a.read.version.State++
	if configuration {
		a.read.version.Configuration++
	}
	if queue {
		a.read.version.Queue++
	}
	if configuration || queue {
		for _, watcher := range a.read.watchers {
			select {
			case <-watcher:
			default:
			}
			watcher <- a.readVersionLocked()
		}
	}
}

// SubscribeConfigurationChanges invalidates config/queue reads even when a
// mutator has no canonical event. Token updates stay on the event stream.
func (a *Agent) SubscribeConfigurationChanges() (ReadVersion, <-chan ReadVersion, func()) {
	a.mu.Lock()
	if a.read.watchers == nil {
		a.read.watchers = make(map[uint64]chan ReadVersion)
	}
	a.read.nextWatcher++
	id := a.read.nextWatcher
	watcher := make(chan ReadVersion, 1)
	a.read.watchers[id] = watcher
	initial := a.readVersionLocked()
	a.mu.Unlock()
	var once sync.Once
	return initial, watcher, func() {
		once.Do(func() {
			a.mu.Lock()
			delete(a.read.watchers, id)
			close(watcher)
			a.mu.Unlock()
		})
	}
}

func (a *Agent) newReadMessageLocked() string {
	version := a.readVersionLocked()
	a.read.nextMessage++
	return version.Generation + "/" + strconv.FormatUint(a.read.nextMessage, 10)
}

func (a *Agent) ensureReadMessageLocked() string {
	if a.read.messageID == "" {
		a.read.messageID = a.newReadMessageLocked()
	}
	return a.read.messageID
}

func (a *Agent) appendPendingReadMessageLocked() {
	a.read.pending = append(a.read.pending, pendingReadMessage{id: a.ensureReadMessageLocked(), index: len(a.messages) - 1})
}

func (a *Agent) forgetPendingReadMessageLocked(id string) {
	for index, pending := range a.read.pending {
		if pending.id == id {
			copy(a.read.pending[index:], a.read.pending[index+1:])
			a.read.pending[len(a.read.pending)-1] = pendingReadMessage{}
			a.read.pending = a.read.pending[:len(a.read.pending)-1]
			return
		}
	}
}

func (a *Agent) acknowledgeReadMessage(id string) ReadVersion {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.forgetPendingReadMessageLocked(id)
	a.touchReadLocked(false, false)
	return a.readVersionLocked()
}

func (a *Agent) ReadVersion() ReadVersion {
	if a == nil {
		return ReadVersion{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.readVersionLocked()
}

func (a *Agent) ReadSnapshot() ReadSnapshot {
	if a == nil {
		return ReadSnapshot{Phase: PhaseIdle}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	steering, followUp := a.richQueueMessagesLocked()
	llmSteering, _ := agentmsg.ConvertToLLM(steering)
	llmFollowUp, _ := agentmsg.ConvertToLLM(followUp)
	result := ReadSnapshot{
		Version: a.readVersionLocked(), Phase: a.phaseLocked(), IsStreaming: a.isStreaming,
		MessageCount: len(a.messages), ErrorMessage: a.errorMessage,
		Model: a.model, HasModel: a.hasModel, ThinkingLevel: a.thinkingLevel,
		SystemPrompt: a.systemPrompt, Tools: append([]provider.ToolDefinition(nil), a.tools...),
		Steering: agentmsg.Clone(steering), FollowUp: agentmsg.Clone(followUp),
		SteeringMode: a.steeringMode, FollowUpMode: a.followUpMode,
		Queue: newQueueState(llmSteering, llmFollowUp),
	}
	if a.streaming != nil {
		result.Streaming = &ReadMessage{ID: a.read.messageID, Message: agentmsg.CloneOne(a.streaming)}
	}
	for _, pending := range a.read.pending {
		result.Pending = append(result.Pending, ReadMessage{ID: pending.id, Message: agentmsg.CloneOne(a.messages[pending.index])})
	}
	if active := a.active; active != nil {
		result.RunID, result.Turn = active.id, active.turn
		for _, id := range active.pendingToolCalls {
			progress := active.toolProgress[id]
			progress.Arguments = bytes.Clone(progress.Arguments)
			if progress.Update != nil {
				update := cloneToolUpdate(*progress.Update)
				progress.Update = &update
			}
			result.ToolsRunning = append(result.ToolsRunning, progress)
		}
	}
	return result
}

// SubscribeObserved shares the canonical listener list. Legacy Subscribe
// simply drops the metadata, preserving awaited errors and registration order.
func (a *Agent) SubscribeObserved(listener func(context.Context, Observation) error) func() {
	return a.subscribeObserved(listener)
}
