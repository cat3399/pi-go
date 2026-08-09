package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
)

// PromptOptions mirrors coding-agent's prompt preflight controls. A nil
// ExpandPromptTemplates uses the upstream default (true).
type PromptOptions struct {
	ExpandPromptTemplates *bool
	Images                []llm.ImageBlock
	StreamingBehavior     StreamingBehavior
	Source                InputSource
}

type UserMessageOptions struct {
	DeliverAs StreamingBehavior
}

type CustomMessageInput struct {
	CustomType    string
	Content       []llm.UserContentBlock
	StringContent *string
	Display       bool
	Details       json.RawMessage
}

type CustomMessageDelivery string

const (
	DeliverCustomSteer    CustomMessageDelivery = "steer"
	DeliverCustomFollowUp CustomMessageDelivery = "followUp"
	DeliverCustomNextTurn CustomMessageDelivery = "nextTurn"
)

func (d CustomMessageDelivery) Valid() bool {
	return d == "" || d == DeliverCustomSteer || d == DeliverCustomFollowUp || d == DeliverCustomNextTurn
}

type CustomMessageOptions struct {
	TriggerTurn bool
	DeliverAs   CustomMessageDelivery
}

type promptPreflight struct {
	text        string
	images      []llm.ImageBlock
	transformed bool
	handled     bool
}

type resolvedExtensionCommand struct {
	ExtensionCommand
	invocationName string
	index          int
}

type promptBusyError struct{}

func (promptBusyError) Error() string {
	return "Agent is already processing. Specify streamingBehavior ('steer' or 'followUp') to queue the message."
}
func (promptBusyError) Unwrap() error { return ErrBusy }

func handledPromptResult() Result { return Result{handled: true} }

func normalizePromptOptions(options PromptOptions) (PromptOptions, bool, error) {
	if options.Source == "" {
		options.Source = InputInteractive
	}
	if !options.Source.Valid() {
		return PromptOptions{}, false, fmt.Errorf("%w: invalid input source %q", ErrInvalidRun, options.Source)
	}
	if !options.StreamingBehavior.Valid() {
		return PromptOptions{}, false, fmt.Errorf("%w: invalid streaming behavior %q", ErrInvalidRun, options.StreamingBehavior)
	}
	expand := true
	if options.ExpandPromptTemplates != nil {
		expand = *options.ExpandPromptTemplates
	}
	options.Images = append([]llm.ImageBlock(nil), options.Images...)
	return options, expand, nil
}

func validateExtensionCommands(commands []ExtensionCommand) error {
	for index, command := range commands {
		if command.Handler == nil {
			return fmt.Errorf("%w: extension command %d has no handler", ErrInvalidConfig, index)
		}
	}
	return nil
}

func resolveExtensionCommands(commands []ExtensionCommand) []resolvedExtensionCommand {
	counts := make(map[string]int, len(commands))
	for _, command := range commands {
		counts[command.Name]++
	}
	seen := make(map[string]int, len(commands))
	taken := make(map[string]struct{}, len(commands))
	resolved := make([]resolvedExtensionCommand, 0, len(commands))
	for index, command := range commands {
		seen[command.Name]++
		occurrence := seen[command.Name]
		invocation := command.Name
		if counts[command.Name] > 1 {
			invocation = fmt.Sprintf("%s:%d", command.Name, occurrence)
		}
		if _, exists := taken[invocation]; exists {
			suffix := occurrence
			for {
				suffix++
				invocation = fmt.Sprintf("%s:%d", command.Name, suffix)
				if _, collision := taken[invocation]; !collision {
					break
				}
			}
		}
		taken[invocation] = struct{}{}
		resolved = append(resolved, resolvedExtensionCommand{
			ExtensionCommand: command,
			invocationName:   invocation,
			index:            index,
		})
	}
	return resolved
}

func (s *AgentSession) extensionCommands() []resolvedExtensionCommand {
	if s == nil {
		return nil
	}
	return resolveExtensionCommands(s.hooks.Commands)
}

func commandInvocation(text string) (string, string) {
	space := strings.IndexByte(text, ' ')
	if space < 0 {
		return strings.TrimPrefix(text, "/"), ""
	}
	return strings.TrimPrefix(text[:space], "/"), text[space+1:]
}

func (s *AgentSession) commandNamed(name string) (resolvedExtensionCommand, bool) {
	for _, command := range s.extensionCommands() {
		if command.invocationName == name {
			return command, true
		}
	}
	return resolvedExtensionCommand{}, false
}

func callExtensionCommand(handler ExtensionCommandHandler, ctx context.Context, args string, session *AgentSession) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("extension command panicked: %s", safeValueText(recovered))
		}
	}()
	return handler(ctx, args, session)
}

func (s *AgentSession) tryExtensionCommand(ctx context.Context, text string) bool {
	if !strings.HasPrefix(text, "/") {
		return false
	}
	name, args := commandInvocation(text)
	command, exists := s.commandNamed(name)
	if !exists {
		return false
	}
	if err := callExtensionCommand(command.Handler, ctx, args, s); err != nil {
		s.reportExtensionError(ctx, "command", command.index, err)
	}
	return true
}

func callInputHook(hook InputHook, ctx context.Context, event InputEvent) (result InputResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("input hook panicked: %s", safeValueText(recovered))
		}
	}()
	return hook(ctx, event)
}

func (s *AgentSession) hasActiveSessionRun() bool {
	if s == nil {
		return false
	}
	s.lifecycleMu.Lock()
	active := s.run != nil
	s.lifecycleMu.Unlock()
	return active
}

func (s *AgentSession) emitInput(ctx context.Context, text string, images []llm.ImageBlock, source InputSource, behavior StreamingBehavior) promptPreflight {
	currentText := text
	currentImages := append([]llm.ImageBlock(nil), images...)
	transformed := false
	eventBehavior := StreamingBehavior("")
	if s.hasActiveSessionRun() {
		eventBehavior = behavior
	}
	for index, hook := range s.hooks.InputHandlers {
		if hook == nil {
			s.reportExtensionError(ctx, "input", index, fmt.Errorf("%w: nil input handler", ErrInvalidExtensionResult))
			continue
		}
		result, err := callInputHook(hook, ctx, InputEvent{
			Text: currentText, Images: append([]llm.ImageBlock(nil), currentImages...),
			Source: source, StreamingBehavior: eventBehavior,
		})
		if err != nil {
			s.reportExtensionError(ctx, "input", index, err)
			continue
		}
		switch result.Action {
		case "", InputContinue:
			continue
		case InputHandled:
			return promptPreflight{text: currentText, images: currentImages, transformed: transformed, handled: true}
		case InputTransform:
			currentText = result.Text
			if result.Images != nil {
				currentImages = append([]llm.ImageBlock(nil), result.Images...)
			}
			transformed = true
		default:
			s.reportExtensionError(ctx, "input", index, fmt.Errorf("%w: unknown input action %q", ErrInvalidExtensionResult, result.Action))
		}
	}
	return promptPreflight{text: currentText, images: currentImages, transformed: transformed}
}

func (s *AgentSession) preprocessPrompt(ctx context.Context, text string, images []llm.ImageBlock, options PromptOptions, expand bool) (promptPreflight, error) {
	if expand && strings.HasPrefix(text, "/") && s.tryExtensionCommand(ctx, text) {
		return promptPreflight{handled: true}, nil
	}
	processed := s.emitInput(ctx, text, images, options.Source, options.StreamingBehavior)
	if processed.handled || !expand {
		return processed, nil
	}
	expanded, err := s.expandPromptInput(processed.text)
	if err != nil {
		return promptPreflight{}, err
	}
	if expanded != processed.text {
		processed.transformed = true
		processed.text = expanded
	}
	return processed, nil
}

func userContentFromPrompt(text string, images []llm.ImageBlock) ([]llm.UserContentBlock, error) {
	block, err := llm.NewTextBlock(text)
	if err != nil {
		return nil, fmt.Errorf("%w: prompt: %w", ErrInvalidRun, err)
	}
	content := make([]llm.UserContentBlock, 0, 1+len(images))
	content = append(content, block)
	for _, image := range images {
		if isNilInterface(image) {
			return nil, fmt.Errorf("%w: prompt contains a nil image", ErrInvalidRun)
		}
		content = append(content, image)
	}
	return content, nil
}

func (s *AgentSession) prepareUserPrompt(content []llm.UserContentBlock) (sessionPromptInput, error) {
	timestamp, err := s.loop.now()
	if err != nil {
		return sessionPromptInput{}, err
	}
	user, err := llm.NewUserContentMessage(content, timestamp)
	if err != nil {
		return sessionPromptInput{}, fmt.Errorf("%w: prompt content: %w", ErrInvalidRun, err)
	}
	wrapper, err := agentmsg.NewLLM(user)
	if err != nil {
		return sessionPromptInput{}, err
	}
	messages := []agentmsg.Message{wrapper}
	prompt, images := promptTextAndImages(messages)
	return sessionPromptInput{Text: prompt, Messages: messages, Images: images}, nil
}

func (s *AgentSession) queuePromptInputIfRunning(input sessionPromptInput, behavior StreamingBehavior) (bool, error) {
	if len(input.Messages) != 1 {
		return false, fmt.Errorf("%w: prompt queue requires one user message", ErrInvalidQueueMessage)
	}
	steering := behavior == StreamingSteer
	s.lifecycleMu.Lock()
	if s.closed || s.closing {
		s.lifecycleMu.Unlock()
		return false, fmt.Errorf("%w: session is closed", ErrInvalidRun)
	}
	if s.run == nil || !s.run.acceptingQueues {
		s.lifecycleMu.Unlock()
		return false, nil
	}
	err := s.loop.enqueueAgentMessage(input.Messages[0], steering)
	s.lifecycleMu.Unlock()
	if err != nil {
		return false, err
	}
	s.emitControl(context.Background(), "queue_update")
	return true, nil
}

func (s *AgentSession) runOrQueuePrompt(
	ctx context.Context,
	behavior StreamingBehavior,
	prepare func() (sessionPromptInput, error),
) (Result, error) {
	var cached sessionPromptInput
	var cachedErr error
	prepared := false
	prepareOnce := func() (sessionPromptInput, error) {
		if !prepared {
			cached, cachedErr = prepare()
			prepared = true
		}
		return cached, cachedErr
	}
	for {
		if s.hasActiveSessionRun() {
			if behavior == "" {
				return Result{}, promptBusyError{}
			}
			input, err := prepareOnce()
			if err != nil {
				return Result{}, err
			}
			queued, err := s.queuePromptInputIfRunning(input, behavior)
			if err != nil {
				return Result{}, err
			}
			if queued {
				return handledPromptResult(), nil
			}
			if s.hasActiveSessionRun() {
				if err := s.WaitForIdle(ctx); err != nil {
					return Result{}, err
				}
			}
			continue
		}
		result, err := s.runSession(ctx, true, prepareOnce, func(run context.Context, input sessionPromptInput, extra []agentmsg.Message) (Result, error) {
			messages := append(agentmsg.Clone(input.Messages), extra...)
			return s.loop.RunAgentMessages(run, messages)
		})
		if !errors.Is(err, ErrBusy) {
			return result, err
		}
		if !s.hasActiveSessionRun() {
			return Result{}, err
		}
		if behavior == "" {
			return Result{}, promptBusyError{}
		}
	}
}

func (s *AgentSession) runTextWithOptions(ctx context.Context, prompt string, options PromptOptions) (Result, error) {
	if err := s.rejectIfClosed(); err != nil {
		return Result{}, err
	}
	if s.loop == nil {
		return Result{}, fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	if ctx == nil || context.Cause(ctx) != nil {
		return Result{}, fmt.Errorf("%w: invalid session context", ErrInvalidRun)
	}
	options, expand, err := normalizePromptOptions(options)
	if err != nil {
		return Result{}, err
	}
	processed, err := s.preprocessPrompt(ctx, prompt, options.Images, options, expand)
	if err != nil {
		return Result{}, err
	}
	if processed.handled {
		return handledPromptResult(), nil
	}
	return s.runOrQueuePrompt(ctx, options.StreamingBehavior, func() (sessionPromptInput, error) {
		content, err := userContentFromPrompt(processed.text, processed.images)
		if err != nil {
			return sessionPromptInput{}, err
		}
		return s.prepareUserPrompt(content)
	})
}

func promptContentTextAndImages(content []llm.UserContentBlock) (string, []llm.ImageBlock) {
	var text strings.Builder
	var images []llm.ImageBlock
	for _, block := range content {
		switch block := block.(type) {
		case llm.TextBlock:
			if text.Len() != 0 {
				text.WriteByte('\n')
			}
			text.WriteString(block.Text())
		case llm.ImageBlock:
			images = append(images, block)
		}
	}
	return text.String(), images
}

func (s *AgentSession) runContentWithOptions(ctx context.Context, content []llm.UserContentBlock, options PromptOptions) (Result, error) {
	if err := s.rejectIfClosed(); err != nil {
		return Result{}, err
	}
	if s.loop == nil {
		return Result{}, fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	if ctx == nil || context.Cause(ctx) != nil {
		return Result{}, fmt.Errorf("%w: invalid session context", ErrInvalidRun)
	}
	options, expand, err := normalizePromptOptions(options)
	if err != nil {
		return Result{}, err
	}
	text, images := promptContentTextAndImages(content)
	processed, err := s.preprocessPrompt(ctx, text, images, options, expand)
	if err != nil {
		return Result{}, err
	}
	if processed.handled {
		return handledPromptResult(), nil
	}
	original := append([]llm.UserContentBlock(nil), content...)
	return s.runOrQueuePrompt(ctx, options.StreamingBehavior, func() (sessionPromptInput, error) {
		// Preserve the existing Go rich-input shape when preflight did not alter
		// it. The validation remains after model-access admission, as before.
		prepared := original
		if processed.transformed {
			var err error
			prepared, err = userContentFromPrompt(processed.text, processed.images)
			if err != nil {
				return sessionPromptInput{}, err
			}
		}
		return s.prepareUserPrompt(prepared)
	})
}

// PromptWithOptions is the canonical coding-agent-style prompt entry point.
func (s *AgentSession) PromptWithOptions(ctx context.Context, prompt string, options PromptOptions) (Result, error) {
	return s.runTextWithOptions(ctx, prompt, options)
}

func (s *AgentSession) PromptContentWithOptions(ctx context.Context, content []llm.UserContentBlock, options PromptOptions) (Result, error) {
	return s.runContentWithOptions(ctx, content, options)
}

// SendUserMessage skips command/skill/template expansion but still traverses
// input hooks with source:"extension", matching pi.sendUserMessage.
func (s *AgentSession) SendUserMessage(ctx context.Context, text string, options UserMessageOptions) (Result, error) {
	expand := false
	return s.PromptWithOptions(ctx, text, PromptOptions{
		ExpandPromptTemplates: &expand,
		StreamingBehavior:     options.DeliverAs,
		Source:                InputExtension,
	})
}

func (s *AgentSession) SendUserMessageContent(ctx context.Context, content []llm.UserContentBlock, options UserMessageOptions) (Result, error) {
	expand := false
	return s.PromptContentWithOptions(ctx, content, PromptOptions{
		ExpandPromptTemplates: &expand,
		StreamingBehavior:     options.DeliverAs,
		Source:                InputExtension,
	})
}

func (s *AgentSession) rejectQueuedExtensionCommand(text string) error {
	if !strings.HasPrefix(text, "/") {
		return nil
	}
	name, _ := commandInvocation(text)
	if _, exists := s.commandNamed(name); !exists {
		return nil
	}
	return fmt.Errorf(
		"%w: Extension command \"/%s\" cannot be queued. Use PromptWithOptions or execute the command when not streaming.",
		ErrInvalidQueueMessage, name,
	)
}

func (s *AgentSession) appendPendingNextTurn(message agentmsg.Message) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.closed || s.closing {
		return fmt.Errorf("%w: session is closed", ErrInvalidRun)
	}
	s.pendingNextTurn = append(s.pendingNextTurn, agentmsg.CloneOne(message))
	return nil
}

func (s *AgentSession) reserveStandaloneMutation() (func(), error) {
	s.lifecycleMu.Lock()
	if s.closed || s.closing {
		s.lifecycleMu.Unlock()
		return nil, fmt.Errorf("%w: session is closed", ErrInvalidRun)
	}
	if s.run != nil || s.standaloneMutation {
		s.lifecycleMu.Unlock()
		return nil, ErrBusy
	}
	s.standaloneMutation = true
	if s.idleWait == nil {
		s.idleWait = make(chan struct{})
	}
	s.lifecycleMu.Unlock()
	return func() {
		s.lifecycleMu.Lock()
		s.standaloneMutation = false
		s.lifecycleMu.Unlock()
		s.resolveSessionIdle()
	}, nil
}

func (s *AgentSession) emitStandaloneMessage(ctx context.Context, message agentmsg.Message) {
	s.mu.RLock()
	observers := make([]SessionObserver, 0, len(s.observers))
	for _, entry := range s.observers {
		if entry.observer != nil {
			observers = append(observers, entry.observer)
		}
	}
	s.mu.RUnlock()
	s.emitToObservers(ctx, observers, MessageStartEvent{Message: agentmsg.CloneOne(message)})
	s.emitToObservers(ctx, observers, MessageEndEvent{Message: agentmsg.CloneOne(message)})
}

func (s *AgentSession) appendStandaloneCustom(ctx context.Context, message agentmsg.Custom) (Result, error) {
	release, err := s.reserveStandaloneMutation()
	if err != nil {
		return Result{}, err
	}
	reserved := true
	defer func() {
		if reserved {
			release()
		}
	}()
	if _, err := s.sessionManager.AppendCustomMessage(ctx, message); err != nil {
		return Result{}, fmt.Errorf("%w: custom message: %w", ErrTranscriptCommit, err)
	}
	if err := s.loop.appendSettledMessage(message); err != nil {
		// The durable append is authoritative. Rebuild state before surfacing the
		// invariant failure so transcript and future provider context cannot split.
		_ = s.reloadAgentMessagesFromSession()
		return Result{}, fmt.Errorf("%w: publish custom message: %w", ErrInvariant, err)
	}
	// Publish the visible idle state before callbacks, like agent_settled. A
	// message listener may synchronously start a prompt that already sees the
	// committed custom context, while WaitForIdle remains attached until all
	// callbacks (and any reentrant replacement run) finish.
	s.lifecycleMu.Lock()
	s.standaloneMutation = false
	s.settlingCallbacks++
	s.lifecycleMu.Unlock()
	reserved = false
	defer func() {
		s.lifecycleMu.Lock()
		if s.settlingCallbacks > 0 {
			s.settlingCallbacks--
		}
		s.lifecycleMu.Unlock()
		s.resolveSessionIdle()
	}()
	s.emitStandaloneMessage(ctx, message)
	return handledPromptResult(), nil
}

func (s *AgentSession) queueCustomIfRunning(message agentmsg.Custom, delivery CustomMessageDelivery) (bool, error) {
	steering := delivery != DeliverCustomFollowUp
	s.lifecycleMu.Lock()
	if s.closed || s.closing {
		s.lifecycleMu.Unlock()
		return false, fmt.Errorf("%w: session is closed", ErrInvalidRun)
	}
	if s.run == nil || !s.run.acceptingQueues {
		s.lifecycleMu.Unlock()
		return false, nil
	}
	err := s.loop.enqueueAgentMessage(message, steering)
	s.lifecycleMu.Unlock()
	if err != nil {
		return false, err
	}
	s.emitControl(context.Background(), "queue_update")
	return true, nil
}

func (s *AgentSession) runCustomMessage(ctx context.Context, message agentmsg.Custom) (Result, error) {
	return s.runSession(ctx, false, func() (sessionPromptInput, error) {
		return sessionPromptInput{Messages: []agentmsg.Message{message}}, nil
	}, func(run context.Context, input sessionPromptInput, _ []agentmsg.Message) (Result, error) {
		return s.loop.RunAgentMessages(run, agentmsg.Clone(input.Messages))
	})
}

// SendCustomMessage implements pi.sendMessage's four delivery paths without
// manufacturing user/assistant messages: next-turn buffering, active-run
// steering/follow-up, an immediate custom-triggered turn, or an idle durable
// context append with message_start/message_end notifications.
func (s *AgentSession) SendCustomMessage(ctx context.Context, input CustomMessageInput, options CustomMessageOptions) (Result, error) {
	if err := s.rejectIfClosed(); err != nil {
		return Result{}, err
	}
	if !options.DeliverAs.Valid() {
		return Result{}, fmt.Errorf("%w: invalid custom message delivery %q", ErrInvalidRun, options.DeliverAs)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timestamp, err := s.loop.now()
	if err != nil {
		return Result{}, err
	}
	var stringContent *string
	if input.StringContent != nil {
		value := *input.StringContent
		stringContent = &value
	}
	message, err := agentmsg.NewCustom(agentmsg.CustomSpec{
		CustomType: input.CustomType, Content: append([]llm.UserContentBlock(nil), input.Content...),
		StringContent: stringContent, Display: input.Display, Details: bytes.Clone(input.Details), At: timestamp,
	})
	if err != nil {
		return Result{}, fmt.Errorf("%w: custom message: %w", ErrInvalidRun, err)
	}
	if options.DeliverAs == DeliverCustomNextTurn {
		if err := s.appendPendingNextTurn(message); err != nil {
			return Result{}, err
		}
		return handledPromptResult(), nil
	}
	for {
		queued, err := s.queueCustomIfRunning(message, options.DeliverAs)
		if err != nil {
			return Result{}, err
		}
		if queued {
			return handledPromptResult(), nil
		}
		if s.hasActiveSessionRun() {
			if err := s.WaitForIdle(ctx); err != nil {
				return Result{}, err
			}
			continue
		}
		if options.TriggerTurn {
			result, err := s.runCustomMessage(ctx, message)
			if !errors.Is(err, ErrBusy) {
				return result, err
			}
			if !s.hasActiveSessionRun() {
				return Result{}, err
			}
			continue
		}
		result, err := s.appendStandaloneCustom(ctx, message)
		if !errors.Is(err, ErrBusy) {
			return result, err
		}
		if !s.hasActiveSessionRun() {
			return Result{}, err
		}
	}
}
