package auth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	callbackReadHeaderTimeout = 5 * time.Second
	callbackMaxHeaderBytes    = 8 << 10
	callbackMaxQueryBytes     = 8 << 10
	callbackMaxCodeBytes      = 4 << 10
	callbackMaxErrorBytes     = 512
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
	state    string
	listener net.Listener
	server   *http.Server
	result   chan callbackResult
	once     sync.Once
	closed   chan struct{}
	hosts    map[string]struct{}
}

func startCallbackWaiter(host string, port int, state string, factory ListenerFactory) (*callbackWaiter, error) {
	if factory == nil {
		factory = systemListener{}
	}
	listener, err := factory.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, failure(KindOAuth, "bind OAuth callback listener", "openai", err)
	}
	w := &callbackWaiter{
		state: state, listener: listener, result: make(chan callbackResult, 1), closed: make(chan struct{}),
		hosts: map[string]struct{}{net.JoinHostPort("localhost", strconv.Itoa(port)): {}, net.JoinHostPort(host, strconv.Itoa(port)): {}},
	}
	w.server = &http.Server{Handler: http.HandlerFunc(w.handle), ReadHeaderTimeout: callbackReadHeaderTimeout, MaxHeaderBytes: callbackMaxHeaderBytes}
	go func() {
		if err := w.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			w.finish(callbackResult{err: failure(KindOAuth, "serve OAuth callback", "openai", err)})
		}
	}()
	return w, nil
}

func (w *callbackWaiter) finish(result callbackResult) bool {
	won := false
	w.once.Do(func() { won = true; w.result <- result; close(w.closed); _ = w.listener.Close() })
	return won
}
func (w *callbackWaiter) Close() {
	if w == nil {
		return
	}
	w.finish(callbackResult{err: failure(KindCancelled, "cancel OAuth callback", "openai", context.Canceled)})
	_ = w.server.Close()
	_ = w.listener.Close()
}
func (w *callbackWaiter) handle(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	if request.Method != http.MethodGet || request.URL.Path != "/auth/callback" || request.URL.RawPath != "" {
		response.WriteHeader(http.StatusNotFound)
		_, _ = response.Write([]byte(oauthPage("Authentication failed", "Callback route not found.")))
		return
	}
	if _, ok := w.hosts[request.Host]; !ok {
		response.WriteHeader(http.StatusBadRequest)
		_, _ = response.Write([]byte(oauthPage("Authentication failed", "Invalid callback host.")))
		return
	}
	if len(request.URL.RawQuery) > callbackMaxQueryBytes {
		response.WriteHeader(http.StatusRequestURITooLong)
		return
	}
	query := request.URL.Query()
	for _, field := range []string{"state", "code", "error", "error_description"} {
		if len(query[field]) > 1 {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
	}
	if len(query.Get("state")) > 64 || len(query.Get("code")) > callbackMaxCodeBytes || len(query.Get("error")) > callbackMaxErrorBytes || len(query.Get("error_description")) > callbackMaxErrorBytes {
		response.WriteHeader(http.StatusBadRequest)
		return
	}
	if query.Get("state") != w.state {
		response.WriteHeader(http.StatusBadRequest)
		_, _ = response.Write([]byte(oauthPage("Authentication failed", "State mismatch.")))
		return
	}
	if query.Get("error") != "" {
		response.WriteHeader(http.StatusBadRequest)
		_, _ = response.Write([]byte(oauthPage("Authentication failed", "Authorization was rejected.")))
		w.finish(callbackResult{err: failure(KindOAuth, "receive OAuth callback", "openai", nil)})
		return
	}
	code := query.Get("code")
	if !validOAuthText(code) {
		response.WriteHeader(http.StatusBadRequest)
		_, _ = response.Write([]byte(oauthPage("Authentication failed", "Missing authorization code.")))
		return
	}
	if !w.finish(callbackResult{code: code}) {
		response.WriteHeader(http.StatusGone)
		_, _ = response.Write([]byte(oauthPage("Authentication failed", "This login has already completed.")))
	} else {
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte(oauthPage("Authentication successful", "OpenAI authentication completed. You can close this window.")))
	}
}

func oauthPage(title, message string) string {
	return "<!doctype html><html><head><meta charset=\"utf-8\"><title>" + htmlEscape(title) + "</title></head><body><main><h1>" + htmlEscape(title) + "</h1><p>" + htmlEscape(message) + "</p></main></body></html>"
}
func htmlEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&#39;")
	return replacer.Replace(value)
}
