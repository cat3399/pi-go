package surfacewire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/application"
	protocolv1 "github.com/cat3399/pi-go/internal/protocol/v1"
	"github.com/cat3399/pi-go/internal/provider"
)

var ErrInvalidRequest = errors.New("invalid surface request")

func invalidRequest(err error) error {
	return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
}

// ApplicationSnapshot is the cursor-consistent application projection shared
// by HTTP/SSE and native GUI IPC. It contains no surface-owned state.
type ApplicationSnapshot struct {
	Revision          uint64        `json:"revision"`
	AgentDir          string        `json:"agentDir"`
	DefaultCWD        string        `json:"defaultCwd"`
	Sessions          []SessionInfo `json:"sessions"`
	RunningSessionIDs []string      `json:"runningSessionIds"`
}

func Snapshot(api application.API) (ApplicationSnapshot, error) {
	if api == nil {
		return ApplicationSnapshot{}, errors.New("application API is required")
	}
	// The cursor is sampled before the catalog. Events published during the
	// read remain replayable by either SSE or GUI IPC.
	revision := api.CurrentRevision()
	sessions, err := ListSessions(api)
	if err != nil {
		return ApplicationSnapshot{}, err
	}
	return ApplicationSnapshot{
		Revision: revision, AgentDir: api.AgentDir(), DefaultCWD: api.DefaultCWD(),
		Sessions: sessions, RunningSessionIDs: api.RunningIDs(),
	}, nil
}

type CreateSessionRequest struct {
	CWD           string                  `json:"cwd"`
	Provider      string                  `json:"provider,omitempty"`
	ModelID       string                  `json:"modelId,omitempty"`
	ToolNames     *[]string               `json:"toolNames,omitempty"`
	ThinkingLevel *provider.ThinkingLevel `json:"thinkingLevel,omitempty"`
}

type CreateSessionResult struct {
	SessionID     string                 `json:"sessionId"`
	Revision      uint64                 `json:"revision"`
	Model         *SelectedModel         `json:"model"`
	ThinkingLevel provider.ThinkingLevel `json:"thinkingLevel"`
}

type DirectoryEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type DirectoryView struct {
	Path        string           `json:"path"`
	ParentPath  *string          `json:"parentPath"`
	Directories []DirectoryEntry `json:"directories"`
	Drives      []DirectoryEntry `json:"drives,omitempty"`
}

type FileEntry struct {
	Name     string `json:"name"`
	IsDir    bool   `json:"isDir"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
}

type FileList struct {
	Path    string      `json:"path"`
	Entries []FileEntry `json:"entries"`
}

func BrowseDirectories(ctx context.Context, api application.API, requested string) (DirectoryView, error) {
	if api == nil {
		return DirectoryView{}, errors.New("application API is required")
	}
	result, err := api.BrowseDirectories(ctx, requested)
	if err != nil {
		return DirectoryView{}, err
	}
	view := DirectoryView{
		Path: result.Path, ParentPath: result.ParentPath,
		Directories: make([]DirectoryEntry, 0, len(result.Directories)),
	}
	for _, entry := range result.Directories {
		view.Directories = append(view.Directories, DirectoryEntry{Name: entry.Name, Path: entry.Path})
	}
	if result.Drives != nil {
		view.Drives = make([]DirectoryEntry, 0, len(result.Drives))
		for _, entry := range result.Drives {
			view.Drives = append(view.Drives, DirectoryEntry{Name: entry.Name, Path: entry.Path})
		}
	}
	return view, nil
}

func ListFiles(ctx context.Context, api application.API, requested string) (FileList, error) {
	if api == nil {
		return FileList{}, errors.New("application API is required")
	}
	result, err := api.ListFiles(ctx, requested)
	if err != nil {
		return FileList{}, err
	}
	view := FileList{
		Path: result.Path, Entries: make([]FileEntry, 0, len(result.Entries)),
	}
	for _, entry := range result.Entries {
		view.Entries = append(view.Entries, FileEntry{
			Name: entry.Name, IsDir: entry.IsDir, Size: entry.Size, Modified: entry.Modified,
		})
	}
	return view, nil
}

func CreateSession(ctx context.Context, api application.API, input CreateSessionRequest) (CreateSessionResult, error) {
	if api == nil {
		return CreateSessionResult{}, errors.New("application API is required")
	}
	input.CWD = strings.TrimSpace(input.CWD)
	input.Provider = strings.TrimSpace(input.Provider)
	input.ModelID = strings.TrimSpace(input.ModelID)
	if input.CWD == "" {
		return CreateSessionResult{}, invalidRequest(errors.New("cwd is required"))
	}
	resolved, err := NormalizeUserCWD(input.CWD)
	if err != nil {
		return CreateSessionResult{}, invalidRequest(err)
	}
	if (input.Provider == "") != (input.ModelID == "") {
		return CreateSessionResult{}, invalidRequest(errors.New("provider and modelId must be provided together"))
	}
	if input.ToolNames != nil && *input.ToolNames == nil {
		return CreateSessionResult{}, invalidRequest(errors.New("toolNames must be an array of strings"))
	}
	if input.ThinkingLevel != nil && !input.ThinkingLevel.Valid() {
		return CreateSessionResult{}, invalidRequest(fmt.Errorf("invalid thinking level: %s", *input.ThinkingLevel))
	}
	toolNames := []string(nil)
	if input.ToolNames != nil {
		toolNames = append(toolNames, (*input.ToolNames)...)
	}
	state, err := api.NewSession(ctx, application.NewSessionOptions{
		CWD: resolved, Provider: input.Provider, ModelID: input.ModelID,
		ToolNames: toolNames, HasToolNames: input.ToolNames != nil, ThinkingLevel: input.ThinkingLevel,
	})
	if err != nil {
		return CreateSessionResult{}, err
	}
	return CreateSessionResult{
		SessionID: state.SessionID, Revision: api.CurrentRevision(),
		Model: StateModel(state), ThinkingLevel: state.ThinkingLevel,
	}, nil
}

func NormalizeUserCWD(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "~" || strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if value == "~" {
			value = home
		} else {
			value = filepath.Join(home, value[2:])
		}
	}
	return application.ValidateCWD(value)
}

func RenameSession(ctx context.Context, api application.API, sessionID, name string) error {
	if api == nil {
		return errors.New("application API is required")
	}
	sessionID = strings.TrimSpace(sessionID)
	name = strings.TrimSpace(name)
	if sessionID == "" {
		return invalidRequest(errors.New("session id is required"))
	}
	if name == "" {
		return invalidRequest(errors.New("name cannot be empty"))
	}
	return api.RenameSession(ctx, sessionID, name)
}

func DeleteSession(ctx context.Context, api application.API, sessionID string) error {
	if api == nil {
		return errors.New("application API is required")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return invalidRequest(errors.New("session id is required"))
	}
	return api.DeleteSession(ctx, sessionID)
}

type CommandResponse struct {
	Data any `json:"data"`
}

func DispatchJSON(ctx context.Context, api application.API, sessionID string, payload []byte, source agent.InputSource) (CommandResponse, error) {
	if api == nil {
		return CommandResponse{}, errors.New("application API is required")
	}
	command, err := protocolv1.DecodeCommand(payload, source)
	if err != nil {
		return CommandResponse{}, invalidRequest(err)
	}
	result, err := api.Dispatch(ctx, sessionID, command)
	if err != nil {
		return CommandResponse{}, err
	}
	data, present, err := protocolv1.EncodeResult(result)
	if err != nil {
		return CommandResponse{}, err
	}
	if !present {
		data = nil
	}
	return CommandResponse{Data: data}, nil
}

type EventEnvelope struct {
	Sequence  uint64 `json:"sequence"`
	SessionID string `json:"sessionId"`
	Event     any    `json:"event"`
}

func EncodeEvent(event application.Event) (EventEnvelope, error) {
	encoded, err := protocolv1.EncodeEvent(event)
	if err != nil {
		return EventEnvelope{}, err
	}
	return EventEnvelope{Sequence: event.Sequence, SessionID: event.SessionID, Event: encoded}, nil
}

// DecodeCommandPayload validates that a Wails string argument is one JSON
// object before it reaches the canonical command decoder.
func DecodeCommandPayload(value string) ([]byte, error) {
	payload := []byte(strings.TrimSpace(value))
	if len(payload) == 0 {
		return nil, invalidRequest(errors.New("command payload is required"))
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil {
		return nil, invalidRequest(fmt.Errorf("invalid JSON: %w", err))
	}
	if object == nil {
		return nil, invalidRequest(errors.New("command payload must be an object"))
	}
	return payload, nil
}
