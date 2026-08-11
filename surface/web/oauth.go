package web

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/cat3399/pi-go/internal/application"
)

type oauthPendingLogin struct {
	providerID string
	login      *application.ProviderOAuthLogin
}

type oauthBroker struct {
	mu      sync.Mutex
	pending map[string]oauthPendingLogin
}

func newOAuthBroker() *oauthBroker {
	return &oauthBroker{pending: make(map[string]oauthPendingLogin)}
}

func (broker *oauthBroker) add(providerID string, login *application.ProviderOAuthLogin) (string, error) {
	value := make([]byte, 24)
	if _, err := io.ReadFull(rand.Reader, value); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(value)
	broker.mu.Lock()
	broker.pending[token] = oauthPendingLogin{providerID: providerID, login: login}
	broker.mu.Unlock()
	return token, nil
}

func (broker *oauthBroker) get(token string) (oauthPendingLogin, bool) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	login, exists := broker.pending[token]
	return login, exists
}

func (broker *oauthBroker) remove(token string) {
	broker.mu.Lock()
	delete(broker.pending, token)
	broker.mu.Unlock()
}

func handleOAuthLoginStream(api application.API, broker *oauthBroker) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		flusher, ok := writer.(http.Flusher)
		if !ok {
			writeAPIError(writer, http.StatusInternalServerError, errors.New("streaming is unavailable"))
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache")
		writer.Header().Set("Connection", "keep-alive")
		writer.Header().Set("X-Accel-Buffering", "no")

		providerID := strings.TrimSpace(request.PathValue("provider"))
		login, err := api.StartProviderOAuth(request.Context(), providerID)
		if err != nil {
			_ = writeOAuthSSE(writer, map[string]any{"type": "error", "message": err.Error()})
			flusher.Flush()
			return
		}
		defer login.Close()
		token, err := broker.add(providerID, login)
		if err != nil {
			_ = writeOAuthSSE(writer, map[string]any{"type": "error", "message": "failed to create OAuth transaction"})
			flusher.Flush()
			return
		}
		defer broker.remove(token)
		if err := writeOAuthSSE(writer, map[string]any{
			"type": "auth", "url": login.URL, "instructions": login.Instructions, "token": token,
		}); err != nil {
			return
		}
		flusher.Flush()

		if err := login.Wait(request.Context()); err != nil {
			if request.Context().Err() == nil {
				_ = writeOAuthSSE(writer, map[string]any{"type": "error", "message": err.Error()})
				flusher.Flush()
			}
			return
		}
		_ = writeOAuthSSE(writer, map[string]any{"type": "success"})
		flusher.Flush()
	}
}

func handleOAuthLoginInput(broker *oauthBroker) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		body, err := readRequestBody(writer, request)
		if err != nil {
			writeRequestBodyError(writer, err)
			return
		}
		var input struct {
			Token string `json:"token"`
			Code  string `json:"code"`
		}
		if err := json.Unmarshal(body, &input); err != nil || strings.TrimSpace(input.Token) == "" || strings.TrimSpace(input.Code) == "" {
			writeAPIError(writer, http.StatusBadRequest, errors.New("token and code are required"))
			return
		}
		pending, exists := broker.get(input.Token)
		if !exists {
			writeAPIError(writer, http.StatusNotFound, errors.New("no pending login for token"))
			return
		}
		if pending.providerID != request.PathValue("provider") {
			writeAPIError(writer, http.StatusBadRequest, errors.New("token does not match provider"))
			return
		}
		if err := pending.login.Submit(input.Code); err != nil {
			writeAPIError(writer, http.StatusConflict, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"ok": true, "provider": pending.providerID})
	}
}

func handleOAuthLogout(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if err := api.DeleteProviderCredential(request.Context(), request.PathValue("provider"), "oauth"); err != nil {
			writeModelConfigurationError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"ok": true})
	}
}

func writeOAuthSSE(writer io.Writer, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = writer.Write(append(append([]byte("data: "), encoded...), '\n', '\n'))
	return err
}
