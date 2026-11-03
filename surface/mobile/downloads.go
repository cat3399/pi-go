package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type StagedDownload struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Name     string `json:"name"`
	MIMEType string `json:"mimeType"`
}

// The Go bridge owns authentication and streams to private app storage. Only
// the staged file path crosses into the WebView for the Android save picker.
func (b *RemoteBridge) DownloadFileWithID(requestID, endpoint, requestPath, token, name string) (StagedDownload, error) {
	if strings.TrimSpace(requestID) == "" {
		return StagedDownload{}, errors.New("download request ID is required")
	}
	reference, err := url.Parse(requestPath)
	if err != nil || !strings.HasPrefix(reference.Path, "/api/v1/files/") || reference.Query().Get("type") != "download" {
		return StagedDownload{}, errors.New("invalid file download path")
	}
	target, err := remoteAPIURL(endpoint, requestPath)
	if err != nil {
		return StagedDownload{}, err
	}
	ctx, finish, err := b.beginRequest(requestID, 0)
	if err != nil {
		return StagedDownload{}, err
	}
	defer finish()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return StagedDownload{}, err
	}
	credential := b.credentialForEndpoint(endpoint, token)
	if credential.token != "" {
		request.Header.Set("Authorization", "Bearer "+credential.token)
	}
	response, err := b.streamClient.Do(request)
	if err != nil {
		return StagedDownload{}, fmt.Errorf("download failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusUnauthorized {
			b.forgetToken(endpoint, credential)
		}
		var body struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(response.Body, 16<<10)).Decode(&body)
		if body.Error != "" {
			return StagedDownload{}, errors.New(body.Error)
		}
		return StagedDownload{}, fmt.Errorf("download failed: HTTP %d", response.StatusCode)
	}
	directory := b.downloadRoot
	if directory == "" {
		directory, err = platformDownloadDirectory()
		if err != nil {
			return StagedDownload{}, err
		}
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return StagedDownload{}, err
	}
	file, err := os.CreateTemp(directory, "pi-download-*")
	if err != nil {
		return StagedDownload{}, err
	}
	complete := false
	defer func() {
		_ = file.Close()
		if !complete {
			_ = os.Remove(file.Name())
		}
	}()
	if _, err := io.Copy(file, response.Body); err != nil {
		return StagedDownload{}, err
	}
	if err := ctx.Err(); err != nil {
		return StagedDownload{}, err
	}
	if err := file.Close(); err != nil {
		return StagedDownload{}, err
	}
	id := filepath.Base(file.Name())
	b.downloadMu.Lock()
	if err := ctx.Err(); err != nil {
		b.downloadMu.Unlock()
		return StagedDownload{}, err
	}
	if b.downloads == nil {
		b.downloads = make(map[string]string)
	}
	b.downloads[id] = file.Name()
	b.downloadMu.Unlock()
	complete = true
	return StagedDownload{ID: id, Path: file.Name(), Name: filepath.Base(strings.ReplaceAll(name, "\\", "/")), MIMEType: response.Header.Get("Content-Type")}, nil
}

func (b *RemoteBridge) DiscardDownload(id string) {
	b.downloadMu.Lock()
	defer b.downloadMu.Unlock()
	if path := b.downloads[id]; path != "" {
		_ = os.Remove(path)
		delete(b.downloads, id)
	}
}

func (b *RemoteBridge) discardDownloads() {
	b.downloadMu.Lock()
	defer b.downloadMu.Unlock()
	for id, path := range b.downloads {
		_ = os.Remove(path)
		delete(b.downloads, id)
	}
}
