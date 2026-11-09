package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/session"
)

var contextCLIInstructions = agent.ContextAccessInstructions{
	Handoff: `Use bash to run "${PI_EXECUTABLE}" session context new --handoff-stdin and supply the handoff on stdin (for example, with a quoted heredoc). Include current progress, relevant files and the next direction. This saves the handoff and switches windows after the current tool batch; continue the same task afterward.`,
	History: `Original history is available through bash. Search with "${PI_EXECUTABLE}" session history search 'query'; read a matching record with "${PI_EXECUTABLE}" session history read --entry ID. Use --view raw to include tool calls and results, or session history --help for window/range options. Read only what you need to continue.`,
}

type sessionControlRequest struct {
	SessionID string                     `json:"sessionId"`
	Action    string                     `json:"action"`
	Query     session.HistoryViewQuery   `json:"query,omitempty"`
	Change    agent.ContextChangeRequest `json:"change,omitempty"`
}

type sessionControlResponse struct {
	Status  *agent.ContextStatus       `json:"status,omitempty"`
	Change  *agent.ContextChangeResult `json:"change,omitempty"`
	History *session.HistoryView       `json:"history,omitempty"`
	Info    *application.SessionInfo   `json:"info,omitempty"`
	Error   string                     `json:"error,omitempty"`
}

// AttachSession mounts a CLI-only wire protocol over the typed Application
// boundary. The loopback listener is capability-scoped to this session and is
// closed by the owning AgentSession on replacement or shutdown.
func AttachSession(current *agent.AgentSession, agentDir string) error {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return err
	}
	token := hex.EncodeToString(secret[:])
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("start session CLI access: %w", err)
	}
	server := &http.Server{Handler: sessionControlHandler(application.BorrowSessionContext(current), token), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	if err := current.ConfigureContextAccess(contextCLIInstructions, map[string]string{
		"PI_SESSION_ENDPOINT":  "http://" + listener.Addr().String() + "/session",
		"PI_SESSION_TOKEN":     token,
		"PI_SESSION_AGENT_DIR": agentDir,
	}, server); err != nil {
		_ = listener.Close()
		return err
	}
	go func() { _ = server.Serve(listener) }()
	return nil
}

func sessionControlHandler(access application.SessionContextAccess, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/session" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Origin") != "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "session access denied", http.StatusUnauthorized)
			return
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
		decoder.DisallowUnknownFields()
		var request sessionControlRequest
		if err := decoder.Decode(&request); err != nil {
			http.Error(w, "invalid session request", http.StatusBadRequest)
			return
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			http.Error(w, "invalid trailing request", http.StatusBadRequest)
			return
		}
		if request.SessionID != access.SessionID() {
			http.Error(w, "session ID does not match this endpoint", http.StatusConflict)
			return
		}
		var response sessionControlResponse
		var err error
		switch request.Action {
		case "info":
			info := access.Info()
			response.Info = &info
		case "history":
			value, readErr := access.ReadHistoryView(request.Query)
			response.History, err = &value, readErr
		case "status", "change":
			var command application.Command = application.ContextStatusCommand{}
			if request.Action == "change" {
				command = application.ContextChangeCommand{Request: request.Change}
			}
			var result application.CommandResult
			result, err = access.Dispatch(r.Context(), command)
			switch result := result.(type) {
			case application.ContextStatusResult:
				response.Status = &result.Status
			case application.ContextChangeResult:
				response.Change = &result.Result
			}
		default:
			err = fmt.Errorf("unknown session action %q", request.Action)
		}
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			response.Error = err.Error()
			w.WriteHeader(http.StatusBadRequest)
		}
		_ = json.NewEncoder(w).Encode(response)
	})
}

func callSessionControl(ctx context.Context, endpoint, token string, request sessionControlRequest) (sessionControlResponse, error) {
	address, err := url.Parse(endpoint)
	if err != nil || address.Scheme != "http" || address.User != nil || address.RawQuery != "" || address.Fragment != "" || address.Path != "/session" || net.ParseIP(address.Hostname()) == nil || !net.ParseIP(address.Hostname()).IsLoopback() {
		return sessionControlResponse{}, fmt.Errorf("session endpoint must be a loopback HTTP /session endpoint")
	}
	if token == "" {
		return sessionControlResponse{}, fmt.Errorf("PI_SESSION_TOKEN is required for live session access")
	}
	body, err := json.Marshal(request)
	if err != nil {
		return sessionControlResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address.String(), bytes.NewReader(body))
	if err != nil {
		return sessionControlResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return sessionControlResponse{}, fmt.Errorf("connect to current session: %w", err)
	}
	defer response.Body.Close()
	var result sessionControlResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return result, fmt.Errorf("session endpoint returned HTTP %d: %w", response.StatusCode, err)
	}
	if result.Error != "" {
		return result, errors.New(result.Error)
	}
	if response.StatusCode != http.StatusOK {
		return result, fmt.Errorf("session endpoint returned HTTP %d", response.StatusCode)
	}
	if request.Action == "history" && result.History == nil || request.Action == "status" && result.Status == nil || request.Action == "change" && result.Change == nil || request.Action == "info" && result.Info == nil {
		return result, fmt.Errorf("session endpoint omitted the %s result", request.Action)
	}
	return result, nil
}
