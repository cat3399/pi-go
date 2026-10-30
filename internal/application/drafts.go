package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/filelock"
)

// DraftText is unsubmitted input. It is application data, not a session entry
// or an Agent queue. Attachments remain on the surface's local device.
type DraftText struct {
	Revision  string
	Text      string
	UpdatedAt time.Time
}

type DraftTextUpdate struct {
	BaseRevision string
	Revision     string
	Text         string
}

type DraftTextWriteResult struct {
	Saved bool
	Draft DraftText
}

var ErrInvalidDraft = errors.New("invalid draft")

const maxDraftFileBytes = 64 << 20

type storedDraftText struct {
	Version   int       `json:"version"`
	Key       string    `json:"key"`
	Revision  string    `json:"revision"`
	Text      string    `json:"text"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (s *Service) draftPath(key string) (string, error) {
	if s == nil {
		return "", ErrClosed
	}
	if key == "" || len(key) > 16<<10 || !utf8.ValidString(key) {
		return "", fmt.Errorf("%w: draft key is required and must be valid UTF-8", ErrInvalidDraft)
	}
	hash := sha256.Sum256([]byte(key))
	return filepath.Join(s.paths.AgentDir, "drafts", hex.EncodeToString(hash[:])+".json"), nil
}

func readDraftText(path, key string) (DraftText, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return DraftText{}, nil
	}
	if err != nil {
		return DraftText{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxDraftFileBytes+1))
	if err != nil {
		return DraftText{}, err
	}
	if len(data) > maxDraftFileBytes {
		return DraftText{}, errors.New("stored draft exceeds the size limit")
	}
	var stored storedDraftText
	if err := json.Unmarshal(data, &stored); err != nil {
		return DraftText{}, fmt.Errorf("read draft: %w", err)
	}
	if stored.Version != 1 || stored.Key != key || stored.Revision == "" {
		return DraftText{}, errors.New("stored draft is invalid")
	}
	return DraftText{Revision: stored.Revision, Text: stored.Text, UpdatedAt: stored.UpdatedAt}, nil
}

func (s *Service) ReadDraftText(ctx context.Context, key string) (DraftText, error) {
	if err := normalizeContext(ctx).Err(); err != nil {
		return DraftText{}, err
	}
	path, err := s.draftPath(key)
	if err != nil {
		return DraftText{}, err
	}
	return readDraftText(path, key)
}

// WriteDraftText compares the last observed revision before replacing text.
// A stale writer gets the current value back without overwriting it. Retrying
// an acknowledged-or-lost request with the same revision is idempotent.
func (s *Service) WriteDraftText(ctx context.Context, key string, update DraftTextUpdate) (DraftTextWriteResult, error) {
	ctx = normalizeContext(ctx)
	path, err := s.draftPath(key)
	if err != nil {
		return DraftTextWriteResult{}, err
	}
	if update.Revision == "" || len(update.Revision) > 200 || len(update.BaseRevision) > 200 ||
		!utf8.ValidString(update.Revision) || !utf8.ValidString(update.BaseRevision) || !utf8.ValidString(update.Text) {
		return DraftTextWriteResult{}, fmt.Errorf("%w: invalid text or revision", ErrInvalidDraft)
	}
	unlock, err := filelock.Acquire(ctx, path+".lock")
	if err != nil {
		return DraftTextWriteResult{}, err
	}
	defer unlock()
	current, err := readDraftText(path, key)
	if err != nil {
		return DraftTextWriteResult{}, err
	}
	if current.Revision == update.Revision {
		if current.Text != update.Text {
			return DraftTextWriteResult{}, fmt.Errorf("%w: revision reused for different text", ErrInvalidDraft)
		}
		return DraftTextWriteResult{Saved: true, Draft: current}, nil
	}
	if current.Revision != update.BaseRevision {
		return DraftTextWriteResult{Draft: current}, nil
	}
	next := DraftText{Revision: update.Revision, Text: update.Text, UpdatedAt: time.Now().UTC()}
	encoded, err := json.Marshal(storedDraftText{
		Version: 1, Key: key, Revision: next.Revision, Text: next.Text, UpdatedAt: next.UpdatedAt,
	})
	if err != nil {
		return DraftTextWriteResult{}, err
	}
	if len(encoded) > maxDraftFileBytes {
		return DraftTextWriteResult{}, fmt.Errorf("%w: text exceeds the size limit", ErrInvalidDraft)
	}
	if err := ctx.Err(); err != nil {
		return DraftTextWriteResult{}, err
	}
	if err := writeFileAtomic(path, encoded, 0o600); err != nil {
		return DraftTextWriteResult{}, err
	}
	return DraftTextWriteResult{Saved: true, Draft: next}, nil
}
