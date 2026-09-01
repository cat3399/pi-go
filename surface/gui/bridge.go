package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	coreapp "github.com/cat3399/pi-go/internal/app"
	coreapplication "github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/surfacewire"
	wails "github.com/wailsapp/wails/v3/pkg/application"
)

const (
	guiApplicationEvent = "pi:application-event"
	guiStreamResetEvent = "pi:application-stream-reset"
)

type HostInfo struct {
	Version               string `json:"version"`
	LocalAvailable        bool   `json:"localAvailable"`
	LocalError            string `json:"localError,omitempty"`
	DefaultRemoteEndpoint string `json:"defaultRemoteEndpoint,omitempty"`
}

type EventStreamOpened struct {
	StreamID      uint64                      `json:"streamId"`
	Revision      uint64                      `json:"revision"`
	ResetRequired bool                        `json:"resetRequired"`
	Replay        []surfacewire.EventEnvelope `json:"replay"`
}

type GUIApplicationEvent struct {
	StreamID uint64                    `json:"streamId"`
	Envelope surfacewire.EventEnvelope `json:"envelope"`
}

type GUIStreamReset struct {
	StreamID uint64 `json:"streamId"`
	Revision uint64 `json:"revision"`
	Reason   string `json:"reason,omitempty"`
}

type UploadResponse struct {
	Uploaded       []string                          `json:"uploaded"`
	Skipped        []string                          `json:"skipped"`
	Errors         []coreapplication.UploadFileError `json:"errors"`
	Conflicts      []string                          `json:"conflicts,omitempty"`
	NonReplaceable []string                          `json:"nonReplaceable,omitempty"`
}

type encodedUploadFile struct {
	Name string `json:"name"`
	Data string `json:"data"`
}

type GUIBridge struct {
	production    coreapp.ProductionConfig
	defaultRemote string
	version       string

	mu         sync.RWMutex
	ctx        context.Context
	wailsApp   *wails.App
	local      *coreapplication.Service
	localError error
	nextStream uint64
	streams    map[uint64]*guiEventStream
}

type guiEventStream struct {
	id           uint64
	subscription *coreapplication.EventSubscription

	mu      sync.Mutex
	started bool
	closed  bool
}

func NewGUIBridge(production coreapp.ProductionConfig, defaultRemote, version string) *GUIBridge {
	return &GUIBridge{
		production: production, defaultRemote: defaultRemote, version: version,
		streams: make(map[uint64]*guiEventStream),
	}
}

// ServiceStartup assembles the complete pi-go Application Service in-process.
// A startup error remains visible to the frontend so the same desktop binary
// can still connect to a remote core for recovery.
func (b *GUIBridge) ServiceStartup(ctx context.Context, _ wails.ServiceOptions) error {
	service, err := coreapplication.NewService(coreapplication.ServiceOptions{
		Context: ctx, Production: b.production,
	})
	b.mu.Lock()
	b.ctx = ctx
	b.wailsApp = wails.Get()
	b.local = service
	b.localError = err
	b.mu.Unlock()
	return nil
}

func (b *GUIBridge) ServiceShutdown() error {
	b.mu.Lock()
	streams := make([]*guiEventStream, 0, len(b.streams))
	for _, stream := range b.streams {
		streams = append(streams, stream)
	}
	b.streams = make(map[uint64]*guiEventStream)
	service := b.local
	b.local = nil
	b.mu.Unlock()

	for _, stream := range streams {
		stream.close()
	}
	if service == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return service.Close(ctx)
}

func (b *GUIBridge) HostInfo() HostInfo {
	b.mu.RLock()
	defer b.mu.RUnlock()
	info := HostInfo{
		Version: b.version, LocalAvailable: b.local != nil,
		DefaultRemoteEndpoint: b.defaultRemote,
	}
	if b.localError != nil {
		info.LocalError = b.localError.Error()
	}
	return info
}

func (b *GUIBridge) Snapshot() (surfacewire.ApplicationSnapshot, error) {
	api, err := b.localAPI()
	if err != nil {
		return surfacewire.ApplicationSnapshot{}, err
	}
	return surfacewire.Snapshot(api)
}

func (b *GUIBridge) SessionView(sessionID, leafID string) (surfacewire.SessionView, error) {
	api, err := b.localAPI()
	if err != nil {
		return surfacewire.SessionView{}, err
	}
	return surfacewire.SessionViewFor(api, sessionID, leafID, false, false)
}

func (b *GUIBridge) CreateSession(input surfacewire.CreateSessionRequest) (surfacewire.CreateSessionResult, error) {
	api, err := b.localAPI()
	if err != nil {
		return surfacewire.CreateSessionResult{}, err
	}
	return surfacewire.CreateSession(b.context(), api, input)
}

func (b *GUIBridge) Models(cwd string) (surfacewire.ModelsView, error) {
	api, err := b.localAPI()
	if err != nil {
		return surfacewire.ModelsView{}, err
	}
	return surfacewire.Models(b.context(), api, cwd)
}

func (b *GUIBridge) BrowseDirectories(path string) (surfacewire.DirectoryView, error) {
	api, err := b.localAPI()
	if err != nil {
		return surfacewire.DirectoryView{}, err
	}
	return surfacewire.BrowseDirectories(b.context(), api, path)
}

func (b *GUIBridge) ListFiles(path string) (surfacewire.FileList, error) {
	api, err := b.localAPI()
	if err != nil {
		return surfacewire.FileList{}, err
	}
	return surfacewire.ListFiles(b.context(), api, path)
}

func (b *GUIBridge) PreviewFile(path string) (surfacewire.FilePreview, error) {
	api, err := b.localAPI()
	if err != nil {
		return surfacewire.FilePreview{}, err
	}
	return surfacewire.PreviewFile(b.context(), api, path)
}

func (b *GUIBridge) DeleteFile(path string) error {
	api, err := b.localAPI()
	if err != nil {
		return err
	}
	return api.DeleteFile(b.context(), path)
}

func (b *GUIBridge) InspectUploadTargets(directory string, fileNames []string) (coreapplication.UploadTargetInspection, error) {
	api, err := b.localAPI()
	if err != nil {
		return coreapplication.UploadTargetInspection{}, err
	}
	return api.InspectUploadTargets(b.context(), directory, fileNames)
}

func (b *GUIBridge) UploadFiles(directory, filesJSON, strategy string) (UploadResponse, error) {
	api, err := b.localAPI()
	if err != nil {
		return UploadResponse{}, err
	}
	files, err := decodeUploadFiles(filesJSON)
	if err != nil {
		return UploadResponse{}, err
	}
	result, err := api.SaveUploads(
		b.context(), directory, files, coreapplication.UploadConflictStrategy(strategy),
	)
	response := UploadResponse{
		Uploaded: result.Uploaded, Skipped: result.Skipped, Errors: result.Errors,
	}
	if errors.Is(err, coreapplication.ErrUploadConflict) {
		response.Conflicts = result.Inspection.Conflicts
		response.NonReplaceable = result.Inspection.NonReplaceable
		return response, nil
	}
	return response, err
}

func (b *GUIBridge) AddProject(path string) (surfacewire.ProjectInfo, error) {
	api, err := b.localAPI()
	if err != nil {
		return surfacewire.ProjectInfo{}, err
	}
	return surfacewire.AddProject(b.context(), api, path)
}

func decodeUploadFiles(payload string) ([]coreapplication.UploadFile, error) {
	var encoded []encodedUploadFile
	if err := json.Unmarshal([]byte(payload), &encoded); err != nil {
		return nil, fmt.Errorf("decode upload files: %w", err)
	}
	if len(encoded) == 0 {
		return nil, errors.New("no files selected")
	}
	files := make([]coreapplication.UploadFile, 0, len(encoded))
	var total int64
	for _, input := range encoded {
		if len(input.Data) > base64.StdEncoding.EncodedLen(int(coreapplication.MaxUploadFileBytes)) {
			return nil, fmt.Errorf("upload file %q exceeds 25MB", input.Name)
		}
		data, err := base64.StdEncoding.DecodeString(input.Data)
		if err != nil {
			return nil, fmt.Errorf("decode upload file %q: %w", input.Name, err)
		}
		if total += int64(len(data)); total > coreapplication.MaxUploadTotalBytes {
			return nil, errors.New("uploads exceed 100MB total")
		}
		files = append(files, coreapplication.UploadFile{Name: input.Name, Data: data})
	}
	return files, nil
}

func (b *GUIBridge) RemoveProject(path string) error {
	api, err := b.localAPI()
	if err != nil {
		return err
	}
	return surfacewire.RemoveProject(b.context(), api, path)
}

func (b *GUIBridge) RenameSession(sessionID, name string) error {
	api, err := b.localAPI()
	if err != nil {
		return err
	}
	return surfacewire.RenameSession(b.context(), api, sessionID, name)
}

func (b *GUIBridge) DeleteSession(sessionID string) error {
	api, err := b.localAPI()
	if err != nil {
		return err
	}
	return surfacewire.DeleteSession(b.context(), api, sessionID)
}

func (b *GUIBridge) Dispatch(sessionID, commandJSON string) (surfacewire.CommandResponse, error) {
	api, err := b.localAPI()
	if err != nil {
		return surfacewire.CommandResponse{}, err
	}
	payload, err := surfacewire.DecodeCommandPayload(commandJSON)
	if err != nil {
		return surfacewire.CommandResponse{}, err
	}
	return surfacewire.DispatchJSON(b.context(), api, sessionID, payload, agent.InputInteractive)
}

func (b *GUIBridge) OpenEventStream(after uint64) (EventStreamOpened, error) {
	api, err := b.localAPI()
	if err != nil {
		return EventStreamOpened{}, err
	}
	subscription, err := api.SubscribeEvents(after)
	resetRequired := errors.Is(err, coreapplication.ErrEventCursorUnavailable)
	if resetRequired {
		after = api.CurrentRevision()
		subscription, err = api.SubscribeEvents(after)
	}
	if err != nil {
		return EventStreamOpened{}, err
	}
	replay := make([]surfacewire.EventEnvelope, 0, len(subscription.Replay))
	for _, event := range subscription.Replay {
		envelope, encodeErr := surfacewire.EncodeEvent(event)
		if encodeErr != nil {
			subscription.Close()
			return EventStreamOpened{}, encodeErr
		}
		replay = append(replay, envelope)
	}

	b.mu.Lock()
	b.nextStream++
	streamID := b.nextStream
	stream := &guiEventStream{id: streamID, subscription: subscription}
	b.streams[streamID] = stream
	b.mu.Unlock()
	return EventStreamOpened{
		StreamID: streamID, Revision: subscription.Revision,
		ResetRequired: resetRequired, Replay: replay,
	}, nil
}

// ResumeEventStream is deliberately separate from OpenEventStream: the
// frontend applies the replay first while the core subscription buffers live
// events, then starts delivery without a replay/live ordering race.
func (b *GUIBridge) ResumeEventStream(streamID uint64) error {
	b.mu.RLock()
	stream := b.streams[streamID]
	b.mu.RUnlock()
	if stream == nil {
		return fmt.Errorf("event stream %d is not open", streamID)
	}
	if !stream.start() {
		return fmt.Errorf("event stream %d is already started or closed", streamID)
	}
	go b.pumpEventStream(stream)
	return nil
}

func (b *GUIBridge) CloseEventStream(streamID uint64) {
	b.mu.Lock()
	stream := b.streams[streamID]
	delete(b.streams, streamID)
	b.mu.Unlock()
	if stream != nil {
		stream.close()
	}
}

func (b *GUIBridge) pumpEventStream(stream *guiEventStream) {
	defer func() {
		b.mu.Lock()
		if b.streams[stream.id] == stream {
			delete(b.streams, stream.id)
		}
		b.mu.Unlock()
		stream.close()
	}()

	for {
		select {
		case <-b.context().Done():
			return
		case event, ok := <-stream.subscription.Events:
			if !ok {
				if !stream.isClosed() {
					b.emitReset(stream.id, "application event cursor must be refreshed")
				}
				return
			}
			envelope, err := surfacewire.EncodeEvent(event)
			if err != nil {
				b.emitReset(stream.id, err.Error())
				return
			}
			b.emit(guiApplicationEvent, GUIApplicationEvent{StreamID: stream.id, Envelope: envelope})
		}
	}
}

func (b *GUIBridge) emitReset(streamID uint64, reason string) {
	api, err := b.localAPI()
	if err != nil {
		return
	}
	b.emit(guiStreamResetEvent, GUIStreamReset{
		StreamID: streamID, Revision: api.CurrentRevision(), Reason: reason,
	})
}

func (b *GUIBridge) emit(name string, payload any) {
	b.mu.RLock()
	application := b.wailsApp
	b.mu.RUnlock()
	if application != nil {
		application.Event.Emit(name, payload)
	}
}

func (b *GUIBridge) localAPI() (coreapplication.API, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.local != nil {
		return b.local, nil
	}
	if b.localError != nil {
		return nil, fmt.Errorf("embedded pi-go core is unavailable: %w", b.localError)
	}
	return nil, errors.New("embedded pi-go core has not started")
}

func (b *GUIBridge) context() context.Context {
	b.mu.RLock()
	ctx := b.ctx
	b.mu.RUnlock()
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func (s *guiEventStream) start() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.closed {
		return false
	}
	s.started = true
	return true
}

func (s *guiEventStream) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	subscription := s.subscription
	s.mu.Unlock()
	if subscription != nil {
		subscription.Close()
	}
}

func (s *guiEventStream) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}
