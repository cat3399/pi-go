package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/session"
)

func TestHistoryImagesLoadOnDemandWithoutChangingSession(t *testing.T) {
	cwd, agentDir := t.TempDir(), t.TempDir()
	directory, err := session.SessionDirForAgentDir(cwd, agentDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	const encoded = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	image := map[string]any{"type": "image", "data": encoded, "mimeType": "image/png"}
	legacy := map[string]any{"type": "image", "source": map[string]any{"type": "base64", "data": encoded, "media_type": "image/png"}}
	text := map[string]any{"type": "text", "text": "keep this text"}
	entries := []map[string]any{
		{"type": "session", "version": 3, "id": "media-session", "cwd": cwd, "timestamp": "2026-09-10T00:00:00Z"},
		{"type": "message", "id": "user", "parentId": nil, "message": map[string]any{"role": "user", "content": []any{text, image, image}, "timestamp": 1}},
		// This tool result is on a different branch from the selected leaf.
		{"type": "message", "id": "tool", "parentId": "user", "message": map[string]any{"role": "toolResult", "toolCallId": "read-1", "toolName": "read", "content": []any{text, legacy}, "isError": false, "timestamp": 2}},
		{"type": "custom_message", "id": "custom", "parentId": "user", "customType": "attachment", "content": []any{text, image}, "display": true},
	}
	var original bytes.Buffer
	for _, entry := range entries {
		if entry["timestamp"] == nil {
			entry["timestamp"] = "2026-09-10T00:00:00Z"
		}
		if err := json.NewEncoder(&original).Encode(entry); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(directory, "media.jsonl")
	if err := os.WriteFile(path, original.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	api := testService(t, app.ProductionConfig{WorkingDir: cwd, AgentDir: agentDir}, nil)
	mux := http.NewServeMux()
	registerAPIRoutes(mux, api)
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		return response
	}
	for _, suffix := range []string{"?deferMedia=1", "/context?deferMedia=1", "?leafId=tool&deferMedia=1"} {
		response := get("/api/v1/sessions/media-session" + suffix)
		if response.Code != http.StatusOK {
			t.Fatalf("history: %d %s", response.Code, response.Body)
		}
		if strings.Contains(response.Body.String(), encoded) || !strings.Contains(response.Body.String(), `"imageRef"`) || !strings.Contains(response.Body.String(), "keep this text") {
			t.Fatalf("history should retain references and text only: %s", response.Body)
		}
	}
	viewResponse := get("/api/v1/sessions/media-session?deferMedia=1")
	var view sessionViewWire
	if err := json.Unmarshal(viewResponse.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Context.Messages) != 2 {
		t.Fatalf("context = %s", viewResponse.Body)
	}
	var user struct {
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(view.Context.Messages[0], &user); err != nil {
		t.Fatal(err)
	}
	if len(user.Content) != 3 {
		t.Fatalf("image positions changed: %#v", user.Content)
	}
	for index := 1; index <= 2; index++ {
		ref := user.Content[index]["imageRef"].(map[string]any)
		if ref["entryId"] != "user" || ref["blockIndex"] != float64(index) {
			t.Fatalf("ref = %#v", ref)
		}
	}
	decoded, _ := base64.StdEncoding.DecodeString(encoded)
	for _, entry := range []string{"user", "tool", "custom"} {
		url := fmt.Sprintf("/api/v1/sessions/media-session/entries/%s/image?blockIndex=1", entry)
		response := get(url)
		if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "image/png" || !bytes.Equal(response.Body.Bytes(), decoded) {
			t.Fatalf("image %s = %d %s", entry, response.Code, response.Body)
		}
		request := httptest.NewRequest(http.MethodGet, url, nil)
		request.Header.Set("If-None-Match", response.Header().Get("ETag"))
		cached := httptest.NewRecorder()
		mux.ServeHTTP(cached, request)
		if cached.Code != http.StatusNotModified || cached.Body.Len() != 0 {
			t.Fatalf("cache = %d", cached.Code)
		}
	}
	for _, tc := range []struct {
		entry, index string
		status       int
	}{
		{"user", "0", 404}, {"user", "99", 404}, {"missing", "1", 404},
		{"user", "-1", 400}, {"user", "invalid", 400}, {"user", "", 400},
	} {
		response := get(fmt.Sprintf("/api/v1/sessions/media-session/entries/%s/image?blockIndex=%s", tc.entry, tc.index))
		if response.Code != tc.status {
			t.Fatalf("%+v = %d %s", tc, response.Code, response.Body)
		}
	}
	// Full queries and the durable file retain the original image contract.
	full := get("/api/v1/sessions/media-session")
	if full.Code != http.StatusOK || !strings.Contains(full.Body.String(), encoded) {
		t.Fatal("full history lost images")
	}
	stored, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(stored, original.Bytes()) {
		t.Fatalf("session file changed: %v", err)
	}
	snapshot, err := api.SnapshotSession("media-session", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range snapshot.Context.AgentMessages() {
		raw, err := session.MarshalAgentMessage(message)
		if err != nil || !bytes.Contains(raw, []byte(encoded)) || bytes.Contains(raw, []byte(`"imageRef"`)) {
			t.Fatalf("model context lost image data: %s, %v", raw, err)
		}
	}
}
