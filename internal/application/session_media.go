package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"github.com/cat3399/pi-go/internal/session"
)

var ErrImageNotFound = errors.New("image block not found")

type SessionImage struct {
	Data     []byte
	MIMEType string
}

// SessionImage reads the durable image at its original content-block position,
// including entries outside the active branch or compacted context.
func (s *Service) SessionImage(ctx context.Context, id, entryID string, blockIndex int) (SessionImage, error) {
	return s.SessionImageAt(ctx, id, entryID, "", blockIndex)
}

func (s *Service) SessionImageAt(ctx context.Context, id, entryID, generation string, blockIndex int) (SessionImage, error) {
	if cause := context.Cause(normalizeContext(ctx)); cause != nil {
		return SessionImage{}, cause
	}
	if blockIndex < 0 {
		return SessionImage{}, ErrImageNotFound
	}
	read, err := s.ReadEntry(id, entryID, generation)
	if errors.Is(err, session.ErrEntryNotFound) {
		return SessionImage{}, ErrSessionEntryNotFound
	}
	if err != nil {
		return SessionImage{}, err
	}
	entry := read.Entry
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
