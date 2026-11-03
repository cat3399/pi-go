package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type pendingDownload struct {
	file        *os.File
	destination string
}

// Downloads are host file operations, independent of preview rendering limits.
func (b *GUIBridge) DownloadFile(path string) error {
	api, err := b.localAPI()
	if err != nil {
		return err
	}
	resource, err := api.ResolveFile(b.context(), path, "")
	if err != nil {
		return err
	}
	if !resource.IsFile {
		return errors.New("not a file")
	}
	source, err := os.Open(resource.Path)
	if err != nil {
		return err
	}
	defer source.Close()
	id, err := b.BeginDownload(resource.Name)
	if err != nil || id == "" {
		return err
	}
	defer b.DiscardDownload(id)
	b.downloadMu.Lock()
	if download := b.downloads[id]; download != nil {
		_, err = io.Copy(download.file, source)
	} else {
		err = errors.New("download is no longer active")
	}
	b.downloadMu.Unlock()
	if err != nil {
		return err
	}
	return b.CompleteDownload(id)
}

func (b *GUIBridge) BeginDownload(name string) (string, error) {
	b.mu.RLock()
	app := b.wailsApp
	b.mu.RUnlock()
	if app == nil {
		return "", errors.New("desktop application is unavailable")
	}
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	destination, err := app.Dialog.SaveFile().SetFilename(name).CanCreateDirectories(true).PromptForSingleSelection()
	if err != nil || destination == "" {
		return "", err
	}
	if err := b.context().Err(); err != nil {
		return "", err
	}
	return b.beginDownload(destination)
}

// Stage beside the destination so failed or cancelled transfers never truncate
// an existing file, including when the user selects the source file itself.
func (b *GUIBridge) beginDownload(destination string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(filepath.Dir(destination), ".pi-download-*")
	if err != nil {
		return "", err
	}
	id := hex.EncodeToString(random[:])
	b.downloadMu.Lock()
	defer b.downloadMu.Unlock()
	if b.downloads == nil {
		b.downloads = make(map[string]*pendingDownload)
	}
	b.downloads[id] = &pendingDownload{file: file, destination: destination}
	return id, nil
}

func (b *GUIBridge) WriteDownload(id, encoded string) error {
	if len(encoded) > base64.StdEncoding.EncodedLen(64*1024) {
		return errors.New("download chunk exceeds 64 KiB")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return err
	}
	b.downloadMu.Lock()
	defer b.downloadMu.Unlock()
	download := b.downloads[id]
	if download == nil {
		return errors.New("download is no longer active")
	}
	_, err = download.file.Write(data)
	return err
}

func (b *GUIBridge) CompleteDownload(id string) error {
	b.downloadMu.Lock()
	defer b.downloadMu.Unlock()
	download := b.downloads[id]
	if download == nil {
		return errors.New("download is no longer active")
	}
	if err := download.file.Sync(); err != nil {
		return err
	}
	if err := download.file.Close(); err != nil {
		return err
	}
	if err := os.Rename(download.file.Name(), download.destination); err != nil {
		return err
	}
	delete(b.downloads, id)
	return nil
}

func (b *GUIBridge) DiscardDownload(id string) {
	b.downloadMu.Lock()
	defer b.downloadMu.Unlock()
	if download := b.downloads[id]; download != nil {
		_ = download.file.Close()
		_ = os.Remove(download.file.Name())
		delete(b.downloads, id)
	}
}

func (b *GUIBridge) discardDownloads() {
	b.downloadMu.Lock()
	defer b.downloadMu.Unlock()
	for id, download := range b.downloads {
		_ = download.file.Close()
		_ = os.Remove(download.file.Name())
		delete(b.downloads, id)
	}
}
