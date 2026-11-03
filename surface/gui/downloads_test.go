package main

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestDownloadCommitsOnlyAfterAllChunks(t *testing.T) {
	bridge := &GUIBridge{}
	directory := t.TempDir()
	destination := filepath.Join(directory, "下载.bin")
	if err := os.WriteFile(destination, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := bridge.beginDownload(destination)
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte{0, 255, 128, 10}, 16384)
	for range 10 {
		if err := bridge.WriteDownload(id, base64.StdEncoding.EncodeToString(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := os.ReadFile(destination)
	if string(before) != "existing" {
		t.Fatal("download truncated the existing destination")
	}
	if err := bridge.CompleteDownload(id); err != nil {
		t.Fatal(err)
	}
	bridge.DiscardDownload(id)
	after, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(after, bytes.Repeat(chunk, 10)) {
		t.Fatalf("saved bytes differ: %v", err)
	}
	entries, _ := os.ReadDir(directory)
	if len(entries) != 1 {
		t.Fatalf("temporary files remain: %v", entries)
	}
}

func TestDownloadCancellationPreservesExistingFile(t *testing.T) {
	bridge := &GUIBridge{}
	directory := t.TempDir()
	destination := filepath.Join(directory, "existing.md")
	if err := os.WriteFile(destination, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := bridge.beginDownload(destination)
	if err != nil {
		t.Fatal(err)
	}
	if err := bridge.WriteDownload(id, "cGFydGlhbA=="); err != nil {
		t.Fatal(err)
	}
	bridge.DiscardDownload(id)
	if err := bridge.CompleteDownload(id); err == nil {
		t.Fatal("cancelled download committed")
	}
	data, _ := os.ReadFile(destination)
	if string(data) != "original" {
		t.Fatal("cancellation changed destination")
	}
	entries, _ := os.ReadDir(directory)
	if len(entries) != 1 {
		t.Fatalf("temporary files remain: %v", entries)
	}
}

func TestDownloadShutdownCleansPendingFiles(t *testing.T) {
	bridge := &GUIBridge{}
	directory := t.TempDir()
	id, err := bridge.beginDownload(filepath.Join(directory, "new.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := bridge.WriteDownload(id, "invalid!"); err == nil {
		t.Fatal("invalid base64 accepted")
	}
	bridge.discardDownloads()
	entries, _ := os.ReadDir(directory)
	if len(entries) != 0 {
		t.Fatalf("temporary files remain: %v", entries)
	}
}
