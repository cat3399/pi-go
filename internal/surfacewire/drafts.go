package surfacewire

import (
	"context"
	"time"

	"github.com/cat3399/pi-go/internal/application"
)

type DraftText struct {
	Revision  string `json:"revision"`
	Text      string `json:"text"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

type DraftTextUpdate struct {
	BaseRevision string `json:"baseRevision"`
	Revision     string `json:"revision"`
	Text         string `json:"text"`
}

type DraftTextWriteResult struct {
	Saved bool      `json:"saved"`
	Draft DraftText `json:"draft"`
}

func projectDraftText(value application.DraftText) DraftText {
	result := DraftText{Revision: value.Revision, Text: value.Text}
	if !value.UpdatedAt.IsZero() {
		result.UpdatedAt = value.UpdatedAt.Format(time.RFC3339Nano)
	}
	return result
}

func ReadDraftText(ctx context.Context, api application.API, key string) (DraftText, error) {
	value, err := api.ReadDraftText(ctx, key)
	return projectDraftText(value), err
}

func WriteDraftText(ctx context.Context, api application.API, key string, update DraftTextUpdate) (DraftTextWriteResult, error) {
	value, err := api.WriteDraftText(ctx, key, application.DraftTextUpdate{
		BaseRevision: update.BaseRevision, Revision: update.Revision, Text: update.Text,
	})
	return DraftTextWriteResult{Saved: value.Saved, Draft: projectDraftText(value.Draft)}, err
}
