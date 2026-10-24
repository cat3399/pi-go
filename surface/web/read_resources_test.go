package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/session"
	"github.com/cat3399/pi-go/internal/surfacewire"
)

func TestSessionResourcesShareConditionalReadsAndRejectReplacedSource(t *testing.T) {
	cwd, agentDir := t.TempDir(), t.TempDir()
	directory, err := session.SessionDirForAgentDir(cwd, agentDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	var original bytes.Buffer
	encode := func(value any) {
		t.Helper()
		if err := json.NewEncoder(&original).Encode(value); err != nil {
			t.Fatal(err)
		}
	}
	encode(map[string]any{"type": "session", "version": 3, "id": "paged", "cwd": cwd, "timestamp": "2026-10-24T00:00:00Z"})
	for i := 0; i < 10; i++ {
		var parent any
		if i > 0 {
			parent = fmt.Sprintf("n%d", i-1)
		}
		text := fmt.Sprintf("message %d", i)
		if i == 8 {
			text = strings.Repeat("大", 4096)
		}
		content := []any{map[string]any{"type": "text", "text": text}}
		if i == 9 {
			content = append(content, map[string]any{"type": "image", "data": "YWJjZA==", "mimeType": "image/png"})
		}
		encode(map[string]any{"type": "message", "id": fmt.Sprintf("n%d", i), "parentId": parent, "timestamp": "2026-10-24T00:00:00Z", "message": map[string]any{"role": "user", "content": content, "timestamp": i + 1}})
	}
	path := filepath.Join(directory, "paged.jsonl")
	if err := os.WriteFile(path, original.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	api := testService(t, app.ProductionConfig{WorkingDir: cwd, AgentDir: agentDir}, nil)
	mux := http.NewServeMux()
	registerAPIRoutes(mux, api)
	for _, kind := range []string{"overview", "directory"} {
		path := "/api/v1/resources/" + kind
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		var remote surfacewire.ResourceResponse
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &remote) != nil {
			t.Fatalf("%s = %d %s", kind, response.Code, response.Body)
		}
		local, err := surfacewire.ReadApplicationResource(api, kind, surfacewire.ApplicationReadRequest{}, "")
		if err != nil || local.Token != remote.Token || !bytes.Equal(local.Data, remote.Data) {
			t.Fatalf("%s differs across HTTP/IPC: %v", kind, err)
		}
		conditional := httptest.NewRequest(http.MethodGet, path, nil)
		conditional.Header.Set("If-None-Match", response.Header().Get("ETag"))
		validated := httptest.NewRecorder()
		mux.ServeHTTP(validated, conditional)
		if validated.Code != 304 || validated.Body.Len() != 0 {
			t.Fatalf("%s validation = %d", kind, validated.Code)
		}
		if kind == "overview" && validated.Header().Get("X-Pi-Event-Cursor") == "" {
			t.Fatal("304 lost event anchor")
		}
	}
	get := func(id, kind string, query surfacewire.ReadResourceRequest, known, etag string) (*httptest.ResponseRecorder, surfacewire.ResourceResponse) {
		t.Helper()
		encoded, _ := json.Marshal(query)
		params := url.Values{"query": {string(encoded)}, "known": {known}}
		request := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+id+"/resources/"+kind+"?"+params.Encode(), nil)
		request.Header.Set("If-None-Match", etag)
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		var body surfacewire.ResourceResponse
		if response.Code == 200 {
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
		}
		return response, body
	}
	query := surfacewire.ReadResourceRequest{Limit: 2, MaxBytes: 4096}
	response, first := get("paged", "history", query, "", "")
	if response.Code != 200 {
		t.Fatalf("page = %d %s", response.Code, response.Body)
	}
	var page surfacewire.HistoryPage
	if err := json.Unmarshal(first.Data, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Total != 10 || page.Start != 9 || page.Older == "" || len(page.Items[0].Message) == 0 {
		t.Fatalf("bounded page = %s", first.Data)
	}
	if bytes.Contains(first.Data, []byte("YWJjZA==")) || !bytes.Contains(first.Data, []byte(page.Version.Generation)) || !bytes.Contains(first.Data, []byte(`"imageRef"`)) {
		t.Fatalf("versioned media = %s", first.Data)
	}
	local, err := surfacewire.ReadSessionResource(context.Background(), api, "paged", "history", query, "")
	if err != nil || local.Token != first.Token || !bytes.Equal(local.Data, first.Data) {
		t.Fatalf("HTTP/IPC differ: %v", err)
	}
	cached, unchanged := get("paged", "history", query, first.Token, "")
	if cached.Code != 200 || !unchanged.Unchanged || len(unchanged.Data) != 0 {
		t.Fatalf("IPC-style validation = %s", cached.Body)
	}
	notModified, _ := get("paged", "history", query, "", response.Header().Get("ETag"))
	if notModified.Code != 304 || notModified.Body.Len() != 0 {
		t.Fatalf("HTTP validation = %d %s", notModified.Code, notModified.Body)
	}
	older, value := get("paged", "history", surfacewire.ReadResourceRequest{Cursor: page.Older, Limit: 2}, "", "")
	var previous surfacewire.HistoryPage
	_ = json.Unmarshal(value.Data, &previous)
	if older.Code != 200 || previous.Version != page.Version || previous.End != page.Start || !bytes.Contains(value.Data, []byte(strings.Repeat("大", 4096))) {
		t.Fatalf("older page = %s", older.Body)
	}
	wrong, _ := get("other", "history", surfacewire.ReadResourceRequest{Cursor: page.Older}, "", "")
	if wrong.Code != 400 {
		t.Fatalf("cross-session cursor = %d", wrong.Code)
	}
	wrongKind, _ := get("paged", "nodes", surfacewire.ReadResourceRequest{Cursor: page.Older}, "", "")
	if wrongKind.Code != 400 {
		t.Fatalf("cross-query cursor = %d", wrongKind.Code)
	}
	live, liveValue := get("paged", "live", surfacewire.ReadResourceRequest{}, "", "")
	if live.Code != 200 || !bytes.Contains(liveValue.Data, []byte(`"active":false`)) {
		t.Fatalf("read opened Runtime: %s", live.Body)
	}
	entry, entryValue := get("paged", "entry", surfacewire.ReadResourceRequest{EntryID: "n8", Generation: page.Version.Generation}, "", "")
	if entry.Code != 200 || !bytes.Contains(entryValue.Data, []byte(strings.Repeat("大", 4096))) {
		t.Fatal("single entry lost original content")
	}
	imageURL := "/api/v1/sessions/paged/entries/n9/image?blockIndex=1&generation=" + page.Version.Generation
	image := httptest.NewRecorder()
	mux.ServeHTTP(image, httptest.NewRequest(http.MethodGet, imageURL, nil))
	if image.Code != 200 || image.Body.String() != "abcd" {
		t.Fatalf("image = %d %s", image.Code, image.Body)
	}
	before, _ := os.Stat(path)
	replacement := bytes.Replace(original.Bytes(), []byte("YWJjZA=="), []byte("ZWZnaA=="), 1)
	if err := os.WriteFile(path, replacement, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	stale := httptest.NewRecorder()
	mux.ServeHTTP(stale, httptest.NewRequest(http.MethodGet, imageURL, nil))
	if stale.Code != 409 {
		t.Fatalf("stale image read different bytes: %d %s", stale.Code, stale.Body)
	}
	oldPage, _ := get("paged", "history", surfacewire.ReadResourceRequest{Cursor: page.Older}, "", "")
	if oldPage.Code != 409 {
		t.Fatalf("stale page = %d %s", oldPage.Code, oldPage.Body)
	}
	latest, next := get("paged", "history", query, first.Token, "")
	if latest.Code != 200 || next.Unchanged || next.Token == first.Token {
		t.Fatal("replacement validated old page")
	}
}
