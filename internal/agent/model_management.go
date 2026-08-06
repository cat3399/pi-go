package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

// ScopedModel is one --models entry after source-faithful pattern resolution.
// A nil ThinkingLevel inherits the session preference when selected.
type ScopedModel struct {
	Model         provider.Model
	ThinkingLevel *provider.ThinkingLevel
}

type ModelCycleDirection string

const (
	CycleForward  ModelCycleDirection = "forward"
	CycleBackward ModelCycleDirection = "backward"
)

type ModelCycleResult struct {
	Model         provider.Model
	ThinkingLevel provider.ThinkingLevel
	IsScoped      bool
}

// SettingsUpdate uses pointers to distinguish an unchanged field from a real
// value. Empty provider/model values are used only by compensating writes that
// restore previously absent global defaults.
type SettingsUpdate struct {
	DefaultProvider      *string
	DefaultModel         *string
	DefaultThinkingLevel *provider.ThinkingLevel
}

// SettingsUndo restores only fields still equal to the values written by its
// update. Production uses this conditional form so a failed transcript commit
// cannot overwrite a concurrent settings change.
type SettingsUndo func(context.Context) error

type SettingsWriteResult struct {
	Undo          SettingsUndo
	CommitUnknown bool
}

type SettingsPersistence func(context.Context, SettingsUpdate) (SettingsWriteResult, error)

func cloneModels(models []provider.Model) []provider.Model {
	return append([]provider.Model(nil), models...)
}

func cloneScopedModels(models []ScopedModel) []ScopedModel {
	result := make([]ScopedModel, len(models))
	for index, scoped := range models {
		result[index] = ScopedModel{Model: scoped.Model}
		if scoped.ThinkingLevel != nil {
			level := *scoped.ThinkingLevel
			result[index].ThinkingLevel = &level
		}
	}
	return result
}

// ScopedModels returns a copy-only snapshot of the current cycle scope.
func (s *AgentSession) ScopedModels() []ScopedModel {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneScopedModels(s.scopedModels)
}

// SetScopedModels replaces the cycle scope without retaining caller-owned
// slices or thinking pointers, matching pi's mutable scopedModels property.
func (s *AgentSession) SetScopedModels(models []ScopedModel) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.scopedModels = cloneScopedModels(models)
	s.mu.Unlock()
}

func (s *AgentSession) AvailableThinkingLevels() []provider.ThinkingLevel {
	if s == nil {
		return nil
	}
	model, hasModel, _ := s.selectionSnapshot()
	if !hasModel {
		return []provider.ThinkingLevel{
			provider.ThinkingOff, provider.ThinkingMinimal, provider.ThinkingLow,
			provider.ThinkingMedium, provider.ThinkingHigh,
		}
	}
	return model.SupportedThinkingLevels()
}

func (s *AgentSession) SupportsThinking() bool {
	if s == nil {
		return false
	}
	model, hasModel, _ := s.selectionSnapshot()
	return hasModel && model.Reasoning()
}

// CycleThinkingLevel advances through the exact supported-level order. The
// absent result is represented by nil when the current model cannot reason.
func (s *AgentSession) CycleThinkingLevel() (*provider.ThinkingLevel, error) {
	if s == nil {
		return nil, nil
	}
	s.controlMu.Lock()
	model, hasModel, current := s.selectionSnapshot()
	if !hasModel || !model.Reasoning() {
		s.controlMu.Unlock()
		return nil, nil
	}
	levels := model.SupportedThinkingLevels()
	if len(levels) == 0 {
		var event thinkingSelectionEvent
		var err error
		if current != provider.ThinkingOff {
			event, err = s.setThinkingLevelLocked(provider.ThinkingOff)
		}
		s.controlMu.Unlock()
		s.emitThinkingSelection(event)
		return nil, err
	}
	currentIndex := -1
	for index, level := range levels {
		if level == current {
			currentIndex = index
			break
		}
	}
	next := levels[(currentIndex+1)%len(levels)]
	event, err := s.setThinkingLevelLocked(next)
	s.controlMu.Unlock()
	s.emitThinkingSelection(event)
	if err != nil {
		return nil, err
	}
	selected := next
	if event.published {
		selected = event.selected
	}
	return &selected, nil
}

func (s *AgentSession) CycleModel(ctx context.Context, direction ModelCycleDirection) (*ModelCycleResult, error) {
	if s == nil {
		return nil, fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil model cycle context", ErrInvalidRun)
	}
	if direction == "" {
		direction = CycleForward
	}
	if direction != CycleForward && direction != CycleBackward {
		return nil, fmt.Errorf("%w: invalid model cycle direction %q", ErrInvalidConfig, direction)
	}
	s.mu.RLock()
	scoped := cloneScopedModels(s.scopedModels)
	s.mu.RUnlock()
	if len(scoped) != 0 {
		available, err := s.availableScopedModels(ctx, scoped)
		if err != nil {
			return nil, err
		}
		if len(available) <= 1 {
			return nil, nil
		}
		s.controlMu.Lock()
		current, _, _ := s.selectionSnapshot()
		next := nextScopedModel(available, current, direction)
		event, err := s.setModelSelectionLocked(ctx, next.Model, next.ThinkingLevel, ModelSelectCycle)
		s.controlMu.Unlock()
		s.emitModelSelectionEvent(event)
		if err != nil {
			return nil, err
		}
		return &ModelCycleResult{Model: next.Model, ThinkingLevel: s.ThinkingLevel(), IsScoped: true}, nil
	}
	available, err := s.availableModels(ctx)
	if err != nil {
		return nil, err
	}
	if len(available) <= 1 {
		return nil, nil
	}
	s.controlMu.Lock()
	current, _, _ := s.selectionSnapshot()
	next := nextAvailableModel(available, current, direction)
	event, err := s.setModelSelectionLocked(ctx, next, nil, ModelSelectCycle)
	s.controlMu.Unlock()
	s.emitModelSelectionEvent(event)
	if err != nil {
		return nil, err
	}
	return &ModelCycleResult{Model: next, ThinkingLevel: s.ThinkingLevel(), IsScoped: false}, nil
}

type availabilityResult struct {
	available bool
	err       error
}

func (s *AgentSession) availableScopedModels(ctx context.Context, scoped []ScopedModel) ([]ScopedModel, error) {
	results := make([]availabilityResult, len(scoped))
	var wait sync.WaitGroup
	wait.Add(len(scoped))
	for index := range scoped {
		go func(index int) {
			defer wait.Done()
			defer func() {
				if recovered := recover(); recovered != nil {
					candidate := scoped[index].Model
					results[index].err = fmt.Errorf("%w: model availability callback panicked for %s/%s: %s", ErrInvariant, candidate.Provider(), candidate.ID(), safeValueText(recovered))
				}
			}()
			results[index].available, results[index].err = s.isModelAvailable(ctx, scoped[index].Model)
		}(index)
	}
	wait.Wait()
	available := make([]ScopedModel, 0, len(scoped))
	for index, result := range results {
		if result.err != nil {
			return nil, result.err
		}
		if result.available {
			available = append(available, scoped[index])
		}
	}
	return available, nil
}

func (s *AgentSession) availableModels(ctx context.Context) ([]provider.Model, error) {
	if s.resolveAvailableModels != nil {
		models, err := s.resolveAvailableModels(ctx)
		if err != nil {
			return nil, err
		}
		return cloneModels(models), nil
	}
	s.mu.RLock()
	all := cloneModels(s.allModels)
	s.mu.RUnlock()
	available := make([]provider.Model, 0, len(all))
	for _, candidate := range all {
		ok, err := s.isModelAvailable(ctx, candidate)
		if err != nil {
			return nil, err
		}
		if ok {
			available = append(available, candidate)
		}
	}
	return available, nil
}

func nextScopedModel(models []ScopedModel, current provider.Model, direction ModelCycleDirection) ScopedModel {
	index := 0
	for candidateIndex, candidate := range models {
		if candidate.Model.Equal(current) {
			index = candidateIndex
			break
		}
	}
	if direction == CycleBackward {
		index = (index - 1 + len(models)) % len(models)
	} else {
		index = (index + 1) % len(models)
	}
	return models[index]
}

func nextAvailableModel(models []provider.Model, current provider.Model, direction ModelCycleDirection) provider.Model {
	index := 0
	for candidateIndex, candidate := range models {
		if candidate.Equal(current) {
			index = candidateIndex
			break
		}
	}
	if direction == CycleBackward {
		index = (index - 1 + len(models)) % len(models)
	} else {
		index = (index + 1) % len(models)
	}
	return models[index]
}

func (s *AgentSession) isModelAvailable(ctx context.Context, model provider.Model) (bool, error) {
	if s.modelAvailable == nil {
		return true, nil
	}
	return s.modelAvailable(ctx, model)
}

func (s *AgentSession) SetModel(model provider.Model) error {
	return s.SetModelContext(context.Background(), model)
}

func (s *AgentSession) SetModelContext(ctx context.Context, model provider.Model) error {
	if s == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	return s.setModelSelection(ctx, model, nil, ModelSelectSet)
}

func (s *AgentSession) setModelSelection(ctx context.Context, model provider.Model, explicitThinking *provider.ThinkingLevel, source ModelSelectSource) error {
	if s == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	s.controlMu.Lock()
	event, err := s.setModelSelectionLocked(ctx, model, explicitThinking, source)
	s.controlMu.Unlock()
	s.emitModelSelectionEvent(event)
	return err
}

type modelSelectionEvent struct {
	previous         provider.Model
	hadPrevious      bool
	previousThinking provider.ThinkingLevel
	model            provider.Model
	thinking         provider.ThinkingLevel
	source           ModelSelectSource
	published        bool
}

func (s *AgentSession) setModelSelectionLocked(ctx context.Context, model provider.Model, explicitThinking *provider.ThinkingLevel, source ModelSelectSource) (modelSelectionEvent, error) {
	if s == nil {
		return modelSelectionEvent{}, fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	if ctx == nil {
		return modelSelectionEvent{}, fmt.Errorf("%w: nil model access context", ErrInvalidRun)
	}
	if err := s.rejectIfClosed(); err != nil {
		return modelSelectionEvent{}, err
	}
	if _, err := provider.NewRequest(model, "", nil); err != nil {
		return modelSelectionEvent{}, fmt.Errorf("%w: model: %w", ErrInvalidConfig, err)
	}
	if routes, ok := s.loop.config.provider.(provider.RouteValidator); ok && !routes.SupportsModel(model) {
		return modelSelectionEvent{}, fmt.Errorf("%w: no provider adapter for %s/%s", ErrInvalidConfig, model.Provider(), model.API())
	}
	if s.validateSelect != nil {
		if err := s.validateSelect(ctx, model); err != nil {
			return modelSelectionEvent{}, err
		}
	}
	if explicitThinking != nil && !explicitThinking.Valid() {
		return modelSelectionEvent{}, fmt.Errorf("%w: invalid thinking level %q", ErrInvalidConfig, *explicitThinking)
	}

	s.lifecycleMu.Lock()
	if s.closed || s.closing {
		s.lifecycleMu.Unlock()
		return modelSelectionEvent{}, fmt.Errorf("%w: session is closed", ErrInvalidRun)
	}
	s.mu.RLock()
	previous, hadPrevious, previousThinking := s.model, s.hasModel, s.thinkingLevel
	s.mu.RUnlock()
	desired := previousThinking
	if explicitThinking != nil {
		desired = *explicitThinking
	} else if !hadPrevious || !previous.Reasoning() {
		var err error
		desired, err = s.currentDefaultThinking()
		if err != nil {
			s.lifecycleMu.Unlock()
			return modelSelectionEvent{}, err
		}
	}
	selectedThinking := model.ClampThinkingLevel(desired)
	thinkingChanged := selectedThinking != previousThinking
	persistThinking := thinkingChanged && (model.Reasoning() || selectedThinking != provider.ThinkingOff)
	providerID, modelID := model.Provider(), model.ID()
	update := SettingsUpdate{DefaultProvider: &providerID, DefaultModel: &modelID}
	if persistThinking {
		level := selectedThinking
		update.DefaultThinkingLevel = &level
	}
	settingsWrite, settingsErr := s.writeSettings(update)
	if settingsErr != nil && !settingsWrite.CommitUnknown {
		s.lifecycleMu.Unlock()
		return modelSelectionEvent{}, settingsErr
	}
	settlement, cancel := context.WithTimeout(context.Background(), s.settlementTimeout)
	var thinkingEntry *string
	if thinkingChanged {
		value := string(selectedThinking)
		thinkingEntry = &value
	}
	_, transcriptErr := s.appendModelControl(settlement, providerID, modelID, thinkingEntry)
	cancel()
	if transcriptErr != nil {
		if errors.Is(transcriptErr, session.ErrCommitUnknown) {
			// The complete control batch may already be durable. Keep settings and
			// publish the same selection in memory; rolling settings back would make
			// either possible disk outcome less recoverable. The poisoned session
			// forces reopen/reconciliation before another transcript mutation.
			if err := s.publishModelSelection(model, selectedThinking, persistThinking); err != nil {
				s.lifecycleMu.Unlock()
				return modelSelectionEvent{}, errors.Join(fmt.Errorf("%w: model change: %w", ErrTranscriptCommit, transcriptErr), err)
			}
			s.lifecycleMu.Unlock()
			event := modelSelectionEvent{previous: previous, hadPrevious: hadPrevious, previousThinking: previousThinking, model: model, thinking: selectedThinking, source: source, published: true}
			return event, errors.Join(fmt.Errorf("%w: model change outcome unknown: %w", ErrTranscriptCommit, transcriptErr), settingsErr)
		}
		rollbackErr := runSettingsUndo(s.settlementTimeout, settingsWrite.Undo)
		s.lifecycleMu.Unlock()
		base := fmt.Errorf("%w: model change: %w", ErrTranscriptCommit, transcriptErr)
		if rollbackErr != nil {
			return modelSelectionEvent{}, errors.Join(base, settingsErr, fmt.Errorf("restore settings: %w", rollbackErr))
		}
		return modelSelectionEvent{}, errors.Join(base, settingsErr)
	}
	if err := s.publishModelSelection(model, selectedThinking, persistThinking); err != nil {
		s.lifecycleMu.Unlock()
		return modelSelectionEvent{}, err
	}
	s.lifecycleMu.Unlock()
	return modelSelectionEvent{previous: previous, hadPrevious: hadPrevious, previousThinking: previousThinking, model: model, thinking: selectedThinking, source: source, published: true}, settingsErr
}

func (s *AgentSession) emitModelSelectionEvent(event modelSelectionEvent) {
	if !event.published {
		return
	}
	s.emitModelSelection(event.previous, event.hadPrevious, event.previousThinking, event.model, event.thinking, event.source)
}

func (s *AgentSession) publishModelSelection(model provider.Model, thinking provider.ThinkingLevel, persistThinking bool) error {
	s.selectionMu.Lock()
	defer s.selectionMu.Unlock()
	if err := s.loop.SetModelAndThinking(model, thinking); err != nil {
		return err
	}
	s.mu.Lock()
	s.model, s.hasModel, s.thinkingLevel = model, true, thinking
	if persistThinking && s.resolveDefaultThinking == nil {
		s.defaultThinking = thinking
	}
	s.mu.Unlock()
	return nil
}

func (s *AgentSession) emitModelSelection(previous provider.Model, hadPrevious bool, previousThinking provider.ThinkingLevel, model provider.Model, thinking provider.ThinkingLevel, source ModelSelectSource) {
	if thinking != previousThinking {
		s.emitThinkingLevelChanged(context.Background(), thinking)
		if hook := s.hooks.ThinkingLevelSelect; hook != nil {
			_ = hook(context.Background(), ThinkingLevelSelectEvent{Level: thinking, PreviousLevel: previousThinking})
		}
	}
	if hook := s.hooks.ModelSelect; hook != nil && (!hadPrevious || !previous.Equal(model)) {
		var previousModel *provider.Model
		if hadPrevious {
			copy := previous
			previousModel = &copy
		}
		_ = hook(context.Background(), ModelSelectEvent{Model: model, PreviousModel: previousModel, Source: source})
	}
}

func (s *AgentSession) SetThinkingLevel(level provider.ThinkingLevel) error {
	if s == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	s.controlMu.Lock()
	event, err := s.setThinkingLevelLocked(level)
	s.controlMu.Unlock()
	s.emitThinkingSelection(event)
	return err
}

type thinkingSelectionEvent struct {
	previous  provider.ThinkingLevel
	selected  provider.ThinkingLevel
	published bool
}

func (s *AgentSession) setThinkingLevelLocked(level provider.ThinkingLevel) (thinkingSelectionEvent, error) {
	if !level.Valid() {
		return thinkingSelectionEvent{}, fmt.Errorf("%w: invalid thinking level %q", ErrInvalidConfig, level)
	}
	s.lifecycleMu.Lock()
	if s.closed || s.closing {
		s.lifecycleMu.Unlock()
		return thinkingSelectionEvent{}, fmt.Errorf("%w: session is closed", ErrInvalidRun)
	}
	s.mu.RLock()
	previous, model, hasModel := s.thinkingLevel, s.model, s.hasModel
	s.mu.RUnlock()
	selected := level
	if hasModel {
		selected = model.ClampThinkingLevel(level)
	} else if level == provider.ThinkingXHigh || level == provider.ThinkingMax {
		// The model-less surface exposes only pi's standard five levels. Extended
		// requests therefore take the no-model clamp path and normalize to off.
		selected = provider.ThinkingOff
	}
	if selected == previous {
		s.lifecycleMu.Unlock()
		return thinkingSelectionEvent{}, nil
	}
	persistDefault := (hasModel && model.Reasoning()) || selected != provider.ThinkingOff
	var settingsWrite SettingsWriteResult
	var settingsErr error
	if persistDefault {
		updateLevel := selected
		settingsWrite, settingsErr = s.writeSettings(SettingsUpdate{DefaultThinkingLevel: &updateLevel})
		if settingsErr != nil && !settingsWrite.CommitUnknown {
			s.lifecycleMu.Unlock()
			return thinkingSelectionEvent{}, settingsErr
		}
	}
	settlement, cancel := context.WithTimeout(context.Background(), s.settlementTimeout)
	_, persistErr := s.appendThinkingControl(settlement, string(selected))
	cancel()
	if persistErr != nil {
		if errors.Is(persistErr, session.ErrCommitUnknown) {
			if err := s.publishThinkingLevel(selected, persistDefault); err != nil {
				s.lifecycleMu.Unlock()
				return thinkingSelectionEvent{}, errors.Join(fmt.Errorf("%w: thinking level outcome unknown: %w", ErrTranscriptCommit, persistErr), err)
			}
			s.lifecycleMu.Unlock()
			event := thinkingSelectionEvent{previous: previous, selected: selected, published: true}
			return event, errors.Join(fmt.Errorf("%w: thinking level outcome unknown: %w", ErrTranscriptCommit, persistErr), settingsErr)
		}
		rollbackErr := runSettingsUndo(s.settlementTimeout, settingsWrite.Undo)
		s.lifecycleMu.Unlock()
		base := fmt.Errorf("%w: thinking level change: %w", ErrTranscriptCommit, persistErr)
		if rollbackErr != nil {
			return thinkingSelectionEvent{}, errors.Join(base, settingsErr, fmt.Errorf("restore settings: %w", rollbackErr))
		}
		return thinkingSelectionEvent{}, errors.Join(base, settingsErr)
	}
	if err := s.publishThinkingLevel(selected, persistDefault); err != nil {
		s.lifecycleMu.Unlock()
		return thinkingSelectionEvent{}, err
	}
	s.lifecycleMu.Unlock()
	return thinkingSelectionEvent{previous: previous, selected: selected, published: true}, settingsErr
}

func (s *AgentSession) emitThinkingSelection(event thinkingSelectionEvent) {
	if !event.published {
		return
	}
	s.emitThinkingLevelChanged(context.Background(), event.selected)
	if hook := s.hooks.ThinkingLevelSelect; hook != nil {
		_ = hook(context.Background(), ThinkingLevelSelectEvent{Level: event.selected, PreviousLevel: event.previous})
	}
}

func (s *AgentSession) publishThinkingLevel(thinking provider.ThinkingLevel, persistDefault bool) error {
	s.selectionMu.Lock()
	defer s.selectionMu.Unlock()
	if err := s.loop.SetThinkingLevel(thinking); err != nil {
		return err
	}
	s.mu.Lock()
	s.thinkingLevel = thinking
	if persistDefault && s.resolveDefaultThinking == nil {
		s.defaultThinking = thinking
	}
	s.mu.Unlock()
	return nil
}

func (s *AgentSession) currentDefaultThinking() (provider.ThinkingLevel, error) {
	if s.resolveDefaultThinking != nil {
		if level, ok := s.resolveDefaultThinking(); ok {
			if !level.Valid() {
				return "", fmt.Errorf("%w: invalid effective default thinking level %q", ErrInvalidConfig, level)
			}
			return level, nil
		}
		return provider.ThinkingMedium, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.defaultThinking, nil
}

func (s *AgentSession) writeSettings(update SettingsUpdate) (SettingsWriteResult, error) {
	if s.persistSettings == nil {
		return SettingsWriteResult{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.settlementTimeout)
	defer cancel()
	result, err := s.persistSettings(ctx, update)
	if err != nil {
		if result.CommitUnknown {
			return result, fmt.Errorf("persist model settings outcome unknown: %w", err)
		}
		return SettingsWriteResult{}, fmt.Errorf("persist model settings: %w", err)
	}
	if result.CommitUnknown {
		return SettingsWriteResult{}, fmt.Errorf("%w: settings persistence reported unknown without an error", ErrInvariant)
	}
	return result, nil
}

func runSettingsUndo(timeout time.Duration, undo SettingsUndo) error {
	if undo == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := undo(ctx); err != nil {
		return fmt.Errorf("persist model settings: %w", err)
	}
	return nil
}
