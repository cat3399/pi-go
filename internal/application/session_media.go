package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

var ErrImageNotFound = errors.New("image block not found")

type SessionImage struct {
	Data     []byte
	MIMEType string
}

// SessionImage reads the durable image at its original content-block position,
// including entries outside the active branch or compacted context.
func (s *Service) SessionImage(ctx context.Context, id, entryID string, blockIndex int) (SessionImage, error) {
	if cause := context.Cause(normalizeContext(ctx)); cause != nil {
		return SessionImage{}, cause
	}
	if blockIndex < 0 {
		return SessionImage{}, ErrImageNotFound
	}
	manager, _, _, closeManager, err := s.sessionManagerForRead(id)
	if err != nil {
		return SessionImage{}, err
	}
	if closeManager {
		defer manager.Close()
	}
	entry, ok := manager.Entry(entryID)
	if !ok {
		return SessionImage{}, ErrSessionEntryNotFound
	}
	var raw struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(entry.RawJSON(), &raw); err != nil {
		return SessionImage{}, err
	}
	content := raw.Message.Content
	if entry.Type() == "custom_message" {
		content = raw.Content
	}
	var blocks []struct {
		Type     string `json:"type"`
		Data     string `json:"data"`
		MIMEType string `json:"mimeType"`
		Source   *struct {
			Type      string `json:"type"`
			Data      string `json:"data"`
			MediaType string `json:"media_type"`
		} `json:"source"`
	}
	if json.Unmarshal(content, &blocks) != nil || blockIndex >= len(blocks) {
		return SessionImage{}, ErrImageNotFound
	}
	block := blocks[blockIndex]
	if block.Type != "image" {
		return SessionImage{}, ErrImageNotFound
	}
	data, mime := block.Data, block.MIMEType
	if data == "" && block.Source != nil && block.Source.Type == "base64" {
		data, mime = block.Source.Data, block.Source.MediaType
	}
	if data == "" || !strings.HasPrefix(mime, "image/") {
		return SessionImage{}, ErrImageNotFound
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return SessionImage{}, err
	}
	return SessionImage{Data: decoded, MIMEType: mime}, nil
}
