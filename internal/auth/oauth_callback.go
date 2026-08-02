package auth

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// ListenerFactory is injectable so tests can prove bind and lifecycle paths.
type ListenerFactory interface {
	Listen(network, address string) (net.Listener, error)
}
type systemListener struct{}

func (systemListener) Listen(network, address string) (net.Listener, error) {
	return net.Listen(network, address)
}

type callbackResult struct {
	code string
	err  error
}
type callbackWaiter struct {
	state     string
	listener  net.Listener
	server    *http.Server
	result    chan callbackResult
	once      sync.Once
	completed atomic.Bool
}

func startCallbackWaiter(host string, port int, state string, factory ListenerFactory) (*callbackWaiter, error) {
	if factory == nil {
		factory = systemListener{}
	}
	listener, err := factory.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, failure(KindOAuth, "bind OAuth callback listener", "openai", err)
	}
	w := &callbackWaiter{state: state, listener: listener, result: make(chan callbackResult, 1)}
	w.server = &http.Server{Handler: http.HandlerFunc(w.handle)}
	go func() {
		if err := w.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			w.finish(callbackResult{err: failure(KindOAuth, "serve OAuth callback", "openai", err)})
		}
	}()
	return w, nil
}

func (w *callbackWaiter) finish(result callbackResult) {
	w.once.Do(func() { w.completed.Store(true); w.result <- result })
}
func (w *callbackWaiter) Close() {
	if w == nil {
		return
	}
	_ = w.server.Close()
	_ = w.listener.Close()
}
func (w *callbackWaiter) handle(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	if request.Method != http.MethodGet || request.URL.Path != "/auth/callback" {
		response.WriteHeader(http.StatusNotFound)
		_, _ = response.Write([]byte(oauthPage("Authentication failed", "Callback route not found.")))
		return
	}
	if request.URL.Query().Get("state") != w.state {
		response.WriteHeader(http.StatusBadRequest)
		_, _ = response.Write([]byte(oauthPage("Authentication failed", "State mismatch.")))
		return
	}
	if request.URL.Query().Get("error") != "" {
		response.WriteHeader(http.StatusBadRequest)
		_, _ = response.Write([]byte(oauthPage("Authentication failed", "Authorization was rejected.")))
		w.finish(callbackResult{err: failure(KindOAuth, "receive OAuth callback", "openai", nil)})
		return
	}
	code := request.URL.Query().Get("code")
	if !validOAuthText(code) {
		response.WriteHeader(http.StatusBadRequest)
		_, _ = response.Write([]byte(oauthPage("Authentication failed", "Missing authorization code.")))
		return
	}
	if w.completed.Load() {
		response.WriteHeader(http.StatusGone)
		_, _ = response.Write([]byte(oauthPage("Authentication failed", "This login has already completed.")))
	} else {
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte(oauthPage("Authentication successful", "OpenAI authentication completed. You can close this window.")))
		w.finish(callbackResult{code: code})
	}
}

func oauthPage(title, message string) string {
	return "<!doctype html><html><head><meta charset=\"utf-8\"><title>" + htmlEscape(title) + "</title></head><body><main><h1>" + htmlEscape(title) + "</h1><p>" + htmlEscape(message) + "</p></main></body></html>"
}
func htmlEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&#39;")
	return replacer.Replace(value)
}
