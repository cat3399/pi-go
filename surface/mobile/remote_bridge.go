package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	wails "github.com/wailsapp/wails/v3/pkg/application"
)

const (
	mobileRemoteEvent      = "pi:mobile-remote-event"
	mobileRemoteErrorEvent = "pi:mobile-remote-error"
	maxRemoteResponseBytes = 64 << 20
	maxRemoteUploadFile    = 25 << 20
	maxRemoteUploadTotal   = 100 << 20
)

type RemoteResponse struct {
	Status       int    `json:"status"`
	Body         string `json:"body"`
	RetryAfterMS int64  `json:"retryAfterMs,omitempty"`
}

type RemoteStreamOpened struct {
	StreamID uint64 `json:"streamId"`
}

type RemoteStreamEvent struct {
	StreamID uint64 `json:"streamId"`
	Data     string `json:"data"`
}

type RemoteStreamError struct {
	StreamID uint64 `json:"streamId"`
	Revision uint64 `json:"revision"`
	Message  string `json:"message"`
	Terminal bool   `json:"terminal"`
}

type encodedRemoteUploadFile struct {
	Name string `json:"name"`
	Data string `json:"data"`
}

type remoteUploadFile struct {
	name string
	data []byte
}

type RemoteBridge struct {
	requestClient *http.Client
	uploadClient  *http.Client
	streamClient  *http.Client
	credentials   remoteCredentialStore
	requestMu     sync.Mutex
	requests      map[string]context.CancelFunc
	cancelled     map[string]struct{}

	mu         sync.RWMutex
	ctx        context.Context
	wailsApp   *wails.App
	nextStream uint64
	streams    map[uint64]*remoteEventStream

	credentialMu     sync.Mutex
	credentialTokens map[string]string
	credentialLoaded map[string]bool
}

type remoteEventStream struct {
	id       uint64
	endpoint string
	token    string

	mu       sync.Mutex
	revision uint64
	started  bool
	closed   bool
	ctx      context.Context
	cancel   context.CancelFunc
}

func NewRemoteBridge() *RemoteBridge {
	return &RemoteBridge{
		requestClient:    &http.Client{Timeout: 60 * time.Second},
		uploadClient:     &http.Client{Timeout: 5 * time.Minute},
		streamClient:     &http.Client{},
		credentials:      newPlatformCredentialStore(),
		requests:         make(map[string]context.CancelFunc),
		cancelled:        make(map[string]struct{}),
		streams:          make(map[uint64]*remoteEventStream),
		credentialTokens: make(map[string]string),
		credentialLoaded: make(map[string]bool),
	}
}

func (b *RemoteBridge) ServiceStartup(ctx context.Context, _ wails.ServiceOptions) error {
	b.mu.Lock()
	b.ctx = ctx
	b.wailsApp = wails.Get()
	b.mu.Unlock()
	return nil
}

func (b *RemoteBridge) ServiceShutdown() error {
	b.cancelRequests()
	b.mu.Lock()
	streams := make([]*remoteEventStream, 0, len(b.streams))
	for _, stream := range b.streams {
		streams = append(streams, stream)
	}
	b.streams = make(map[uint64]*remoteEventStream)
	b.wailsApp = nil
	b.mu.Unlock()
	for _, stream := range streams {
		stream.close()
	}
	return nil
}

func (b *RemoteBridge) Request(method, endpoint, requestPath, token, body string) (RemoteResponse, error) {
	return b.request("", method, endpoint, requestPath, token, body)
}

// RequestWithID gives the mobile frontend a cancellable request boundary. The
// legacy Request method remains available for callers that do not need one.
func (b *RemoteBridge) RequestWithID(requestID, method, endpoint, requestPath, token, body string) (RemoteResponse, error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return RemoteResponse{}, errors.New("remote request ID is required")
	}
	return b.request(requestID, method, endpoint, requestPath, token, body)
}

func (b *RemoteBridge) request(requestID, method, endpoint, requestPath, token, body string) (RemoteResponse, error) {
	method = strings.ToUpper(strings.TrimSpace(method))
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return RemoteResponse{}, fmt.Errorf("unsupported remote method %q", method)
	}
	target, err := remoteAPIURL(endpoint, requestPath)
	if err != nil {
		return RemoteResponse{}, err
	}
	requestContext, finishRequest, err := b.beginRequest(requestID, 60*time.Second)
	if err != nil {
		return RemoteResponse{}, err
	}
	defer finishRequest()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequestWithContext(requestContext, method, target.String(), reader)
	if err != nil {
		return RemoteResponse{}, fmt.Errorf("create remote request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	token = b.tokenForEndpoint(endpoint, token)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := b.requestClient.Do(request)
	if err != nil {
		return RemoteResponse{}, fmt.Errorf("remote request failed: %w", err)
	}
	defer response.Body.Close()
	data, err := readLimited(response.Body, maxRemoteResponseBytes)
	if err != nil {
		return RemoteResponse{}, err
	}
	result := RemoteResponse{
		Status: response.StatusCode, Body: string(data),
		RetryAfterMS: retryAfterMilliseconds(response.Header.Get("Retry-After")),
	}
	b.captureAuthentication(endpoint, requestPath, &result)
	return result, nil
}

func (b *RemoteBridge) UploadFilesWithID(requestID, endpoint, requestPath, token, filesJSON string) (RemoteResponse, error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return RemoteResponse{}, errors.New("remote request ID is required")
	}
	target, err := remoteAPIURL(endpoint, requestPath)
	if err != nil {
		return RemoteResponse{}, err
	}
	files, err := decodeRemoteUploadFiles(filesJSON)
	if err != nil {
		return RemoteResponse{}, err
	}
	requestContext, finishRequest, err := b.beginRequest(requestID, 5*time.Minute)
	if err != nil {
		return RemoteResponse{}, err
	}
	defer finishRequest()

	reader, writer := io.Pipe()
	multipartWriter := multipart.NewWriter(writer)
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, target.String(), reader)
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return RemoteResponse{}, fmt.Errorf("create remote upload request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	if token = b.tokenForEndpoint(endpoint, token); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	go writeRemoteUploadBody(writer, multipartWriter, files)

	response, err := b.uploadClient.Do(request)
	if err != nil {
		_ = reader.CloseWithError(err)
		return RemoteResponse{}, fmt.Errorf("remote upload failed: %w", err)
	}
	defer response.Body.Close()
	data, err := readLimited(response.Body, maxRemoteResponseBytes)
	if err != nil {
		return RemoteResponse{}, err
	}
	result := RemoteResponse{
		Status: response.StatusCode, Body: string(data),
		RetryAfterMS: retryAfterMilliseconds(response.Header.Get("Retry-After")),
	}
	b.captureAuthentication(endpoint, requestPath, &result)
	return result, nil
}

func (b *RemoteBridge) beginRequest(requestID string, timeout time.Duration) (context.Context, func(), error) {
	requestContext, cancel := context.WithTimeout(b.context(), timeout)
	if requestID == "" {
		return requestContext, cancel, nil
	}
	b.requestMu.Lock()
	if _, cancelled := b.cancelled[requestID]; cancelled {
		delete(b.cancelled, requestID)
		b.requestMu.Unlock()
		cancel()
		return nil, nil, fmt.Errorf("remote request cancelled: %w", context.Canceled)
	}
	if previous := b.requests[requestID]; previous != nil {
		previous()
	}
	b.requests[requestID] = cancel
	b.requestMu.Unlock()
	return requestContext, func() {
		cancel()
		b.requestMu.Lock()
		delete(b.requests, requestID)
		b.requestMu.Unlock()
	}, nil
}

func decodeRemoteUploadFiles(payload string) ([]remoteUploadFile, error) {
	var encoded []encodedRemoteUploadFile
	if err := json.Unmarshal([]byte(payload), &encoded); err != nil {
		return nil, fmt.Errorf("decode remote upload files: %w", err)
	}
	if len(encoded) == 0 {
		return nil, errors.New("no files selected")
	}
	files := make([]remoteUploadFile, 0, len(encoded))
	var total int64
	for _, input := range encoded {
		if len(input.Data) > base64.StdEncoding.EncodedLen(maxRemoteUploadFile) {
			return nil, fmt.Errorf("upload file %q exceeds 25MB", input.Name)
		}
		data, err := base64.StdEncoding.DecodeString(input.Data)
		if err != nil {
			return nil, fmt.Errorf("decode remote upload file %q: %w", input.Name, err)
		}
		if total += int64(len(data)); total > maxRemoteUploadTotal {
			return nil, errors.New("uploads exceed 100MB total")
		}
		files = append(files, remoteUploadFile{name: input.Name, data: data})
	}
	return files, nil
}

func writeRemoteUploadBody(pipe *io.PipeWriter, writer *multipart.Writer, files []remoteUploadFile) {
	var writeErr error
	for _, file := range files {
		part, err := writer.CreateFormFile("files", file.name)
		if err != nil {
			writeErr = err
			break
		}
		if _, err = part.Write(file.data); err != nil {
			writeErr = err
			break
		}
	}
	if closeErr := writer.Close(); writeErr == nil {
		writeErr = closeErr
	}
	_ = pipe.CloseWithError(writeErr)
}

func (b *RemoteBridge) CancelRequest(requestID string) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return
	}
	b.requestMu.Lock()
	cancel := b.requests[requestID]
	delete(b.requests, requestID)
	if cancel == nil {
		b.cancelled[requestID] = struct{}{}
	}
	b.requestMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (b *RemoteBridge) cancelRequests() {
	b.requestMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(b.requests))
	for requestID, cancel := range b.requests {
		cancels = append(cancels, cancel)
		delete(b.requests, requestID)
	}
	clear(b.cancelled)
	b.requestMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (b *RemoteBridge) OpenEventStream(endpoint, token string, after uint64) (RemoteStreamOpened, error) {
	if _, err := remoteEventsURL(endpoint, after); err != nil {
		return RemoteStreamOpened{}, err
	}
	streamContext, cancel := context.WithCancel(b.context())
	b.mu.Lock()
	b.nextStream++
	streamID := b.nextStream
	b.streams[streamID] = &remoteEventStream{
		id: streamID, endpoint: endpoint, token: b.tokenForEndpoint(endpoint, token), revision: after,
		ctx: streamContext, cancel: cancel,
	}
	b.mu.Unlock()
	return RemoteStreamOpened{StreamID: streamID}, nil
}

func (b *RemoteBridge) ResumeEventStream(streamID uint64) error {
	b.mu.RLock()
	stream := b.streams[streamID]
	b.mu.RUnlock()
	if stream == nil {
		return fmt.Errorf("remote event stream %d is not open", streamID)
	}
	if !stream.start() {
		return fmt.Errorf("remote event stream %d is already started or closed", streamID)
	}
	go b.pumpEventStream(stream)
	return nil
}

func (b *RemoteBridge) CloseEventStream(streamID uint64) {
	b.mu.Lock()
	stream := b.streams[streamID]
	delete(b.streams, streamID)
	b.mu.Unlock()
	if stream != nil {
		stream.close()
	}
}

func (b *RemoteBridge) pumpEventStream(stream *remoteEventStream) {
	defer func() {
		b.mu.Lock()
		if b.streams[stream.id] == stream {
			delete(b.streams, stream.id)
		}
		b.mu.Unlock()
		stream.close()
	}()

	backoff := 400 * time.Millisecond
	for {
		status, err := b.consumeEventStream(stream)
		if stream.ctx.Err() != nil {
			return
		}
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		terminal := status >= 400 && status < 500
		if status == http.StatusUnauthorized {
			b.forgetToken(stream.endpoint)
		}
		b.emit(mobileRemoteErrorEvent, RemoteStreamError{
			StreamID: stream.id, Revision: stream.cursor(), Message: err.Error(), Terminal: terminal,
		})
		if terminal {
			return
		}
		timer := time.NewTimer(backoff)
		select {
		case <-stream.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

func (b *RemoteBridge) consumeEventStream(stream *remoteEventStream) (int, error) {
	target, err := remoteEventsURL(stream.endpoint, stream.cursor())
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(stream.ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Cache-Control", "no-cache")
	if stream.token != "" {
		request.Header.Set("Authorization", "Bearer "+stream.token)
	}
	response, err := b.streamClient.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, readErr := readLimited(response.Body, 1<<20)
		if readErr != nil {
			return response.StatusCode, readErr
		}
		message := strings.TrimSpace(string(data))
		if message == "" {
			message = response.Status
		}
		return response.StatusCode, errors.New(message)
	}
	return response.StatusCode, readSSE(response.Body, func(id uint64, data string) error {
		if id != 0 {
			stream.setCursor(id)
		}
		b.emit(mobileRemoteEvent, RemoteStreamEvent{StreamID: stream.id, Data: data})
		return nil
	})
}

func (b *RemoteBridge) emit(name string, payload any) {
	b.mu.RLock()
	application := b.wailsApp
	b.mu.RUnlock()
	if application != nil {
		application.Event.Emit(name, payload)
	}
}

func (b *RemoteBridge) context() context.Context {
	b.mu.RLock()
	ctx := b.ctx
	b.mu.RUnlock()
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func (s *remoteEventStream) start() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.closed {
		return false
	}
	s.started = true
	return true
}

func (s *remoteEventStream) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *remoteEventStream) cursor() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision
}

func (s *remoteEventStream) setCursor(value uint64) {
	s.mu.Lock()
	if value > s.revision {
		s.revision = value
	}
	s.mu.Unlock()
}

func remoteAPIURL(endpoint, requestPath string) (*url.URL, error) {
	base, err := parseRemoteEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	reference, err := url.Parse(requestPath)
	if err != nil {
		return nil, fmt.Errorf("parse remote API path: %w", err)
	}
	if reference.IsAbs() || reference.Host != "" || !strings.HasPrefix(reference.Path, "/api/v1/") {
		return nil, errors.New("remote request path must remain under /api/v1/")
	}
	base.Path = strings.TrimRight(base.Path, "/") + reference.Path
	base.RawQuery = reference.RawQuery
	return base, nil
}

func remoteEventsURL(endpoint string, after uint64) (*url.URL, error) {
	target, err := remoteAPIURL(endpoint, "/api/v1/events")
	if err != nil {
		return nil, err
	}
	query := target.Query()
	query.Set("after", strconv.FormatUint(after, 10))
	target.RawQuery = query.Encode()
	return target, nil
}

func parseRemoteEndpoint(endpoint string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return nil, fmt.Errorf("parse remote endpoint: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("remote endpoint must use http or https")
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("remote endpoint is invalid")
	}
	return parsed, nil
}

func normalizeRemoteEndpoint(endpoint string) (string, error) {
	parsed, err := parseRemoteEndpoint(endpoint)
	if err != nil {
		return "", err
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String(), nil
}

func (b *RemoteBridge) tokenForEndpoint(endpoint, explicit string) string {
	if token := strings.TrimSpace(explicit); token != "" {
		return token
	}
	normalized, err := normalizeRemoteEndpoint(endpoint)
	if err != nil {
		return ""
	}
	b.credentialMu.Lock()
	defer b.credentialMu.Unlock()
	if !b.credentialLoaded[normalized] {
		b.credentialLoaded[normalized] = true
		if token, loadErr := b.credentials.Load(normalized); loadErr == nil {
			b.credentialTokens[normalized] = strings.TrimSpace(token)
		}
	}
	return b.credentialTokens[normalized]
}

func (b *RemoteBridge) rememberToken(endpoint, token string) {
	normalized, err := normalizeRemoteEndpoint(endpoint)
	if err != nil || strings.TrimSpace(token) == "" {
		return
	}
	token = strings.TrimSpace(token)
	b.credentialMu.Lock()
	b.credentialLoaded[normalized] = true
	b.credentialTokens[normalized] = token
	_ = b.credentials.Save(normalized, token)
	b.credentialMu.Unlock()
}

func (b *RemoteBridge) forgetToken(endpoint string) {
	normalized, err := normalizeRemoteEndpoint(endpoint)
	if err != nil {
		return
	}
	b.credentialMu.Lock()
	b.credentialLoaded[normalized] = true
	delete(b.credentialTokens, normalized)
	_ = b.credentials.Delete(normalized)
	b.credentialMu.Unlock()
}

func (b *RemoteBridge) captureAuthentication(endpoint, requestPath string, response *RemoteResponse) {
	reference, err := url.Parse(requestPath)
	if err != nil {
		return
	}
	if response.Status == http.StatusUnauthorized {
		b.forgetToken(endpoint)
	}
	if response.Status < 200 || response.Status >= 300 {
		return
	}
	var body map[string]any
	if json.Unmarshal([]byte(response.Body), &body) != nil {
		return
	}
	switch reference.Path {
	case "/api/v1/auth/login":
		token, _ := body["token"].(string)
		if strings.TrimSpace(token) == "" {
			return
		}
		b.rememberToken(endpoint, token)
		delete(body, "token")
		if encoded, marshalErr := json.Marshal(body); marshalErr == nil {
			response.Body = string(encoded)
		}
	case "/api/v1/auth/status":
		required, _ := body["authRequired"].(bool)
		authenticated, _ := body["authenticated"].(bool)
		if required && !authenticated {
			b.forgetToken(endpoint)
		}
	case "/api/v1/auth/logout":
		b.forgetToken(endpoint)
	}
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("remote response is too large")
	}
	return data, nil
}

func retryAfterMilliseconds(value string) int64 {
	seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || seconds <= 0 {
		return 0
	}
	return seconds * 1000
}

func readSSE(reader io.Reader, emit func(id uint64, data string) error) error {
	buffered := bufio.NewReader(reader)
	var eventID uint64
	var data strings.Builder
	for {
		line, err := buffered.ReadString('\n')
		if len(line) != 0 {
			line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			switch {
			case line == "":
				if data.Len() != 0 {
					value := strings.TrimSuffix(data.String(), "\n")
					if err := emit(eventID, value); err != nil {
						return err
					}
					data.Reset()
					eventID = 0
				}
			case strings.HasPrefix(line, "id:"):
				value := strings.TrimSpace(strings.TrimPrefix(line, "id:"))
				if value != "" {
					parsed, parseErr := strconv.ParseUint(value, 10, 64)
					if parseErr != nil {
						return fmt.Errorf("invalid SSE event id: %w", parseErr)
					}
					eventID = parsed
				}
			case strings.HasPrefix(line, "data:"):
				value := strings.TrimPrefix(line, "data:")
				value = strings.TrimPrefix(value, " ")
				if data.Len()+len(value) > maxRemoteResponseBytes {
					return errors.New("remote SSE event is too large")
				}
				data.WriteString(value)
				data.WriteByte('\n')
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && data.Len() != 0 {
				value := strings.TrimSuffix(data.String(), "\n")
				if emitErr := emit(eventID, value); emitErr != nil {
					return emitErr
				}
			}
			return err
		}
	}
}
