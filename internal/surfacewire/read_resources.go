package surfacewire

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/cat3399/pi-go/internal/application"
	protocolv1 "github.com/cat3399/pi-go/internal/protocol/v1"
	"github.com/cat3399/pi-go/internal/session"
)

// ResourceResponse is shared by HTTP and IPC. The token validates exactly Data;
// unrelated application events never invalidate a fixed history page. HTTP may
// also express Unchanged as 304 with this token in ETag.
type ResourceResponse struct {
	Token     string          `json:"token"`
	Unchanged bool            `json:"unchanged,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Cursor    string          `json:"cursor,omitempty"`
}

func resourceResponse(value any, known string) (ResourceResponse, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return ResourceResponse{}, err
	}
	token := fmt.Sprintf("%x", sha256.Sum256(data))
	if token == known {
		return ResourceResponse{Token: token, Unchanged: true}, nil
	}
	return ResourceResponse{Token: token, Data: data}, nil
}

type SessionVersion struct {
	Generation string `json:"generation"`
	Log        uint64 `json:"log"`
	View       uint64 `json:"view"`
}

func sessionVersion(value session.ReadVersion) SessionVersion {
	return SessionVersion{Generation: value.Generation, Log: value.Log, View: value.View}
}

type ReadResourceRequest struct {
	LeafID     *string         `json:"leafId,omitempty"`
	At         *SessionVersion `json:"at,omitempty"`
	Cursor     string          `json:"cursor,omitempty"`
	AroundID   string          `json:"aroundId,omitempty"`
	AfterID    string          `json:"afterId,omitempty"`
	Limit      int             `json:"limit,omitempty"`
	MaxBytes   int             `json:"maxBytes,omitempty"`
	ParentID   *string         `json:"parentId,omitempty"`
	Role       string          `json:"role,omitempty"`
	Search     string          `json:"search,omitempty"`
	EntryID    string          `json:"entryId,omitempty"`
	Generation string          `json:"generation,omitempty"`
	Offset     int             `json:"offset,omitempty"`
	Length     int             `json:"length,omitempty"`
	BlockIndex int             `json:"blockIndex,omitempty"`
}

type EntryNode struct {
	ID          string  `json:"id"`
	ParentID    *string `json:"parentId"`
	Type        string  `json:"type"`
	Role        string  `json:"role,omitempty"`
	Timestamp   string  `json:"timestamp"`
	Preview     string  `json:"preview"`
	Label       string  `json:"label,omitempty"`
	HasChildren bool    `json:"hasChildren"`
	RawBytes    int     `json:"rawBytes"`
}

func readNode(value session.EntryNode) EntryNode {
	return EntryNode{ID: value.ID, ParentID: value.ParentID, Type: value.Type, Role: value.Role,
		Timestamp: formatWebTime(value.Timestamp), Preview: value.Preview, Label: value.Label,
		HasChildren: value.HasChildren, RawBytes: value.RawBytes}
}

type HistoryItem struct {
	Node     EntryNode       `json:"node"`
	Message  json.RawMessage `json:"message,omitempty"`
	Deferred bool            `json:"deferred,omitempty"`
}

type HistoryPage struct {
	SessionID string         `json:"sessionId"`
	Version   SessionVersion `json:"version"`
	AnchorID  string         `json:"anchorId"`
	Items     []HistoryItem  `json:"items"`
	Start     int            `json:"start"`
	End       int            `json:"end"`
	Total     int            `json:"total"`
	Connected bool           `json:"connected"`
	Older     string         `json:"older,omitempty"`
	Newer     string         `json:"newer,omitempty"`
}

type NodesPage struct {
	SessionID string         `json:"sessionId"`
	Version   SessionVersion `json:"version"`
	Items     []EntryNode    `json:"items"`
	Next      string         `json:"next,omitempty"`
}

type readCursor struct {
	SessionID string
	History   *session.HistoryCursor
	Nodes     *session.NodeCursor
}

func encodeReadCursor(value readCursor) string {
	if value.History == nil && value.Nodes == nil {
		return ""
	}
	data, _ := json.Marshal(value)
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeReadCursor(id, kind, encoded string) (readCursor, error) {
	var value readCursor
	if encoded == "" {
		return value, nil
	}
	if len(encoded) > 8192 {
		return value, session.ErrInvalidReadQuery
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || json.Unmarshal(data, &value) != nil || value.SessionID != id ||
		(kind == "history" && (value.History == nil || value.Nodes != nil)) ||
		(kind == "nodes" && (value.Nodes == nil || value.History != nil)) {
		return readCursor{}, session.ErrInvalidReadQuery
	}
	return value, nil
}

// ReadSessionResource only projects typed Application reads. Cursor framing,
// JSON and conditional transport responses stay outside Application/Core.
func ReadSessionResource(ctx context.Context, api application.API, id, kind string, query ReadResourceRequest, known string) (ResourceResponse, error) {
	var at *session.ReadVersion
	if query.At != nil {
		at = &session.ReadVersion{Generation: query.At.Generation, Log: query.At.Log, View: query.At.View}
	}
	switch kind {
	case "history":
		cursor, err := decodeReadCursor(id, kind, query.Cursor)
		if err != nil {
			return ResourceResponse{}, err
		}
		value, err := api.ReadHistory(id, session.HistoryQuery{LeafID: query.LeafID, At: at, Cursor: cursor.History,
			AroundID: query.AroundID, AfterID: query.AfterID, Limit: query.Limit, MaxBytes: query.MaxBytes})
		if err != nil {
			return ResourceResponse{}, err
		}
		page := value.Page
		result := HistoryPage{SessionID: id, Version: sessionVersion(page.Version), AnchorID: page.AnchorID,
			Start: page.Start, End: page.End, Total: page.Total, Connected: page.Connected, Items: []HistoryItem{},
			Older: encodeReadCursor(readCursor{SessionID: id, History: page.Older}), Newer: encodeReadCursor(readCursor{SessionID: id, History: page.Newer})}
		for _, item := range page.Items {
			projected := HistoryItem{Node: readNode(item.Node), Deferred: item.Entry == nil}
			if item.Entry != nil {
				projected.Message, _, err = ProjectEntryAt(*item.Entry, page.Version.Generation)
				if err != nil {
					return ResourceResponse{}, err
				}
			}
			result.Items = append(result.Items, projected)
		}
		return resourceResponse(result, known)
	case "nodes":
		cursor, err := decodeReadCursor(id, kind, query.Cursor)
		if err != nil {
			return ResourceResponse{}, err
		}
		value, err := api.ReadNodes(id, session.NodeQuery{ParentID: query.ParentID, Role: query.Role, Search: query.Search, At: at, Cursor: cursor.Nodes, Limit: query.Limit})
		if err != nil {
			return ResourceResponse{}, err
		}
		result := NodesPage{SessionID: id, Version: sessionVersion(value.Page.Version), Items: []EntryNode{}, Next: encodeReadCursor(readCursor{SessionID: id, Nodes: value.Page.Next})}
		for _, item := range value.Page.Items {
			result.Items = append(result.Items, readNode(item))
		}
		return resourceResponse(result, known)
	case "metadata":
		value, err := api.ReadSessionMetadata(id)
		if err != nil {
			return ResourceResponse{}, err
		}
		return resourceResponse(map[string]any{"sessionId": id, "version": sessionVersion(value.Version), "info": SessionInfoFromApplication(value.Info), "filePath": value.FilePath, "leafId": value.LeafID}, known)
	case "live":
		value, active, err := api.ReadLiveSession(id)
		if err != nil {
			return ResourceResponse{}, err
		}
		result, err := protocolv1.EncodeLiveSnapshot(value, active)
		if err != nil {
			return ResourceResponse{}, err
		}
		return resourceResponse(result, known)
	case "entry":
		value, err := api.ReadEntry(id, query.EntryID, query.Generation)
		if err != nil {
			return ResourceResponse{}, err
		}
		message, _, err := ProjectEntryAt(value.Entry, value.Version.Generation)
		if err != nil {
			return ResourceResponse{}, err
		}
		return resourceResponse(map[string]any{"sessionId": id, "generation": value.Version.Generation, "entryId": query.EntryID, "message": message}, known)
	case "chunk":
		value, err := api.ReadEntryChunk(id, query.EntryID, query.Generation, query.Offset, query.Length)
		if err != nil {
			return ResourceResponse{}, err
		}
		// Data is base64: a chunk boundary may split a UTF-8 sequence.
		return resourceResponse(map[string]any{"sessionId": id, "generation": value.Version.Generation, "entryId": value.EntryID, "offset": value.Offset, "total": value.Total, "data": value.Data}, known)
	case "thinking":
		value, err := api.SessionThinkingAt(ctx, id, query.EntryID, query.Generation, query.BlockIndex)
		if err != nil {
			return ResourceResponse{}, err
		}
		return resourceResponse(map[string]any{"sessionId": id, "generation": query.Generation, "entryId": query.EntryID, "thinking": value}, known)
	case "image":
		value, err := api.SessionImageAt(ctx, id, query.EntryID, query.Generation, query.BlockIndex)
		if err != nil {
			return ResourceResponse{}, err
		}
		return resourceResponse(map[string]any{"sessionId": id, "generation": query.Generation, "entryId": query.EntryID, "mimeType": value.MIMEType, "data": value.Data}, known)
	default:
		return ResourceResponse{}, ErrInvalidRequest
	}
}

// Every deferred block carries the source generation as well as entry/block
// identity. Old pages cannot silently load media from a replacement source.
func ProjectEntryAt(entry session.Entry, generation string) (json.RawMessage, bool, error) {
	message, ok, err := ProjectEntry(entry, true, true)
	if err != nil || !ok {
		return message, ok, err
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(message, &value); err != nil {
		return nil, false, err
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(value["content"], &blocks) != nil {
		return message, true, nil
	}
	changed := false
	for index, block := range blocks {
		if block == nil {
			continue
		}
		ref, _ := json.Marshal(map[string]any{"generation": generation, "entryId": entry.ID(), "blockIndex": index})
		if _, exists := block["imageRef"]; exists {
			block["imageRef"], changed = ref, true
		}
		if string(block["deferred"]) == "true" && string(block["type"]) == `"thinking"` {
			block["thinkingRef"], changed = ref, true
		}
	}
	if !changed {
		return message, true, nil
	}
	value["content"], err = json.Marshal(blocks)
	if err != nil {
		return nil, false, err
	}
	message, err = json.Marshal(value)
	return message, true, err
}
