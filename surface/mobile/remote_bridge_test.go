package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type memoryCredentialStore struct {
	mu     sync.Mutex
	tokens map[string]string
}

func newMemoryCredentialStore() *memoryCredentialStore {
	return &memoryCredentialStore{tokens: make(map[string]string)}
}

func (s *memoryCredentialStore) Load(endpoint string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokens[endpoint], nil
}

func (s *memoryCredentialStore) Save(endpoint, token string) error {
	s.mu.Lock()
	s.tokens[endpoint] = token
	s.mu.Unlock()
	return nil
}

func (s *memoryCredentialStore) Delete(endpoint string) error {
	s.mu.Lock()
	delete(s.tokens, endpoint)
	s.mu.Unlock()
	return nil
}

func TestRemoteBridgeRequest(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Errorf("method = %q", request.Method)
		}
		if request.URL.Path != "/root/api/v1/sessions/example/commands" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != `{"type":"abort"}` {
			t.Errorf("body = %q", body)
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusAccepted)
		_, _ = writer.Write([]byte(`{"data":null}`))
	}))
	defer server.Close()

	bridge := NewRemoteBridge()
	bridge.ctx = context.Background()
	response, err := bridge.Request(
		http.MethodPost,
		server.URL+"/root/",
		"/api/v1/sessions/example/commands",
		"secret",
		`{"type":"abort"}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != http.StatusAccepted || response.Body != `{"data":null}` {
		t.Fatalf("response = %#v", response)
	}
}

func TestRemoteBridgeUploadsMultipartFiles(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Errorf("method = %q", request.Method)
		}
		if request.URL.Path != "/root/api/v1/files/workspace" || request.URL.Query().Get("type") != "upload" {
			t.Errorf("URL = %q", request.URL.String())
		}
		if request.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		file, header, err := request.FormFile("files")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			t.Fatal(err)
		}
		if header.Filename != "notes.txt" || string(data) != "hello" {
			t.Errorf("upload = %q %q", header.Filename, data)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"uploaded":["notes.txt"],"skipped":[],"errors":[]}`))
	}))
	defer server.Close()

	bridge := NewRemoteBridge()
	bridge.ctx = context.Background()
	response, err := bridge.UploadFilesWithID(
		"upload-1",
		server.URL+"/root/",
		"/api/v1/files/workspace?type=upload&conflict=error",
		"secret",
		`[{"name":"notes.txt","data":"aGVsbG8="}]`,
	)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != http.StatusOK || !strings.Contains(response.Body, `"notes.txt"`) {
		t.Fatalf("response = %#v", response)
	}
}

func TestRemoteBridgeRequestCanBeCancelled(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
	}))
	defer server.Close()

	bridge := NewRemoteBridge()
	bridge.ctx = context.Background()
	done := make(chan error, 1)
	go func() {
		_, err := bridge.RequestWithID(
			"connection-probe",
			http.MethodGet,
			server.URL,
			"/api/v1/auth/status",
			"",
			"",
		)
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("remote request did not start")
	}
	bridge.CancelRequest("connection-probe")
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled remote request returned without an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled remote request did not return")
	}
}

func TestRemoteBridgePersistsLoginTokenOutsideWebView(t *testing.T) {
	t.Parallel()
	const token = "persistent-secret"
	store := newMemoryCredentialStore()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/auth/login":
			_, _ = writer.Write([]byte(`{"ok":true,"authRequired":true,"authenticated":true,"token":"` + token + `"}`))
		case "/api/v1/auth/status":
			if request.Header.Get("Authorization") != "Bearer "+token {
				t.Errorf("authorization = %q", request.Header.Get("Authorization"))
			}
			_, _ = writer.Write([]byte(`{"ok":true,"authRequired":true,"authenticated":true}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	first := NewRemoteBridge()
	first.ctx = context.Background()
	first.credentials = store
	login, err := first.Request(http.MethodPost, server.URL, "/api/v1/auth/login", "", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(login.Body, token) || strings.Contains(login.Body, `"token"`) {
		t.Fatalf("login response leaked the stored bearer token: %s", login.Body)
	}

	second := NewRemoteBridge()
	second.ctx = context.Background()
	second.credentials = store
	if _, err := second.Request(http.MethodGet, server.URL, "/api/v1/auth/status", "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteBridgeDropsRejectedStoredToken(t *testing.T) {
	t.Parallel()
	store := newMemoryCredentialStore()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true,"authRequired":true,"authenticated":false}`))
	}))
	defer server.Close()
	if err := store.Save(server.URL, "expired"); err != nil {
		t.Fatal(err)
	}

	bridge := NewRemoteBridge()
	bridge.ctx = context.Background()
	bridge.credentials = store
	if _, err := bridge.Request(http.MethodGet, server.URL, "/api/v1/auth/status", "", ""); err != nil {
		t.Fatal(err)
	}
	if token, err := store.Load(server.URL); err != nil || token != "" {
		t.Fatalf("stored token after rejection = %q, %v", token, err)
	}
}

func TestRemoteBridgeLateRejectionKeepsFreshLogin(t *testing.T) {
	for _, route := range []string{"/api/v1/auth/status", "/api/v1/snapshot"} {
		t.Run(route, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/auth/login" {
					if r.Header.Get("Authorization") != "" {
						t.Error("password login carried a previous bearer token")
					}
					_, _ = io.WriteString(w, `{"ok":true,"authenticated":true,"token":"fresh-token"}`)
					return
				}
				close(started)
				<-release
				if route == "/api/v1/snapshot" {
					w.WriteHeader(http.StatusUnauthorized)
				}
				_, _ = io.WriteString(w, `{"authRequired":true,"authenticated":false}`)
			}))
			defer server.Close()
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			bridge := NewRemoteBridge()
			store := newMemoryCredentialStore()
			bridge.credentials = store
			bridge.rememberToken(server.URL, "old-token")
			done := make(chan error, 1)
			go func() {
				_, err := bridge.Request(http.MethodGet, server.URL, route, "", "")
				done <- err
			}()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("old request did not start")
			}
			login, err := bridge.Request(http.MethodPost, server.URL, "/api/v1/auth/login", "", `{}`)
			if err != nil || login.Status != http.StatusOK {
				t.Fatalf("login = %#v, %v", login, err)
			}
			unblock()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if bridge.credentialForEndpoint(server.URL, "").token != "fresh-token" {
				t.Fatal("old rejection erased the new in-memory credential")
			}
			if token, err := store.Load(server.URL); err != nil || token != "fresh-token" {
				t.Fatal("old rejection erased the new persistent credential")
			}
		})
	}
}

func TestRemoteBridgeNodeSwitchRetainsSeparateCredentials(t *testing.T) {
	store := newMemoryCredentialStore()
	node := func(token string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/api/v1/auth/login" {
				body, _ := io.ReadAll(r.Body)
				if string(body) != `{"passwordHash":"correct"}` {
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = io.WriteString(w, `{"error":"Invalid password"}`)
					return
				}
				_, _ = io.WriteString(w, `{"authenticated":true,"token":"`+token+`"}`)
				return
			}
			if r.Header.Get("Authorization") != "Bearer "+token {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"authRequired":true,"authenticated":true}`)
		}))
	}
	first, second := node("first-token"), node("second-token")
	defer first.Close()
	defer second.Close()
	bridge := NewRemoteBridge()
	bridge.credentials = store
	for _, endpoint := range []string{first.URL, second.URL} {
		response, err := bridge.Request(http.MethodPost, endpoint, "/api/v1/auth/login", "", `{"passwordHash":"correct"}`)
		if err != nil || response.Status != http.StatusOK {
			t.Fatalf("node login = %#v, %v", response, err)
		}
	}
	// A wrong password is not evidence that a previously issued token is invalid.
	response, err := bridge.Request(http.MethodPost, first.URL, "/api/v1/auth/login", "", `{"passwordHash":"wrong"}`)
	if err != nil || response.Status != http.StatusUnauthorized {
		t.Fatalf("incorrect password response = %#v, %v", response, err)
	}
	for range 2 {
		for _, endpoint := range []string{first.URL + "/", second.URL, first.URL} {
			response, err := bridge.Request(http.MethodGet, endpoint, "/api/v1/auth/status", "", "")
			if err != nil || response.Status != http.StatusOK {
				t.Fatalf("switching nodes lost their credentials: %#v, %v", response, err)
			}
		}
		// Repeat the switches after restarting the native bridge.
		bridge = NewRemoteBridge()
		bridge.credentials = store
	}
}

func TestRemoteBridgeOldEventStreamCannotInvalidateNewLogin(t *testing.T) {
	bridge := NewRemoteBridge()
	bridge.credentials = newMemoryCredentialStore()
	endpoint := "https://node.example"
	bridge.rememberToken(endpoint, "same-token")
	opened, err := bridge.OpenEventStream(endpoint, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.CloseEventStream(opened.StreamID)
	stream := bridge.streams[opened.StreamID]
	bridge.rememberToken(endpoint, "same-token")
	bridge.forgetToken(endpoint, stream.credential)
	if bridge.credentialForEndpoint(endpoint, "").token != "same-token" {
		t.Fatal("an earlier event stream erased the credential of a new login")
	}
}

func TestRemoteAPIURLValidation(t *testing.T) {
	t.Parallel()
	valid, err := remoteAPIURL("https://pi.example/base/", "/api/v1/snapshot?full=1")
	if err != nil {
		t.Fatal(err)
	}
	if valid.String() != "https://pi.example/base/api/v1/snapshot?full=1" {
		t.Fatalf("URL = %q", valid)
	}
	for _, test := range []struct {
		endpoint string
		path     string
	}{
		{"file:///tmp/pi", "/api/v1/snapshot"},
		{"https://user@pi.example", "/api/v1/snapshot"},
		{"https://pi.example", "https://other.example/api/v1/snapshot"},
		{"https://pi.example", "/not-api"},
	} {
		if _, err := remoteAPIURL(test.endpoint, test.path); err == nil {
			t.Errorf("remoteAPIURL(%q, %q) succeeded", test.endpoint, test.path)
		}
	}
}

func TestReadSSE(t *testing.T) {
	t.Parallel()
	input := strings.NewReader(": heartbeat\n\nid: 7\ndata: {\"type\":\"connected\"}\n\nid: 8\ndata: first\ndata: second\n\n")
	type event struct {
		id   uint64
		data string
	}
	var events []event
	err := readSSE(input, func(id uint64, data string) error {
		events = append(events, event{id: id, data: data})
		return nil
	})
	if err != io.EOF {
		t.Fatalf("error = %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %#v", events)
	}
	if events[0].id != 7 || events[0].data != `{"type":"connected"}` {
		t.Errorf("first event = %#v", events[0])
	}
	if events[1].id != 8 || events[1].data != "first\nsecond" {
		t.Errorf("second event = %#v", events[1])
	}
}
