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
	state       string
	listener    net.Listener
	server      *http.Server
	result      chan callbackResult
	closed      chan struct{}
	hosts       map[string]struct{}
	mu          sync.Mutex
	claimed     bool
	published   bool
	publishDone chan struct{}
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
		state: state, listener: listener, result: make(chan callbackResult, 1), closed: make(chan struct{}), publishDone: make(chan struct{}),
		hosts: map[string]struct{}{net.JoinHostPort("localhost", strconv.Itoa(port)): {}, net.JoinHostPort(host, strconv.Itoa(port)): {}},
	}
	w.server = &http.Server{Handler: http.HandlerFunc(w.handle), ReadHeaderTimeout: callbackReadHeaderTimeout, MaxHeaderBytes: callbackMaxHeaderBytes}
	go func() {
		if err := w.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			if w.claim() {
				w.publish(callbackResult{err: failure(KindOAuth, "serve OAuth callback", "openai", err)})
			}
		}
	}()
	return w, nil
}

func (w *callbackWaiter) claim() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.claimed {
		return false
	}
	w.claimed = true
	return true
}
func (w *callbackWaiter) publish(result callbackResult) {
	w.mu.Lock()
	if w.published {
		w.mu.Unlock()
		return
	}
	w.published = true
	w.result <- result
	close(w.closed)
	close(w.publishDone)
	w.mu.Unlock()
	_ = w.listener.Close()
}
func (w *callbackWaiter) Close() {
	if w == nil {
		return
	}
	if w.claim() {
		w.publish(callbackResult{err: failure(KindCancelled, "cancel OAuth callback", "openai", context.Canceled)})
	}
	<-w.publishDone
	_ = w.server.Close()
	_ = w.listener.Close()
}
func (w *callbackWaiter) handle(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet || request.URL.Path != "/auth/callback" || request.URL.RawPath != "" {
		_ = writeCallbackPage(response, http.StatusNotFound, "Authentication failed", "Callback route not found.")
		return
	}
	if _, ok := w.hosts[request.Host]; !ok {
		_ = writeCallbackPage(response, http.StatusBadRequest, "Authentication failed", "Invalid callback host.")
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
		_ = writeCallbackPage(response, http.StatusBadRequest, "Authentication failed", "State mismatch.")
		return
	}
	if query.Get("error") != "" {
		if !w.claim() {
			_ = writeCallbackPage(response, http.StatusGone, "Authentication failed", "This login has already completed.")
			return
		}
		if err := writeCallbackPage(response, http.StatusBadRequest, "Authentication failed", "Authorization was rejected."); err != nil {
			w.publish(callbackResult{err: failure(KindIO, "write OAuth callback", "openai", err)})
			return
		}
		w.publish(callbackResult{err: failure(KindOAuth, "receive OAuth callback", "openai", nil)})
		return
	}
	code := query.Get("code")
	if !validOAuthText(code) {
		_ = writeCallbackPage(response, http.StatusBadRequest, "Authentication failed", "Missing authorization code.")
		return
	}
	if !w.claim() {
		_ = writeCallbackPage(response, http.StatusGone, "Authentication failed", "This login has already completed.")
		return
	}
	if err := writeCallbackPage(response, http.StatusOK, "Authentication successful", "OpenAI authentication completed. You can close this window."); err != nil {
		w.publish(callbackResult{err: failure(KindIO, "write OAuth callback", "openai", err)})
		return
	}
	w.publish(callbackResult{code: code})
}

func writeCallbackPage(response http.ResponseWriter, status int, title, message string) error {
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.WriteHeader(status)
	if _, err := response.Write([]byte(oauthPage(title, message))); err != nil {
		return err
	}
	flusher, ok := response.(http.Flusher)
	if !ok {
		return errors.New("callback response cannot be flushed")
	}
	flusher.Flush()
	return nil
}

func oauthPage(title, message string) string {
	return "<!doctype html><html><head><meta charset=\"utf-8\"><title>" + htmlEscape(title) + "</title></head><body><main><h1>" + htmlEscape(title) + "</h1><p>" + htmlEscape(message) + "</p></main></body></html>"
}
func htmlEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&#39;")
	return replacer.Replace(value)
}
