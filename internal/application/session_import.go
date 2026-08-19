package application

import (
	"context"
	"errors"
	"strings"
)

// ImportSession replaces one active runtime with an imported JSONL session and
// reconciles its new durable identity into the process-wide session catalog.
func (s *Service) ImportSession(
	ctx context.Context,
	currentID, inputPath, cwdOverride string,
) (SessionImportResult, error) {
	if s == nil {
		return SessionImportResult{}, errors.New("application service is unavailable")
	}
	ctx = normalizeContext(ctx)
	currentID, inputPath = strings.TrimSpace(currentID), strings.TrimSpace(inputPath)
	if currentID == "" || inputPath == "" {
		return SessionImportResult{}, errors.New("current session id and import path are required")
	}
	managed, err := s.open(ctx, currentID)
	if err != nil {
		return SessionImportResult{}, err
	}
	managed.touch()
	runtime := managed.session.Runtime()
	if runtime == nil {
		return SessionImportResult{}, errors.New("active runtime is unavailable")
	}
	result, err := runtime.ImportFromJSONL(ctx, inputPath, strings.TrimSpace(cwdOverride))
	if err != nil {
		return SessionImportResult{}, err
	}
	if result.Cancelled {
		return SessionImportResult{Cancelled: true}, nil
	}
	if err := s.reconcileIdentity(managed); err != nil {
		return SessionImportResult{}, err
	}
	state, err := managed.session.State()
	if err != nil {
		return SessionImportResult{}, err
	}
	s.events.publish(Event{SessionID: state.SessionID, Value: SessionCatalogEvent{Change: SessionCreated}})
	return SessionImportResult{State: state}, nil
}
