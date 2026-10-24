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
	LeafID       *string               `json:"leafId,omitempty"`
	At           *SessionVersion       `json:"at,omitempty"`
	Cursor       string                `json:"cursor,omitempty"`
	AroundID     string                `json:"aroundId,omitempty"`
	Direction    session.PageDirection `json:"direction,omitempty"`
	AfterID      string                `json:"afterId,omitempty"`
	Limit        int                   `json:"limit,omitempty"`
	MaxBytes     int                   `json:"maxBytes,omitempty"`
	EntryType    string                `json:"entryType,omitempty"`
	Tree         bool                  `json:"tree,omitempty"`
	ParentID     *string               `json:"parentId,omitempty"`
	Role         string                `json:"role,omitempty"`
	Search       string                `json:"search,omitempty"`
	EntryID      string                `json:"entryId,omitempty"`
	Generation   string                `json:"generation,omitempty"`
	BlockIndex   int                   `json:"blockIndex,omitempty"`
	RunID        string                `json:"runId,omitempty"`
	MetadataOnly bool                  `json:"metadataOnly,omitempty"`
}

type EntryNode struct {
	ID           string  `json:"id"`
	ParentID     *string `json:"parentId"`
	Type         string  `json:"type"`
	Role         string  `json:"role,omitempty"`
	Timestamp    string  `json:"timestamp"`
	Preview      string  `json:"preview"`
	Label        string  `json:"label,omitempty"`
	HasChildren  bool    `json:"hasChildren"`
	RawBytes     int     `json:"rawBytes"`
	ContentToken string  `json:"contentToken,omitempty"`
	BranchDepth  int     `json:"branchDepth,omitempty"`
	Position     int     `json:"position,omitempty"`
}

func readNode(value session.EntryNode) EntryNode {
	return EntryNode{ID: value.ID, ParentID: value.ParentID, Type: value.Type, Role: value.Role,
		Timestamp: formatWebTime(value.Timestamp), Preview: value.Preview, Label: value.Label,
		HasChildren: value.HasChildren, RawBytes: value.RawBytes, ContentToken: value.ContentToken, BranchDepth: value.BranchDepth, Position: value.Position}
}

type HistoryItem struct {
	Node       EntryNode       `json:"node"`
	Message    json.RawMessage `json:"message,omitempty"`
	Projection string          `json:"projection,omitempty"`
}

type HistoryRun struct {
	ID               string      `json:"id"`
	Header           EntryNode   `json:"header"`
	Result           *EntryNode  `json:"result,omitempty"`
	Start            int         `json:"start"`
	End              int         `json:"end"`
	LastEntryID      string      `json:"lastEntryId"`
	ProcessCount     int         `json:"processCount"`
	ResultHasProcess bool        `json:"resultHasProcess"`
	StopReason       string      `json:"stopReason,omitempty"`
	Anchors          []EntryNode `json:"anchors"`
}

func historyRun(value session.HistoryRun) HistoryRun {
	run := HistoryRun{ID: value.ID, Header: readNode(value.Header), Start: value.Start, End: value.End,
		LastEntryID: value.LastEntryID, ProcessCount: value.ProcessCount, ResultHasProcess: value.ResultHasProcess, StopReason: value.StopReason, Anchors: []EntryNode{}}
	if value.Result != nil {
		node := readNode(*value.Result)
		run.Result = &node
	}
	for _, node := range value.Anchors {
		run.Anchors = append(run.Anchors, readNode(node))
	}
	return run
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
	Run       *HistoryRun    `json:"run,omitempty"`
}

type RunsPage struct {
	HistoryPage
	Runs []HistoryRun `json:"runs"`
}

type NodesPage struct {
	Connected bool           `json:"connected"`
	SessionID string         `json:"sessionId"`
	Version   SessionVersion `json:"version"`
	Items     []EntryNode    `json:"items"`
	Next      string         `json:"next,omitempty"`
}

type readCursor struct {
	SessionID string
	History   *session.HistoryCursor
	Nodes     *session.NodeCursor
	Runs      *session.RunsCursor
}

func encodeReadCursor(value readCursor) string {
	if value.History == nil && value.Nodes == nil && value.Runs == nil {
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
		(kind == "history" && (value.History == nil || value.Nodes != nil || value.Runs != nil)) ||
		(kind == "nodes" && (value.Nodes == nil || value.History != nil || value.Runs != nil)) ||
		(kind == "runs" && (value.Runs == nil || value.History != nil || value.Nodes != nil)) {
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
	case "runs":
		cursor, err := decodeReadCursor(id, kind, query.Cursor)
		if err != nil {
			return ResourceResponse{}, err
		}
		value, err := api.ReadRuns(id, session.RunsQuery{LeafID: query.LeafID, At: at, Cursor: cursor.Runs,
			AroundID: query.AroundID, AfterID: query.AfterID, Direction: query.Direction, MetadataOnly: query.MetadataOnly, Limit: query.Limit, MaxBytes: query.MaxBytes})
		if err != nil {
			return ResourceResponse{}, err
		}
		page := value.Page
		result := RunsPage{HistoryPage: HistoryPage{SessionID: id, Version: sessionVersion(page.Version), AnchorID: page.AnchorID,
			Start: page.Start, End: page.End, Total: page.Total, Connected: page.Connected, Items: []HistoryItem{},
			Older: encodeReadCursor(readCursor{SessionID: id, Runs: page.Older}), Newer: encodeReadCursor(readCursor{SessionID: id, Runs: page.Newer})}, Runs: []HistoryRun{}}
		for _, item := range page.Items {
			result.Runs = append(result.Runs, historyRun(item.Run))
			if item.Header != nil {
				message, _, err := ProjectEntryAt(*item.Header, page.Version.Generation)
				if err != nil {
					return ResourceResponse{}, err
				}
				result.Items = append(result.Items, HistoryItem{Node: readNode(item.Run.Header), Message: message})
			}
			if item.Result != nil {
				message, err := projectRunAnswer(*item.Result, page.Version.Generation)
				if err != nil {
					return ResourceResponse{}, err
				}
				result.Items = append(result.Items, HistoryItem{Node: readNode(*item.Run.Result), Message: message, Projection: "answer"})
			}
		}
		return resourceResponse(result, known)
	case "history":
		cursor, err := decodeReadCursor(id, kind, query.Cursor)
		if err != nil {
			return ResourceResponse{}, err
		}
		value, err := api.ReadHistory(id, session.HistoryQuery{LeafID: query.LeafID, At: at, Cursor: cursor.History,
			AroundID: query.AroundID, AfterID: query.AfterID, Direction: query.Direction, Limit: query.Limit, MaxBytes: query.MaxBytes, RunID: query.RunID})
		if err != nil {
			return ResourceResponse{}, err
		}
		page := value.Page
		result := HistoryPage{SessionID: id, Version: sessionVersion(page.Version), AnchorID: page.AnchorID,
			Start: page.Start, End: page.End, Total: page.Total, Connected: page.Connected, Items: []HistoryItem{},
			Older: encodeReadCursor(readCursor{SessionID: id, History: page.Older}), Newer: encodeReadCursor(readCursor{SessionID: id, History: page.Newer})}
		if page.Run != nil {
			run := historyRun(*page.Run)
			result.Run = &run
		}
		for _, item := range page.Items {
			projected := HistoryItem{Node: readNode(item.Node)}
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
		value, err := api.ReadNodes(id, session.NodeQuery{ParentID: query.ParentID, LeafID: query.LeafID, AfterID: query.AfterID, Type: query.EntryType, Role: query.Role, Search: query.Search, Tree: query.Tree, At: at, Cursor: cursor.Nodes, Limit: query.Limit})
		if err != nil {
			return ResourceResponse{}, err
		}
		result := NodesPage{Connected: value.Page.Connected, SessionID: id, Version: sessionVersion(value.Page.Version), Items: []EntryNode{}, Next: encodeReadCursor(readCursor{SessionID: id, Nodes: value.Page.Next})}
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
		return resourceResponse(map[string]any{"sessionId": id, "generation": value.Version.Generation, "entryId": query.EntryID, "contentToken": value.ContentToken, "message": message}, known)
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

// Overview answers omit the process prefix without changing the stored entry.
// Expanding a run reads that same entry through the complete history projection.
func projectRunAnswer(entry session.Entry, generation string) (json.RawMessage, error) {
	message, ok, err := ProjectEntryAt(entry, generation)
	if err != nil || !ok {
		return message, err
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(message, &value); err != nil {
		return nil, err
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(value["content"], &blocks) != nil {
		return message, nil
	}
	lastProcess := -1
	for index, block := range blocks {
		var kind string
		_ = json.Unmarshal(block["type"], &kind)
		if kind != "text" && kind != "image" {
			lastProcess = index
		}
	}
	if lastProcess < 0 {
		return message, nil
	}
	value["content"], err = json.Marshal(blocks[lastProcess+1:])
	if err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

// Every deferred block carries the source generation as well as entry/block
// identity. Old pages cannot silently load media from a replacement source.
func ProjectEntryAt(entry session.Entry, generation string) (json.RawMessage, bool, error) {
	message, ok, err := ProjectEntry(entry, false, true)
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
