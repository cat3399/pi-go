package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cat3399/pi-go/internal/host"
	"github.com/cat3399/pi-go/internal/hostjson"
	"github.com/cat3399/pi-go/internal/provider"
)

const maxAPIRequestBytes = 64 << 20

func registerAPIRoutes(mux *http.ServeMux, supervisor *Supervisor) {
	mux.HandleFunc("POST /api/agent/new", handleNewAgent(supervisor))
	mux.HandleFunc("GET /api/agent/running", handleRunningAgents(supervisor))
	mux.HandleFunc("GET /api/agent/{id}", handleAgentState(supervisor))
	mux.HandleFunc("POST /api/agent/{id}", handleAgentCommand(supervisor))
	mux.HandleFunc("GET /api/agent/{id}/events", handleAgentEvents(supervisor))

	mux.HandleFunc("GET /api/sessions", handleSessions(supervisor))
	mux.HandleFunc("GET /api/sessions/{id}", handleSessionView(supervisor))
	mux.HandleFunc("PATCH /api/sessions/{id}", handleSessionRename(supervisor))
	mux.HandleFunc("DELETE /api/sessions/{id}", unsupportedAPI)
	mux.HandleFunc("GET /api/sessions/{id}/context", handleSessionContext(supervisor))
	mux.HandleFunc("GET /api/sessions/{id}/state", handleSessionState(supervisor))

	mux.HandleFunc("GET /api/models", handleModels(supervisor))
	mux.HandleFunc("GET /api/home", handleHome)
	mux.HandleFunc("POST /api/cwd/validate", handleCWDValidation)
	mux.HandleFunc("POST /api/default-cwd", handleDefaultCWD)
	mux.HandleFunc("GET /api/project-trust", handleProjectTrust)
}

func readRequestBody(writer http.ResponseWriter, request *http.Request) ([]byte, error) {
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

func handleNewAgent(supervisor *Supervisor) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		body, err := readRequestBody(writer, request)
		if err != nil {
			writeAPIError(writer, http.StatusBadRequest, err)
			return
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(body, &object); err != nil {
			writeAPIError(writer, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
			return
		}
		cwd, err := requiredJSONText(object, "cwd")
		if err != nil {
			writeAPIError(writer, http.StatusBadRequest, err)
			return
		}
		commandType, err := requiredJSONText(object, "type")
		if err != nil {
			writeAPIError(writer, http.StatusBadRequest, err)
			return
		}
		providerID, err := optionalJSONText(object, "provider")
		if err != nil {
			writeAPIError(writer, http.StatusBadRequest, err)
			return
		}
		modelID, err := optionalJSONText(object, "modelId")
		if err != nil {
			writeAPIError(writer, http.StatusBadRequest, err)
			return
		}
		var toolNames []string
		_, hasToolNames := object["toolNames"]
		if hasToolNames {
			if err := json.Unmarshal(object["toolNames"], &toolNames); err != nil || toolNames == nil {
				writeAPIError(writer, http.StatusBadRequest, errors.New("toolNames must be an array of strings"))
				return
			}
		}
		var thinking *provider.ThinkingLevel
		if raw, exists := object["thinkingLevel"]; exists {
			var value string
			if json.Unmarshal(raw, &value) != nil {
				writeAPIError(writer, http.StatusBadRequest, errors.New("thinkingLevel must be a string"))
				return
			}
			level := provider.ThinkingLevel(value)
			if !level.Valid() {
				writeAPIError(writer, http.StatusBadRequest, fmt.Errorf("invalid thinking level: %s", value))
				return
			}
			thinking = &level
		}
		managed, state, err := supervisor.NewSession(request.Context(), NewSessionOptions{
			CWD: cwd, Provider: providerID, ModelID: modelID,
			ToolNames: toolNames, HasToolNames: hasToolNames, ThinkingLevel: thinking,
		})
		if err != nil {
			writeAPIError(writer, http.StatusInternalServerError, err)
			return
		}
		var data any
		if commandType != "ensure_session" {
			command, err := hostjson.DecodeCommand(body)
			if err != nil {
				writeAPIError(writer, http.StatusBadRequest, err)
				return
			}
			managedID, _, _ := managed.identity()
			result, err := supervisor.Dispatch(request.Context(), managedID, command)
			if err != nil {
				writeAPIError(writer, http.StatusInternalServerError, err)
				return
			}
			encoded, present, err := hostjson.EncodeResult(result)
			if err != nil {
				writeAPIError(writer, http.StatusInternalServerError, err)
				return
			}
			if present {
				data = encoded
			}
		}
		managedID, _, _ := managed.identity()
		writeJSON(writer, http.StatusOK, map[string]any{
			"success": true, "sessionId": managedID, "data": data,
			"model": stateModel(state), "thinkingLevel": state.ThinkingLevel,
		})
	}
}

func requiredJSONText(object map[string]json.RawMessage, name string) (string, error) {
	value, err := optionalJSONText(object, name)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func optionalJSONText(object map[string]json.RawMessage, name string) (string, error) {
	raw, exists := object[name]
	if !exists {
		return "", nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", fmt.Errorf("%s must be a string", name)
	}
	return strings.TrimSpace(value), nil
}

func handleAgentCommand(supervisor *Supervisor) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		body, err := readRequestBody(writer, request)
		if err != nil {
			writeAPIError(writer, http.StatusBadRequest, err)
			return
		}
		command, err := hostjson.DecodeCommand(body)
		if err != nil {
			writeAPIError(writer, http.StatusBadRequest, err)
			return
		}
		result, err := supervisor.Dispatch(request.Context(), request.PathValue("id"), command)
		if err != nil {
			writeSupervisorError(writer, err)
			return
		}
		data, present, err := hostjson.EncodeResult(result)
		if err != nil {
			writeAPIError(writer, http.StatusInternalServerError, err)
			return
		}
		if !present {
			data = nil
		}
		writeJSON(writer, http.StatusOK, map[string]any{"success": true, "data": data})
	}
}

func handleAgentState(supervisor *Supervisor) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		state, running, err := supervisor.State(request.Context(), request.PathValue("id"), false)
		if err != nil {
			writeSupervisorError(writer, err)
			return
		}
		if !running {
			writeJSON(writer, http.StatusOK, map[string]any{"running": false})
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"running": true, "state": hostjson.EncodeState(state)})
	}
}

func handleRunningAgents(supervisor *Supervisor) http.HandlerFunc {
	return func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]any{"runningSessionIds": supervisor.RunningIDs()})
	}
}

type sseItem struct {
	data any
	err  error
}

func handleAgentEvents(supervisor *Supervisor) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		flusher, ok := writer.(http.Flusher)
		if !ok {
			writeAPIError(writer, http.StatusInternalServerError, errors.New("streaming is unavailable"))
			return
		}
		events := make(chan sseItem, 256)
		unsubscribe, err := supervisor.Subscribe(request.Context(), request.PathValue("id"), func(ctx context.Context, event host.Event) {
			encoded, encodeErr := hostjson.EncodeEvent(event)
			if encodeErr == nil {
				encoded = eventForWebClient(encoded)
				if encoded == nil {
					return
				}
			}
			select {
			case events <- sseItem{data: encoded, err: encodeErr}:
			case <-ctx.Done():
			case <-request.Context().Done():
			}
		})
		if err != nil {
			writeSupervisorError(writer, err)
			return
		}
		defer unsubscribe()
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache")
		writer.Header().Set("Connection", "keep-alive")
		writer.Header().Set("X-Accel-Buffering", "no")
		if err := writeSSE(writer, map[string]any{"type": "connected", "sessionId": request.PathValue("id")}); err != nil {
			return
		}
		flusher.Flush()
		heartbeat := time.NewTicker(30 * time.Second)
		defer heartbeat.Stop()
		for {
			select {
			case item := <-events:
				if item.err != nil {
					_ = writeSSE(writer, map[string]any{"type": "protocol_error", "errorMessage": item.err.Error()})
					flusher.Flush()
					return
				}
				if err := writeSSE(writer, item.data); err != nil {
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

func eventForWebClient(value any) any {
	event, ok := value.(map[string]any)
	if !ok {
		return value
	}
	typeName, _ := event["type"].(string)
	switch typeName {
	case "turn_start", "turn_end", "tool_execution_update":
		return nil
	case "message_update":
		delete(event, "assistantMessageEvent")
	case "agent_end":
		return map[string]any{"type": "agent_end"}
	}
	return event
}

func writeSSE(writer io.Writer, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(writer, "data: %s\n\n", encoded)
	return err
}

func handleSessions(supervisor *Supervisor) http.HandlerFunc {
	return func(writer http.ResponseWriter, _ *http.Request) {
		sessions, err := supervisor.ListSessions()
		if err != nil {
			writeAPIError(writer, http.StatusInternalServerError, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{
			"sessions": sessions, "runningSessionIds": supervisor.RunningIDs(),
		})
	}
}

func handleSessionView(supervisor *Supervisor) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		view, err := supervisor.SessionView(
			request.PathValue("id"), "", query.Has("deferThinking"), query.Has("deferMedia"),
		)
		if err != nil {
			writeSupervisorError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, view)
	}
}

func handleSessionContext(supervisor *Supervisor) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		value, err := supervisor.SessionContext(
			request.PathValue("id"), query.Get("leafId"), query.Has("deferThinking"), query.Has("deferMedia"),
		)
		if err != nil {
			writeSupervisorError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"context": value})
	}
}

func handleSessionState(supervisor *Supervisor) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		id := request.PathValue("id")
		if _, found, err := supervisor.findSession(id); err != nil {
			writeAPIError(writer, http.StatusInternalServerError, err)
			return
		} else if !found {
			if _, active := supervisor.Active(id); !active {
				writeAPIError(writer, http.StatusNotFound, errors.New("session not found"))
				return
			}
		}
		state, running, err := supervisor.State(request.Context(), id, false)
		if err != nil {
			writeSupervisorError(writer, err)
			return
		}
		if !running {
			writeJSON(writer, http.StatusOK, map[string]any{"running": false})
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"running": true, "state": hostjson.EncodeState(state)})
	}
}

func handleSessionRename(supervisor *Supervisor) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		body, err := readRequestBody(writer, request)
		if err != nil {
			writeAPIError(writer, http.StatusBadRequest, err)
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
		if managed, active := supervisor.Active(id); active {
			if _, err := managed.host.Dispatch(request.Context(), host.SetSessionNameCommand{Name: name}); err != nil {
				writeSupervisorError(writer, err)
				return
			}
		} else {
			manager, _, _, closeManager, err := supervisor.sessionManagerForRead(id)
			if err != nil {
				writeSupervisorError(writer, err)
				return
			}
			if closeManager {
				defer manager.Close()
			}
			if _, err := manager.AppendSessionInfo(request.Context(), name); err != nil {
				writeAPIError(writer, http.StatusInternalServerError, err)
				return
			}
		}
		writeJSON(writer, http.StatusOK, map[string]any{"ok": true})
	}
}

func handleModels(supervisor *Supervisor) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		models, err := supervisor.Models(request.URL.Query().Get("cwd"))
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

func normalizeUserCWD(value string) (string, error) {
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
	return validateCWD(value)
}

func handleCWDValidation(writer http.ResponseWriter, request *http.Request) {
	body, err := readRequestBody(writer, request)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, err)
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
	writeJSON(writer, http.StatusOK, map[string]any{"success": true, "cwd": cwd})
}

func handleDefaultCWD(writer http.ResponseWriter, _ *http.Request) {
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

// Project resources and extensions are currently deferred, so the native Go
// WebUI does not execute untrusted project code during model/session startup.
func handleProjectTrust(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{
		"requiresTrust": false, "trusted": false, "projectResourcesLoaded": false,
	})
}

func writeSupervisorError(writer http.ResponseWriter, err error) {
	if errors.Is(err, os.ErrNotExist) {
		writeAPIError(writer, http.StatusNotFound, errors.New("session not found"))
		return
	}
	writeAPIError(writer, http.StatusInternalServerError, err)
}

func writeAPIError(writer http.ResponseWriter, status int, err error) {
	writeJSON(writer, status, map[string]any{"error": err.Error()})
}
