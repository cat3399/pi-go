package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestDownloadStreamsBinaryBeyondPreviewAndResponseLimits(t *testing.T) {
	chunk := bytes.Repeat([]byte{0, 255, 128, 10}, 16*1024)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer native-download-token" || r.URL.Query().Has("token") {
			t.Error("download did not retain native header authentication")
		}
		if r.URL.Path != "/proxy/api/v1/files/下载.bin" || r.URL.Query().Get("type") != "download" {
			t.Errorf("unexpected URL: %s", r.URL)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		for range 1025 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	bridge := NewRemoteBridge()
	bridge.credentials = newMemoryCredentialStore()
	bridge.downloadRoot = t.TempDir()
	bridge.rememberToken(server.URL+"/proxy", "native-download-token")
	file, err := bridge.DownloadFileWithID("download", server.URL+"/proxy", "/api/v1/files/%E4%B8%8B%E8%BD%BD.bin?type=download", "", "下载.bin")
	if err != nil {
		t.Fatal(err)
	}
	if file.Name != "下载.bin" || file.MIMEType != "application/octet-stream" {
		t.Fatalf("metadata = %#v", file)
	}
	reader, err := os.Open(file.Path)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, len(chunk))
	for range 1025 {
		if _, err := io.ReadFull(reader, buffer); err != nil || !bytes.Equal(buffer, chunk) {
			t.Fatalf("binary data changed: %v", err)
		}
	}
	if n, err := reader.Read(buffer); n != 0 || err != io.EOF {
		t.Fatalf("unexpected trailing data: %d %v", n, err)
	}
	_ = reader.Close()
	bridge.DiscardDownload(file.ID)
	if _, err := os.Stat(file.Path); !os.IsNotExist(err) {
		t.Fatalf("staged file remains: %v", err)
	}
}

func TestDownloadCancellationCleansPartialFile(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	bridge := NewRemoteBridge()
	bridge.downloadRoot = t.TempDir()
	result := make(chan error, 1)
	go func() {
		_, err := bridge.DownloadFileWithID("cancel", server.URL, "/api/v1/files/file.bin?type=download", "", "file.bin")
		result <- err
	}()
	<-started
	bridge.CancelRequest("cancel")
	if err := <-result; err == nil {
		t.Fatal("cancelled download succeeded")
	}
	entries, err := os.ReadDir(bridge.downloadRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("partial files remain: %v %v", entries, err)
	}
}

func TestDownloadRejectsAuthenticationFailureWithoutSaving(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"Authentication required"}`)
	}))
	defer server.Close()
	bridge := NewRemoteBridge()
	bridge.ctx = context.Background()
	bridge.credentials = newMemoryCredentialStore()
	bridge.downloadRoot = t.TempDir()
	bridge.rememberToken(server.URL, "expired")
	_, err := bridge.DownloadFileWithID("unauthorized", server.URL, "/api/v1/files/a?type=download", "", "a")
	if err == nil || err.Error() != "Authentication required" {
		t.Fatalf("error = %v", err)
	}
	if bridge.credentialForEndpoint(server.URL, "").token != "" {
		t.Fatal("rejected credential retained")
	}
	entries, _ := os.ReadDir(bridge.downloadRoot)
	if len(entries) != 0 {
		t.Fatal("error response was saved as a file")
	}
}
