package surfacewire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cat3399/pi-go/internal/application"
	protocolv1 "github.com/cat3399/pi-go/internal/protocol/v1"
	"github.com/cat3399/pi-go/internal/session"
)

const MaxProjectedTreeDepth = 200

type SelectedModel struct {
	Provider string `json:"provider"`
	ModelID  string `json:"modelId"`
}

type SessionInfo struct {
	Path            string `json:"path"`
	ID              string `json:"id"`
	CWD             string `json:"cwd"`
	Name            string `json:"name,omitempty"`
	Created         string `json:"created"`
	Modified        string `json:"modified"`
	MessageCount    int    `json:"messageCount"`
	FirstMessage    string `json:"firstMessage"`
	ParentSessionID string `json:"parentSessionId,omitempty"`
	ProjectRoot     string `json:"projectRoot"`
}

type SessionContext struct {
	Messages      []json.RawMessage `json:"messages"`
	EntryIDs      []string          `json:"entryIds"`
	ThinkingLevel string            `json:"thinkingLevel"`
	Model         *SelectedModel    `json:"model"`
}

type TreeNode struct {
	Entry              json.RawMessage `json:"entry"`
	Children           []*TreeNode     `json:"children"`
	Label              string          `json:"label,omitempty"`
	CompressedEntryIDs []string        `json:"compressedEntryIds,omitempty"`
}

type SessionView struct {
	Revision  uint64         `json:"revision"`
	SessionID string         `json:"sessionId"`
	FilePath  string         `json:"filePath"`
	Info      SessionInfo    `json:"info"`
	LeafID    *string        `json:"leafId"`
	Tree      []*TreeNode    `json:"tree"`
	Context   SessionContext `json:"context"`
	Running   bool           `json:"running"`
	State     map[string]any `json:"state,omitempty"`
}

func ListSessions(api application.API) ([]SessionInfo, error) {
	values, err := api.ListSessions()
	if err != nil {
		return nil, err
	}
	result := make([]SessionInfo, 0, len(values))
	for _, value := range values {
		result = append(result, SessionInfoFromApplication(value))
	}
	return result, nil
}

func SessionInfoFromApplication(info application.SessionInfo) SessionInfo {
	return SessionInfo{
		Path: info.Path, ID: info.ID, CWD: info.CWD, Name: info.Name,
		Created: formatWebTime(info.Created), Modified: formatWebTime(info.Modified),
		MessageCount: info.MessageCount, FirstMessage: info.FirstMessage,
		ParentSessionID: info.ParentSessionID, ProjectRoot: info.CWD,
	}
}

func formatWebTime(value time.Time) string {
	if value.IsZero() {
		return time.Unix(0, 0).UTC().Format(time.RFC3339Nano)
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func SessionViewFor(api application.API, id, leafID string, deferThinking, deferMedia bool) (SessionView, error) {
	snapshot, err := api.SnapshotSession(id, leafID)
	if err != nil {
		return SessionView{}, err
	}
	contextValue, err := BuildSessionContext(snapshot, deferThinking, deferMedia)
	if err != nil {
		return SessionView{}, err
	}
	tree, err := ProjectSessionTree(snapshot.Tree)
	if err != nil {
		return SessionView{}, err
	}
	result := SessionView{
		Revision: snapshot.Revision, SessionID: id, FilePath: snapshot.FilePath,
		Info: SessionInfoFromApplication(snapshot.Info), LeafID: snapshot.LeafID,
		Tree: tree, Context: contextValue, Running: snapshot.LiveState != nil,
	}
	if snapshot.LiveState != nil {
		result.State = protocolv1.EncodeState(*snapshot.LiveState)
	}
	return result, nil
}

func SessionContextFor(api application.API, id, leafID string, deferThinking, deferMedia bool) (SessionContext, error) {
	snapshot, err := api.SnapshotSession(id, leafID)
	if err != nil {
		return SessionContext{}, err
	}
	return BuildSessionContext(snapshot, deferThinking, deferMedia)
}

func BuildSessionContext(snapshot application.SessionSnapshot, deferThinking, deferMedia bool) (SessionContext, error) {
	value := snapshot.Context
	result := SessionContext{Messages: []json.RawMessage{}, EntryIDs: []string{}, ThinkingLevel: "off"}
	if thinking, ok := value.ThinkingLevel(); ok {
		result.ThinkingLevel = thinking
	}
	if model, ok := value.Model(); ok {
		result.Model = &SelectedModel{Provider: model.Provider, ModelID: model.ModelID}
	}
	if snapshot.LiveState != nil {
		result.ThinkingLevel = string(snapshot.LiveState.ThinkingLevel)
		if snapshot.LiveState.HasModel {
			result.Model = &SelectedModel{Provider: snapshot.LiveState.Model.Provider(), ModelID: snapshot.LiveState.Model.ID()}
		}
	}
	for _, entry := range snapshot.Entries {
		message, ok, err := ProjectEntry(entry, deferThinking, deferMedia)
		if err != nil {
			return SessionContext{}, err
		}
		if !ok {
			continue
		}
		result.Messages = append(result.Messages, message)
		result.EntryIDs = append(result.EntryIDs, entry.ID())
	}
	return result, nil
}

func ProjectEntry(entry session.Entry, deferThinking, deferMedia bool) (json.RawMessage, bool, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(entry.RawJSON(), &object); err != nil {
		return nil, false, fmt.Errorf("decode session entry %s: %w", entry.ID(), err)
	}
	switch entry.Type() {
	case "message":
		message := object["message"]
		if len(message) == 0 || !json.Valid(message) {
			return nil, false, nil
		}
		normalized, err := NormalizeHistoryToolCalls(message)
		if err != nil {
			return nil, false, err
		}
		if !deferThinking && !deferMedia {
			return normalized, true, nil
		}
		deferred, err := DeferHistoryMedia(normalized, deferThinking, deferMedia)
		return deferred, true, err
	case "compaction":
		var payload struct {
			Summary          string `json:"summary"`
			FirstKeptEntryID string `json:"firstKeptEntryId"`
			TokensBefore     uint64 `json:"tokensBefore"`
		}
		if err := json.Unmarshal(entry.RawJSON(), &payload); err != nil {
			return nil, false, err
		}
		return marshalRaw(map[string]any{
			"role": "custom", "customType": "compaction", "content": payload.Summary, "display": true,
			"details":   map[string]any{"tokensBefore": payload.TokensBefore, "firstKeptEntryId": payload.FirstKeptEntryID},
			"timestamp": entry.Timestamp().UnixMilli(),
		})
	case "branch_summary":
		var payload struct {
			Summary string `json:"summary"`
		}
		if json.Unmarshal(entry.RawJSON(), &payload) != nil || payload.Summary == "" {
			return nil, false, nil
		}
		return marshalRaw(map[string]any{
			"role": "user", "content": "*The conversation briefly explored another branch and returned with this summary:*\n\n" + payload.Summary,
			"timestamp": entry.Timestamp().UnixMilli(),
		})
	case "custom_message":
		var payload struct {
			CustomType string          `json:"customType"`
			Content    json.RawMessage `json:"content"`
			Display    bool            `json:"display"`
			Details    json.RawMessage `json:"details"`
		}
		if err := json.Unmarshal(entry.RawJSON(), &payload); err != nil {
			return nil, false, err
		}
		value := map[string]any{
			"role": "custom", "customType": payload.CustomType, "content": payload.Content,
			"display": payload.Display, "timestamp": entry.Timestamp().UnixMilli(),
		}
		if len(payload.Details) != 0 {
			value["details"] = payload.Details
		}
		return marshalRaw(value)
	default:
		return nil, false, nil
	}
}

// normalizeHistoryToolCalls ports pi-web's session-reader normalization. Pi's
// durable message format stores tool calls as id/name/arguments, while the Web
// view consumes toolCallId/toolName/input. Live events perform the same
// projection in the browser before rendering.
func NormalizeHistoryToolCalls(message json.RawMessage) (json.RawMessage, error) {
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(message))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if value["role"] != "assistant" {
		return append(json.RawMessage(nil), message...), nil
	}
	content, ok := value["content"].([]any)
	if !ok {
		return append(json.RawMessage(nil), message...), nil
	}
	for index, rawBlock := range content {
		block, ok := rawBlock.(map[string]any)
		if !ok || block["type"] != "toolCall" {
			continue
		}
		toolCallID, ok := block["toolCallId"].(string)
		if !ok {
			toolCallID, _ = block["id"].(string)
		}
		toolName, ok := block["toolName"].(string)
		if !ok {
			toolName, _ = block["name"].(string)
		}
		input, ok := block["input"].(map[string]any)
		if !ok {
			input, _ = block["arguments"].(map[string]any)
		}
		if input == nil {
			input = map[string]any{}
		}
		content[index] = map[string]any{
			"type": "toolCall", "toolCallId": toolCallID,
			"toolName": toolName, "input": input,
		}
	}
	encoded, err := json.Marshal(value)
	return json.RawMessage(encoded), err
}

func marshalRaw(value any) (json.RawMessage, bool, error) {
	encoded, err := json.Marshal(value)
	return json.RawMessage(encoded), err == nil, err
}

func DeferHistoryMedia(message json.RawMessage, deferThinking, deferMedia bool) (json.RawMessage, error) {
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(message))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	role, _ := value["role"].(string)
	content, _ := value["content"].([]any)
	if deferThinking && role == "assistant" {
		for _, rawBlock := range content {
			block, _ := rawBlock.(map[string]any)
			thinking, _ := block["thinking"].(string)
			if block["type"] == "thinking" && strings.TrimSpace(thinking) != "" {
				block["thinking"] = ""
				block["deferred"] = true
			}
		}
	}
	if deferMedia && role == "toolResult" {
		filtered := make([]any, 0, len(content)+1)
		omitted := 0
		var omittedBytes int64
		mimes := make([]string, 0)
		seenMimes := make(map[string]struct{})
		for _, rawBlock := range content {
			block, _ := rawBlock.(map[string]any)
			imageBytes, mime, ok := historyImageData(block)
			if !ok {
				filtered = append(filtered, rawBlock)
				continue
			}
			omitted++
			omittedBytes += imageBytes
			if mime != "" {
				if _, exists := seenMimes[mime]; !exists {
					seenMimes[mime] = struct{}{}
					mimes = append(mimes, mime)
				}
			}
		}
		if omitted != 0 {
			plural := "s"
			if omitted == 1 {
				plural = ""
			}
			mimeText := ""
			if len(mimes) != 0 {
				mimeText = ": " + strings.Join(mimes, ", ")
			}
			filtered = append(filtered, map[string]any{
				"type": "text",
				"text": fmt.Sprintf("[%d tool result image%s omitted from initial history payload%s, ~%d bytes]", omitted, plural, mimeText, omittedBytes),
			})
			value["content"] = filtered
		}
	}
	return json.Marshal(value)
}

func historyImageData(block map[string]any) (bytes int64, mime string, ok bool) {
	if block == nil || block["type"] != "image" {
		return 0, "", false
	}
	data, _ := block["data"].(string)
	if data != "" {
		mime, _ = block["mimeType"].(string)
	} else {
		source, _ := block["source"].(map[string]any)
		if source == nil || source["type"] != "base64" {
			return 0, "", false
		}
		data, _ = source["data"].(string)
		mime, _ = source["media_type"].(string)
	}
	if data == "" {
		return 0, "", false
	}
	padding := int64(0)
	if strings.HasSuffix(data, "==") {
		padding = 2
	} else if strings.HasSuffix(data, "=") {
		padding = 1
	}
	size := int64(len(data))*3/4 - padding
	if size < 0 {
		size = 0
	}
	return size, mime, true
}

func ProjectSessionTree(forest []session.TreeNode) ([]*TreeNode, error) {
	result := make([]*TreeNode, 0, len(forest))
	type projectionTask struct {
		source    *session.TreeNode
		projected *TreeNode
		depth     int
	}
	tasks := make([]projectionTask, 0, len(forest))
	for index := range forest {
		projected, err := cloneProjectedTreeNode(&forest[index], nil)
		if err != nil {
			return nil, err
		}
		result = append(result, projected)
		tasks = append(tasks, projectionTask{source: &forest[index], projected: projected, depth: 1})
	}
	for len(tasks) != 0 {
		last := len(tasks) - 1
		task := tasks[last]
		tasks = tasks[:last]
		for childIndex := range task.source.Children {
			child := &task.source.Children[childIndex]
			if task.depth >= MaxProjectedTreeDepth {
				flattened, err := flattenKeptDescendants(child)
				if err != nil {
					return nil, err
				}
				task.projected.Children = append(task.projected.Children, flattened...)
				continue
			}
			compressed := make([]string, 0)
			for len(child.Children) == 1 {
				compressed = append(compressed, child.Entry.ID())
				child = &child.Children[0]
			}
			projected, err := cloneProjectedTreeNode(child, compressed)
			if err != nil {
				return nil, err
			}
			task.projected.Children = append(task.projected.Children, projected)
			tasks = append(tasks, projectionTask{source: child, projected: projected, depth: task.depth + 1})
		}
	}
	return result, nil
}

func cloneProjectedTreeNode(node *session.TreeNode, compressed []string) (*TreeNode, error) {
	entry := node.Entry.RawJSON()
	if !json.Valid(entry) {
		return nil, fmt.Errorf("session tree entry %s is not valid JSON", node.Entry.ID())
	}
	projected := &TreeNode{
		Entry: append(json.RawMessage(nil), entry...), Children: []*TreeNode{},
		CompressedEntryIDs: append([]string(nil), compressed...),
	}
	if node.Label != nil {
		projected.Label = *node.Label
	}
	return projected, nil
}

func flattenKeptDescendants(root *session.TreeNode) ([]*TreeNode, error) {
	type flattenTask struct {
		node       *session.TreeNode
		compressed []string
	}
	result := make([]*TreeNode, 0)
	pending := []flattenTask{{node: root}}
	for len(pending) != 0 {
		last := len(pending) - 1
		task := pending[last]
		pending = pending[:last]
		keep := len(task.node.Children) != 1
		if keep {
			projected, err := cloneProjectedTreeNode(task.node, task.compressed)
			if err != nil {
				return nil, err
			}
			result = append(result, projected)
		}
		for index := len(task.node.Children) - 1; index >= 0; index-- {
			compressed := []string(nil)
			if !keep {
				compressed = append(append([]string(nil), task.compressed...), task.node.Entry.ID())
			}
			pending = append(pending, flattenTask{node: &task.node.Children[index], compressed: compressed})
		}
	}
	return result, nil
}

func StateModel(state application.State) *SelectedModel {
	if !state.HasModel {
		return nil
	}
	return &SelectedModel{Provider: state.Model.Provider(), ModelID: state.Model.ID()}
}
