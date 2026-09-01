package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cat3399/pi-go/internal/application"
	protocolv1 "github.com/cat3399/pi-go/internal/protocol/v1"
	"github.com/cat3399/pi-go/internal/surfacewire"
)

const maxAPIRequestBytes = 64 << 20

var errJSONContentType = errors.New("Content-Type must be application/json")

func registerAPIRoutes(mux *http.ServeMux, api application.API) {
	oauth := newOAuthBroker()
	mux.HandleFunc("GET /api/v1/snapshot", handleApplicationSnapshot(api))
	mux.HandleFunc("GET /api/v1/events", handleApplicationEvents(api))

	mux.HandleFunc("GET /api/v1/sessions", handleSessions(api))
	mux.HandleFunc("POST /api/v1/sessions", handleCreateSession(api))
	mux.HandleFunc("GET /api/v1/sessions/running", handleRunningSessions(api))
	mux.HandleFunc("GET /api/v1/sessions/{id}", handleSessionView(api))
	mux.HandleFunc("PATCH /api/v1/sessions/{id}", handleSessionRename(api))
	mux.HandleFunc("DELETE /api/v1/sessions/{id}", handleSessionDelete(api))
	mux.HandleFunc("GET /api/v1/sessions/{id}/export", handleSessionExport(api))
	mux.HandleFunc("POST /api/v1/sessions/{id}/auto-name", handleSessionAutoName(api))
	mux.HandleFunc("GET /api/v1/sessions/{id}/context", handleSessionContext(api))
	mux.HandleFunc("GET /api/v1/sessions/{id}/state", handleSessionState(api))
	mux.HandleFunc("POST /api/v1/sessions/{id}/commands", handleSessionCommand(api))
	mux.HandleFunc("GET /api/v1/sessions/{id}/entries/{entryId}/thinking", handleSessionThinking(api))
	mux.HandleFunc("GET /api/v1/sessions/{id}/bash-output", handleSessionBashOutput(api))
	mux.HandleFunc("POST /api/v1/projects", handleProjectAdd(api))
	mux.HandleFunc("DELETE /api/v1/projects", handleProjectRemove(api))

	mux.HandleFunc("GET /api/v1/models", handleModels(api))
	mux.HandleFunc("GET /api/v1/models-config", handleModelsConfigRead(api))
	mux.HandleFunc("PUT /api/v1/models-config", handleModelsConfigWrite(api))
	mux.HandleFunc("POST /api/v1/models-config/discover", handleModelDiscovery(api))
	mux.HandleFunc("GET /api/v1/models-config/catalog", handleModelCatalog(api))
	mux.HandleFunc("POST /api/v1/models-config/test", handleModelProbe(api))
	mux.HandleFunc("GET /api/v1/auth/providers", handleOAuthProviders(api))
	mux.HandleFunc("GET /api/v1/auth/all-providers", handleAPIKeyProviders(api))
	mux.HandleFunc("GET /api/v1/auth/api-key/{provider}", handleAPIKeyStatus(api))
	mux.HandleFunc("POST /api/v1/auth/api-key/{provider}", handleSetAPIKey(api))
	mux.HandleFunc("DELETE /api/v1/auth/api-key/{provider}", handleDeleteAPIKey(api))
	mux.HandleFunc("GET /api/v1/auth/login/{provider}", handleOAuthLoginStream(api, oauth))
	mux.HandleFunc("POST /api/v1/auth/login/{provider}", handleOAuthLoginInput(oauth))
	mux.HandleFunc("POST /api/v1/auth/logout/{provider}", handleOAuthLogout(api))
	mux.HandleFunc("GET /api/v1/skills", handleSkills(api))
	mux.HandleFunc("PATCH /api/v1/skills", handleSkillToggle(api))
	mux.HandleFunc("POST /api/v1/skills/search", handleSkillSearch(api))
	mux.HandleFunc("POST /api/v1/skills/install", handleSkillInstall(api))
	mux.HandleFunc("POST /api/v1/skills/check", handleSkillCheck(api))
	mux.HandleFunc("POST /api/v1/skills/update", handleSkillUpdate(api))
	mux.HandleFunc("GET /api/v1/system/home", handleHome)
	mux.HandleFunc("GET /api/v1/system/cwd/browse", handleDirectoryBrowse(api))
	mux.HandleFunc("POST /api/v1/system/cwd/validate", handleCWDValidation)
	mux.HandleFunc("POST /api/v1/system/default-cwd", handleDefaultCWD)
	mux.HandleFunc("GET /api/v1/system/project-trust", handleProjectTrust(api))
	mux.HandleFunc("POST /api/v1/system/project-trust", handleTrustProject(api))
	mux.HandleFunc("GET /api/v1/worktrees", handleWorktreeList(api))
	mux.HandleFunc("POST /api/v1/worktrees", handleWorktreeAdd(api))
	mux.HandleFunc("DELETE /api/v1/worktrees", handleWorktreeRemove(api))
	mux.HandleFunc("GET /api/v1/files/{path...}", handleFileGet(api))
	mux.HandleFunc("POST /api/v1/files/{path...}", handleFileUpload(api))
	mux.HandleFunc("DELETE /api/v1/files/{path...}", handleFileDelete(api))
	mux.HandleFunc("GET /api/v1/file-index", handleFileIndex(api))
	mux.HandleFunc("GET /api/v1/git/status", handleGitStatus(api))
	mux.HandleFunc("GET /api/v1/git/diff", handleGitDiff(api))
}

func readRequestBody(writer http.ResponseWriter, request *http.Request) ([]byte, error) {
	if !hasJSONContentType(request) {
		return nil, errJSONContentType
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maxAPIRequestBytes)
	data, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("request body is required")
	}
	return data, nil
}

func hasJSONContentType(request *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil {
		return false
	}
	mediaType = strings.ToLower(mediaType)
	return mediaType == "application/json" || strings.HasPrefix(mediaType, "application/") && strings.HasSuffix(mediaType, "+json")
}

func writeRequestBodyError(writer http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, errJSONContentType) {
		status = http.StatusUnsupportedMediaType
	} else {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
	}
	writeAPIError(writer, status, err)
}

func handleCreateSession(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		body, err := readRequestBody(writer, request)
		if err != nil {
			writeRequestBodyError(writer, err)
			return
		}
		var input surfacewire.CreateSessionRequest
		if err := json.Unmarshal(body, &input); err != nil {
			writeAPIError(writer, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
			return
		}
		result, err := surfacewire.CreateSession(request.Context(), api, input)
		if err != nil {
			if errors.Is(err, surfacewire.ErrInvalidRequest) {
				writeAPIError(writer, http.StatusBadRequest, err)
				return
			}
			writeAPIError(writer, http.StatusInternalServerError, err)
			return
		}
		writeJSON(writer, http.StatusCreated, result)
	}
}

func handleSessionCommand(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		body, err := readRequestBody(writer, request)
		if err != nil {
			writeRequestBodyError(writer, err)
			return
		}
		result, err := surfacewire.DispatchJSON(request.Context(), api, request.PathValue("id"), body, "interactive")
		if err != nil {
			if errors.Is(err, surfacewire.ErrInvalidRequest) {
				writeAPIError(writer, http.StatusBadRequest, err)
				return
			}
			writeApplicationError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, result)
	}
}

func handleRunningSessions(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]any{"runningSessionIds": api.RunningIDs()})
	}
}

func handleApplicationEvents(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		flusher, ok := writer.(http.Flusher)
		if !ok {
			writeAPIError(writer, http.StatusInternalServerError, errors.New("streaming is unavailable"))
			return
		}
		after, hasCursor, err := eventCursor(request)
		if err != nil {
			writeAPIError(writer, http.StatusBadRequest, err)
			return
		}
		if !hasCursor {
			after = api.CurrentRevision()
		}
		subscription, err := api.SubscribeEvents(after)
		reset := errors.Is(err, application.ErrEventCursorUnavailable)
		if reset {
			after = api.CurrentRevision()
			subscription, err = api.SubscribeEvents(after)
		}
		if err != nil {
			writeApplicationError(writer, err)
			return
		}
		defer subscription.Close()
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache")
		writer.Header().Set("Connection", "keep-alive")
		writer.Header().Set("X-Accel-Buffering", "no")
		if reset {
			if err := writeSSE(writer, after, map[string]any{"type": "reset_required", "revision": after}); err != nil {
				return
			}
		}
		for _, event := range subscription.Replay {
			if err := writeApplicationSSE(writer, event); err != nil {
				return
			}
		}
		if err := writeSSE(writer, subscription.Revision, map[string]any{"type": "connected", "revision": subscription.Revision}); err != nil {
			return
		}
		flusher.Flush()
		heartbeat := time.NewTicker(30 * time.Second)
		defer heartbeat.Stop()
		for {
			select {
			case event, ok := <-subscription.Events:
				if !ok {
					return
				}
				if err := writeApplicationSSE(writer, event); err != nil {
					return
				}
				flusher.Flush()
			case <-heartbeat.C:
				if _, err := io.WriteString(writer, ":\n\n"); err != nil {
					return
				}
				flusher.Flush()
			case <-request.Context().Done():
				return
			}
		}
	}
}

func writeApplicationSSE(writer io.Writer, event application.Event) error {
	envelope, err := surfacewire.EncodeEvent(event)
	if err != nil {
		envelope = surfacewire.EventEnvelope{
			Sequence: event.Sequence, SessionID: event.SessionID,
			Event: map[string]any{"type": "protocol_error", "errorMessage": err.Error()},
		}
	}
	return writeSSE(writer, event.Sequence, envelope)
}

func eventCursor(request *http.Request) (uint64, bool, error) {
	// `after` establishes the first connection from an authoritative snapshot.
	// Native EventSource reconnects with Last-Event-ID, which must take
	// precedence over that original query parameter or every reconnect would
	// replay from the stale snapshot cursor.
	value := strings.TrimSpace(request.URL.Query().Get("after"))
	if lastEventID := strings.TrimSpace(request.Header.Get("Last-Event-ID")); lastEventID != "" {
		value = lastEventID
	}
	if value == "" {
		return 0, false, nil
	}
	cursor, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, false, errors.New("event cursor must be an unsigned integer")
	}
	return cursor, true, nil
}

func writeSSE(writer io.Writer, sequence uint64, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(writer, "id: %d\ndata: %s\n\n", sequence, encoded)
	return err
}

func handleApplicationSnapshot(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, _ *http.Request) {
		snapshot, err := surfacewire.Snapshot(api)
		if err != nil {
			writeAPIError(writer, http.StatusInternalServerError, err)
			return
		}
		writeJSON(writer, http.StatusOK, snapshot)
	}
}

func handleSessions(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, _ *http.Request) {
		sessions, err := listSessions(api)
		if err != nil {
			writeAPIError(writer, http.StatusInternalServerError, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{
			"sessions": sessions, "runningSessionIds": api.RunningIDs(),
		})
	}
}

func handleSessionView(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		view, err := sessionView(api,
			request.PathValue("id"), query.Get("leafId"), query.Has("deferThinking"), query.Has("deferMedia"),
		)
		if err != nil {
			writeApplicationError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, view)
	}
}

func handleSessionContext(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		value, err := sessionContext(api,
			request.PathValue("id"), query.Get("leafId"), query.Has("deferThinking"), query.Has("deferMedia"),
		)
		if err != nil {
			writeApplicationError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"context": value})
	}
}

func handleSessionState(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		id := request.PathValue("id")
		if found, err := api.SessionExists(id); err != nil {
			writeAPIError(writer, http.StatusInternalServerError, err)
			return
		} else if !found {
			writeAPIError(writer, http.StatusNotFound, errors.New("session not found"))
			return
		}
		state, running, err := api.LiveState(id)
		if err != nil {
			writeApplicationError(writer, err)
			return
		}
		if !running {
			writeJSON(writer, http.StatusOK, map[string]any{"running": false})
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"running": true, "state": protocolv1.EncodeState(state)})
	}
}

func handleSessionRename(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		body, err := readRequestBody(writer, request)
		if err != nil {
			writeRequestBodyError(writer, err)
			return
		}
		var input struct {
			Name *string `json:"name"`
		}
		if json.Unmarshal(body, &input) != nil || input.Name == nil {
			writeAPIError(writer, http.StatusBadRequest, errors.New("name is required"))
			return
		}
		name := strings.TrimSpace(*input.Name)
		if name == "" {
			writeAPIError(writer, http.StatusBadRequest, errors.New("name cannot be empty"))
			return
		}
		id := request.PathValue("id")
		if err := surfacewire.RenameSession(request.Context(), api, id, name); err != nil {
			writeApplicationError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"ok": true})
	}
}

func handleModels(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		models, err := models(request.Context(), api, request.URL.Query().Get("cwd"))
		if err != nil {
			writeAPIError(writer, http.StatusBadRequest, err)
			return
		}
		writeJSON(writer, http.StatusOK, models)
	}
}

func handleHome(writer http.ResponseWriter, _ *http.Request) {
	home, err := os.UserHomeDir()
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"home": home})
}

func handleDirectoryBrowse(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		result, err := surfacewire.BrowseDirectories(request.Context(), api, request.URL.Query().Get("path"))
		if err != nil {
			switch {
			case errors.Is(err, application.ErrDirectoryNotFound):
				writeAPIError(writer, http.StatusNotFound, err)
			case errors.Is(err, application.ErrPathNotDirectory):
				writeAPIError(writer, http.StatusBadRequest, err)
			default:
				writeApplicationError(writer, err)
			}
			return
		}
		writeJSON(writer, http.StatusOK, result)
	}
}

func normalizeUserCWD(value string) (string, error) {
	return surfacewire.NormalizeUserCWD(value)
}

func handleCWDValidation(writer http.ResponseWriter, request *http.Request) {
	body, err := readRequestBody(writer, request)
	if err != nil {
		writeRequestBodyError(writer, err)
		return
	}
	var input struct {
		CWD string `json:"cwd"`
	}
	if json.Unmarshal(body, &input) != nil {
		writeAPIError(writer, http.StatusBadRequest, errors.New("cwd must be a string"))
		return
	}
	cwd, err := normalizeUserCWD(input.CWD)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"cwd": cwd})
}

func handleDefaultCWD(writer http.ResponseWriter, request *http.Request) {
	if !hasJSONContentType(request) {
		writeAPIError(writer, http.StatusUnsupportedMediaType, errJSONContentType)
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, err)
		return
	}
	directory := filepath.Join(home, "pi-cwd-"+time.Now().UTC().Format("20060102"))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		writeAPIError(writer, http.StatusInternalServerError, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"cwd": directory})
}

func writeApplicationError(writer http.ResponseWriter, err error) {
	if errors.Is(err, os.ErrNotExist) {
		writeAPIError(writer, http.StatusNotFound, errors.New("session not found"))
		return
	}
	writeAPIError(writer, http.StatusInternalServerError, err)
}

func writeAPIError(writer http.ResponseWriter, status int, err error) {
	writeJSON(writer, status, map[string]any{"error": err.Error()})
}
