package provider

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
	"sync"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
)

// streamDiagnostics captures the bytes received by the adapter, before JSON
// parsing or repair. Only failed attempts become durable diagnostic records.
// Temporary captures keep memory bounded even for long responses.
type streamDiagnostics struct {
	mu      sync.Mutex
	dir     string
	session string
	model   Model
	payload []byte
	attempt uint32
	current *responseCapture
	closed  bool
}

type responseCapture struct {
	mu     sync.Mutex
	file   *os.File
	record failureRecord
	err    error
	done   bool
	diag   []llm.AssistantDiagnostic
}

type failureRecord struct {
	Timestamp    time.Time `json:"timestamp"`
	SessionID    string    `json:"sessionId,omitempty"`
	Provider     string    `json:"provider"`
	API          string    `json:"api"`
	Model        string    `json:"model"`
	Endpoint     string    `json:"endpoint,omitempty"`
	Attempt      uint32    `json:"attempt"`
	Request      []byte    `json:"requestBase64"`
	HTTPStatus   int       `json:"httpStatus,omitempty"`
	ContentType  string    `json:"contentType,omitempty"`
	RequestID    string    `json:"requestId,omitempty"`
	Encoding     string    `json:"responseEncoding"`
	ResponsePath string    `json:"responsePath,omitempty"`
	Bytes        int64     `json:"responseBytes"`
	EOF          bool      `json:"responseEOF"`
	ReadError    string    `json:"readError,omitempty"`
	Kind         string    `json:"kind"`
	Message      string    `json:"message"`
	Cause        string    `json:"cause,omitempty"`
	VendorCode   string    `json:"vendorCode,omitempty"`
	CaptureError string    `json:"captureError,omitempty"`
}

func newStreamDiagnostics(options StreamOptions, model Model, payload []byte) *streamDiagnostics {
	if options.DiagnosticsDir == "" {
		return nil
	}
	return &streamDiagnostics{dir: options.DiagnosticsDir, session: options.SessionID, model: model, payload: payload}
}

type responseCaptureKey struct{}

func (d *streamDiagnostics) begin(ctx context.Context, endpoint string) context.Context {
	if d == nil {
		return ctx
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return ctx
	}
	d.attempt++
	c := &responseCapture{record: failureRecord{
		Timestamp: time.Now().UTC(), SessionID: d.session, Provider: d.model.Provider(),
		API: d.model.API(), Model: d.model.ID(), Endpoint: endpoint, Attempt: d.attempt, Request: d.payload,
		Encoding: "raw",
	}}
	c.err = os.MkdirAll(d.dir, 0700)
	if c.err == nil {
		c.file, c.err = os.CreateTemp(d.dir, ".response-*")
	}
	d.current = c
	return context.WithValue(ctx, responseCaptureKey{}, c)
}

func captureFromContext(ctx context.Context) *responseCapture {
	c, _ := ctx.Value(responseCaptureKey{}).(*responseCapture)
	return c
}

func captureResponse(ctx context.Context, response *http.Response) {
	c := captureFromContext(ctx)
	if c == nil || response == nil || response.Body == nil || isTypedNil(response.Body) {
		return
	}
	// This body is already recorded as original WebSocket messages.
	if _, websocket := response.Body.(*codexWebSocketBody); websocket {
		return
	}
	c.mu.Lock()
	c.record.HTTPStatus = response.StatusCode
	c.record.ContentType = response.Header.Get("Content-Type")
	c.record.RequestID = firstNonBlank(response.Header.Get("x-request-id"), response.Header.Get("request-id"))
	c.mu.Unlock()
	if response.StatusCode != http.StatusSwitchingProtocols {
		response.Body = &capturedResponseBody{ReadCloser: response.Body, capture: c}
	}
}

// The WebSocket library retains only 1024 bytes of a rejected handshake body.
// Capture it at the HTTP boundary before handing that same prefix to Dial.
// Successful upgrades must retain their original ReadWriteCloser.
type diagnosticHandshakeTransport struct{ next http.RoundTripper }

func (t diagnosticHandshakeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.next.RoundTrip(request)
	if err != nil || response == nil || captureFromContext(request.Context()) == nil {
		return response, err
	}
	captureResponse(request.Context(), response)
	if response.StatusCode != http.StatusSwitchingProtocols && response.Body != nil {
		prefix, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		drainDiagnosticResponse(response)
		_ = response.Body.Close()
		response.Body = io.NopCloser(bytes.NewReader(prefix))
	}
	return response, nil
}

func diagnosticWebSocketClient(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	copy := *client
	next := copy.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	copy.Transport = diagnosticHandshakeTransport{next: next}
	return &copy
}

func (c *responseCapture) request(data []byte) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record.Request = bytes.Clone(data)
}

type capturedResponseBody struct {
	io.ReadCloser
	capture *responseCapture
}

func (b *capturedResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.capture.write(p[:n], err, false)
	return n, err
}

func (c *responseCapture) write(data []byte, readErr error, websocket bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		return
	}
	if websocket {
		c.record.Encoding = "websocket-jsonl-base64"
		// JSON's []byte encoding preserves arbitrary bytes and frame boundaries.
		if len(data) != 0 {
			data, _ = json.Marshal(struct {
				Data []byte `json:"data"`
			}{data})
			data = append(data, '\n')
		}
	}
	if c.file != nil && c.err == nil && len(data) != 0 {
		n, err := c.file.Write(data)
		c.record.Bytes += int64(n)
		c.err = err
	}
	if errors.Is(readErr, io.EOF) {
		c.record.EOF = true
	} else if readErr != nil {
		c.record.ReadError = readErr.Error()
	}
}

// Drain only rejected HTTP responses, so the diagnostic contains the whole
// body even when the parser only needs a bounded prefix. The request context
// still bounds the read. For broken streams we retain exactly what arrived.
func drainDiagnosticResponse(response *http.Response) {
	if _, captured := response.Body.(*capturedResponseBody); captured {
		_, _ = io.Copy(io.Discard, response.Body)
	}
}

func (d *streamDiagnostics) failure(kind FailureKind, message string, cause error, code string) []llm.AssistantDiagnostic {
	if d == nil {
		return nil
	}
	if message == "" {
		message = safeResponsesErrorText(cause, "Model request failed")
	}
	d.mu.Lock()
	c := d.current
	d.mu.Unlock()
	if c == nil {
		d.begin(context.Background(), "")
		d.mu.Lock()
		c = d.current
		d.mu.Unlock()
	}
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		return c.diag
	}
	c.done = true
	c.record.Kind, c.record.Message, c.record.VendorCode = kind.String(), message, code
	if cause != nil {
		c.record.Cause = safeResponsesErrorText(cause, "Unable to format provider error")
	}
	path := ""
	if c.file != nil {
		c.err = errors.Join(c.err, c.file.Sync(), c.file.Close())
		base := filepath.Join(filepath.Dir(c.file.Name()), "failure-"+filepath.Base(c.file.Name())[len(".response-"):])
		path = base + ".json"
		c.record.ResponsePath = base + ".response"
		if err := os.Rename(c.file.Name(), c.record.ResponsePath); err != nil {
			c.record.ResponsePath = c.file.Name()
			c.err = errors.Join(c.err, err)
		}
	}
	if c.err != nil {
		c.record.CaptureError = c.err.Error()
	}
	if path != "" {
		encoded, err := json.MarshalIndent(c.record, "", "  ")
		if err == nil {
			var recordFile *os.File
			recordFile, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err == nil {
				_, err = recordFile.Write(append(encoded, '\n'))
				err = errors.Join(err, recordFile.Sync(), recordFile.Close())
			}
		}
		c.err = errors.Join(c.err, err)
	}
	details := map[string]any{"recordPath": path, "responsePath": c.record.ResponsePath}
	if c.err != nil {
		details["recordingError"] = c.err.Error()
	}
	raw, _ := json.Marshal(details)
	diagnostic, err := llm.NewAssistantDiagnostic(llm.AssistantDiagnosticSpec{
		Type: "provider_failure", Timestamp: c.record.Timestamp,
		Error: &llm.AssistantDiagnosticError{Name: fmt.Sprintf("%T", cause), Message: message}, Details: raw,
	})
	if err == nil {
		c.diag = []llm.AssistantDiagnostic{diagnostic}
	}
	return c.diag
}

func (d *streamDiagnostics) close() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.closed = true
	c := d.current
	d.mu.Unlock()
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		return
	}
	c.done = true
	if c.file != nil {
		_ = c.file.Close()
		// This is an owned, successful request's temporary capture. Failed
		// captures are retained above and never removed by this cleanup.
		_ = os.Remove(c.file.Name())
	}
}
