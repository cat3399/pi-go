package webui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cat3399/pi-go/internal/host"
	"github.com/cat3399/pi-go/internal/session"
)

const maxProjectedTreeDepth = 200

type selectedModelWire struct {
	Provider string `json:"provider"`
	ModelID  string `json:"modelId"`
}

type sessionInfoWire struct {
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

type sessionContextWire struct {
	Messages      []json.RawMessage  `json:"messages"`
	EntryIDs      []string           `json:"entryIds"`
	ThinkingLevel string             `json:"thinkingLevel"`
	Model         *selectedModelWire `json:"model"`
}

type treeNodeWire struct {
	Entry              json.RawMessage `json:"entry"`
	Children           []*treeNodeWire `json:"children"`
	Label              string          `json:"label,omitempty"`
	CompressedEntryIDs []string        `json:"compressedEntryIds,omitempty"`
}

type sessionViewWire struct {
	SessionID string             `json:"sessionId"`
	FilePath  string             `json:"filePath"`
	Info      sessionInfoWire    `json:"info"`
	LeafID    *string            `json:"leafId"`
	Tree      []*treeNodeWire    `json:"tree"`
	Context   sessionContextWire `json:"context"`
}

func (s *Supervisor) ListSessions() ([]sessionInfoWire, error) {
	discovered, err := session.ListAllSessionsInAgentDir(s.paths.AgentDir, nil)
	if err != nil {
		return nil, err
	}
	pathIDs := make(map[string]string, len(discovered))
	for _, info := range discovered {
		pathIDs[cleanPathKey(info.Path)] = info.ID
	}
	byID := make(map[string]sessionInfoWire, len(discovered))
	for _, info := range discovered {
		byID[info.ID] = sessionInfoToWire(info, pathIDs)
	}
	for _, managed := range s.ActiveSessions() {
		managedID, _, _ := managed.identity()
		if _, exists := byID[managedID]; exists {
			continue
		}
		manager := managed.manager()
		if manager == nil {
			continue
		}
		byID[managedID] = activeSessionInfo(manager, managed, pathIDs)
	}
	result := make([]sessionInfoWire, 0, len(byID))
	for _, info := range byID {
		result = append(result, info)
	}
	sort.SliceStable(result, func(left, right int) bool { return result[left].Modified > result[right].Modified })
	return result, nil
}

func sessionInfoToWire(info session.SessionInfo, pathIDs map[string]string) sessionInfoWire {
	parentID := ""
	if info.HasParentSession {
		parentID = pathIDs[cleanPathKey(info.ParentSessionPath)]
	}
	name := ""
	if info.HasName {
		name = info.Name
	}
	first := info.FirstMessage
	if first == "" {
		first = "(no messages)"
	}
	return sessionInfoWire{
		Path: info.Path, ID: info.ID, CWD: info.Cwd, Name: name,
		Created: formatWebTime(info.Created), Modified: formatWebTime(info.Modified),
		MessageCount: info.MessageCount, FirstMessage: first,
		ParentSessionID: parentID, ProjectRoot: info.Cwd,
	}
}

func activeSessionInfo(manager *session.SessionManager, managed *managedSession, pathIDs map[string]string) sessionInfoWire {
	header := manager.Header()
	created := header.Timestamp()
	var lastActivity time.Time
	entries := manager.Entries()
	messageCount := 0
	firstMessage := "(no messages)"
	for _, entry := range entries {
		if entry.Type() != "message" {
			continue
		}
		messageCount++
		text, role, activity, hasContent := sessionEntryMessage(entry.RawJSON())
		if (role == "user" || role == "assistant") && hasContent && activity.After(time.UnixMilli(0)) && activity.After(lastActivity) {
			lastActivity = activity
		}
		if firstMessage == "(no messages)" && role == "user" && text != "" {
			firstMessage = text
		}
	}
	modified := created
	if !lastActivity.IsZero() {
		modified = lastActivity
	}
	name, _ := manager.SessionName()
	parentID := ""
	if parent, ok := header.ParentSession(); ok {
		parentID = pathIDs[cleanPathKey(parent)]
	}
	_, _, path := managed.identity()
	if current, ok := manager.SessionFile(); ok {
		path = current
	}
	return sessionInfoWire{
		Path: path, ID: manager.SessionID(), CWD: manager.Cwd(), Name: name,
		Created: formatWebTime(created), Modified: formatWebTime(modified),
		MessageCount: messageCount, FirstMessage: firstMessage,
		ParentSessionID: parentID, ProjectRoot: manager.Cwd(),
	}
}

func cleanPathKey(value string) string {
	if value == "" {
		return ""
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return filepath.Clean(value)
	}
	return filepath.Clean(absolute)
}

func formatWebTime(value time.Time) string {
	if value.IsZero() {
		return time.Unix(0, 0).UTC().Format(time.RFC3339Nano)
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func sessionEntryMessage(raw []byte) (text, role string, activity time.Time, hasContent bool) {
	var entry map[string]json.RawMessage
	if json.Unmarshal(raw, &entry) != nil {
		return "", "", time.Time{}, false
	}
	var message map[string]json.RawMessage
	if json.Unmarshal(entry["message"], &message) != nil {
		return "", "", time.Time{}, false
	}
	_ = json.Unmarshal(message["role"], &role)
	content, hasContent := message["content"]
	var milliseconds int64
	if json.Unmarshal(message["timestamp"], &milliseconds) == nil {
		activity = time.UnixMilli(milliseconds)
	} else {
		var timestamp string
		_ = json.Unmarshal(entry["timestamp"], &timestamp)
		activity, _ = time.Parse(time.RFC3339, timestamp)
	}
	if json.Unmarshal(content, &text) == nil {
		return text, role, activity, hasContent
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return "", role, activity, hasContent
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, " "), role, activity, hasContent
}

func (s *Supervisor) SessionView(id, leafID string, deferThinking, deferMedia bool) (sessionViewWire, error) {
	manager, managed, info, closeManager, err := s.sessionManagerForRead(id)
	if err != nil {
		return sessionViewWire{}, err
	}
	if closeManager {
		defer manager.Close()
	}
	contextValue, err := buildSessionContext(manager, managed, leafID, deferThinking, deferMedia)
	if err != nil {
		return sessionViewWire{}, err
	}
	tree, err := projectSessionTree(manager.Tree())
	if err != nil {
		return sessionViewWire{}, err
	}
	var leaf *string
	if value, ok := manager.LeafID(); ok {
		copy := value
		leaf = &copy
	}
	file, _ := manager.SessionFile()
	return sessionViewWire{
		SessionID: id, FilePath: file, Info: info, LeafID: leaf,
		Tree: tree, Context: contextValue,
	}, nil
}

func (s *Supervisor) SessionContext(id, leafID string, deferThinking, deferMedia bool) (sessionContextWire, error) {
	manager, managed, _, closeManager, err := s.sessionManagerForRead(id)
	if err != nil {
		return sessionContextWire{}, err
	}
	if closeManager {
		defer manager.Close()
	}
	return buildSessionContext(manager, managed, leafID, deferThinking, deferMedia)
}

func (s *Supervisor) sessionManagerForRead(id string) (*session.SessionManager, *managedSession, sessionInfoWire, bool, error) {
	if managed, ok := s.Active(id); ok {
		manager := managed.manager()
		if manager == nil {
			return nil, nil, sessionInfoWire{}, false, errors.New("active session manager is unavailable")
		}
		infos, err := s.ListSessions()
		if err != nil {
			return nil, nil, sessionInfoWire{}, false, err
		}
		for _, info := range infos {
			if info.ID == id {
				return manager, managed, info, false, nil
			}
		}
		return manager, managed, activeSessionInfo(manager, managed, nil), false, nil
	}
	info, found, err := s.findSession(id)
	if err != nil {
		return nil, nil, sessionInfoWire{}, false, err
	}
	if !found {
		return nil, nil, sessionInfoWire{}, false, os.ErrNotExist
	}
	manager, err := session.OpenSessionManager(info.Path, filepath.Dir(info.Path), "")
	if err != nil {
		return nil, nil, sessionInfoWire{}, false, err
	}
	all, err := session.ListAllSessionsInAgentDir(s.paths.AgentDir, nil)
	if err != nil {
		_ = manager.Close()
		return nil, nil, sessionInfoWire{}, false, err
	}
	pathIDs := make(map[string]string, len(all))
	for _, item := range all {
		pathIDs[cleanPathKey(item.Path)] = item.ID
	}
	return manager, nil, sessionInfoToWire(info, pathIDs), true, nil
}

func buildSessionContext(manager *session.SessionManager, managed *managedSession, leafID string, deferThinking, deferMedia bool) (sessionContextWire, error) {
	var (
		entries []session.Entry
		value   session.Context
	)
	if leafID == "" {
		entries = manager.ContextEntries()
		value = manager.BuildContext()
	} else {
		projection, err := manager.ProjectContextAt(leafID)
		if err != nil {
			return sessionContextWire{}, err
		}
		entries, value = projection.Entries, projection.Context
	}
	result := sessionContextWire{Messages: []json.RawMessage{}, EntryIDs: []string{}, ThinkingLevel: "off"}
	if thinking, ok := value.ThinkingLevel(); ok {
		result.ThinkingLevel = thinking
	}
	if model, ok := value.Model(); ok {
		result.Model = &selectedModelWire{Provider: model.Provider, ModelID: model.ModelID}
	}
	if managed != nil {
		if state, err := managed.host.State(); err == nil {
			result.ThinkingLevel = string(state.ThinkingLevel)
			if state.HasModel {
				result.Model = &selectedModelWire{Provider: state.Model.Provider(), ModelID: state.Model.ID()}
			}
		}
	}
	for _, entry := range entries {
		message, ok, err := entryToUIMessage(entry, deferThinking, deferMedia)
		if err != nil {
			return sessionContextWire{}, err
		}
		if !ok {
			continue
		}
		result.Messages = append(result.Messages, message)
		result.EntryIDs = append(result.EntryIDs, entry.ID())
	}
	return result, nil
}

func entryToUIMessage(entry session.Entry, deferThinking, deferMedia bool) (json.RawMessage, bool, error) {
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
		if !deferThinking && !deferMedia {
			return append(json.RawMessage(nil), message...), true, nil
		}
		deferred, err := deferHistoryMedia(message, deferThinking, deferMedia)
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

func marshalRaw(value any) (json.RawMessage, bool, error) {
	encoded, err := json.Marshal(value)
	return json.RawMessage(encoded), err == nil, err
}

func deferHistoryMedia(message json.RawMessage, deferThinking, deferMedia bool) (json.RawMessage, error) {
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

func projectSessionTree(forest []session.TreeNode) ([]*treeNodeWire, error) {
	result := make([]*treeNodeWire, 0, len(forest))
	type projectionTask struct {
		source    *session.TreeNode
		projected *treeNodeWire
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
			if task.depth >= maxProjectedTreeDepth {
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

func cloneProjectedTreeNode(node *session.TreeNode, compressed []string) (*treeNodeWire, error) {
	entry := node.Entry.RawJSON()
	if !json.Valid(entry) {
		return nil, fmt.Errorf("session tree entry %s is not valid JSON", node.Entry.ID())
	}
	projected := &treeNodeWire{
		Entry: append(json.RawMessage(nil), entry...), Children: []*treeNodeWire{},
		CompressedEntryIDs: append([]string(nil), compressed...),
	}
	if node.Label != nil {
		projected.Label = *node.Label
	}
	return projected, nil
}

func flattenKeptDescendants(root *session.TreeNode) ([]*treeNodeWire, error) {
	type flattenTask struct {
		node       *session.TreeNode
		compressed []string
	}
	result := make([]*treeNodeWire, 0)
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

func stateModel(state host.State) *selectedModelWire {
	if !state.HasModel {
		return nil
	}
	return &selectedModelWire{Provider: state.Model.Provider(), ModelID: state.Model.ID()}
}
