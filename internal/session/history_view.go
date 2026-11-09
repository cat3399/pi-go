package session

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// HistoryViewQuery chooses records, never a character budget. Every selected
// text block is returned whole; output limits belong to the invoking tool.
type HistoryViewQuery struct {
	View       string   `json:"view,omitempty"` // dialogue (default) or raw
	WindowID   string   `json:"window,omitempty"`
	LeafID     *string  `json:"leaf,omitempty"`
	EntryIDs   []string `json:"entries,omitempty"`
	FromID     string   `json:"from,omitempty"` // inclusive, on the selected path
	ToID       string   `json:"to,omitempty"`   // inclusive
	Limit      int      `json:"limit,omitempty"`
	Search     string   `json:"search,omitempty"`
	IgnoreCase bool     `json:"ignoreCase,omitempty"`
}

type HistoryTextBlock struct {
	Index int    `json:"index"`
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	Phase string `json:"phase,omitempty"`
	// Attachments retain their position and type. EntryID + Index is the
	// durable reference, without duplicating inline binary data in a text view.
	MIMEType string `json:"mimeType,omitempty"`
	Name     string `json:"name,omitempty"`
}

type HistoryMatch struct {
	Block  int    `json:"block"`
	Line   int    `json:"line"`   // one-based within this original block
	Offset int    `json:"offset"` // UTF-8 byte offset within this original block
	Text   string `json:"text"`   // complete matching line
}

type HistoryRecord struct {
	EntryID   string             `json:"entryId"`
	WindowID  string             `json:"window"`
	Position  int                `json:"position"`
	Timestamp time.Time          `json:"timestamp"`
	Role      string             `json:"role,omitempty"`
	Type      string             `json:"type"`
	Phase     string             `json:"phase,omitempty"`
	Blocks    []HistoryTextBlock `json:"blocks,omitempty"`
	Raw       json.RawMessage    `json:"raw,omitempty"`
	Matches   []HistoryMatch     `json:"matches,omitempty"`
}

type ContextWindowInfo struct {
	ID            string    `json:"id"`
	Previous      string    `json:"previous,omitempty"`
	BoundaryID    string    `json:"boundaryId,omitempty"`
	BeforeEntryID string    `json:"beforeEntryId,omitempty"`
	Method        string    `json:"method"`
	Timestamp     time.Time `json:"timestamp"`
	Handoff       string    `json:"handoff,omitempty"`
}

type HistoryView struct {
	SessionID string              `json:"sessionId"`
	LeafID    string              `json:"leafId"`
	Version   ReadVersion         `json:"version"`
	View      string              `json:"view"`
	Records   []HistoryRecord     `json:"records"`
	Windows   []ContextWindowInfo `json:"windows"`
	// NextEntryID identifies the first matching record beyond Limit. It is a
	// query range, not a cursor into a partially returned message.
	NextEntryID string `json:"nextEntryId,omitempty"`
}

func (m *SessionManager) ReadHistoryView(query HistoryViewQuery) (HistoryView, error) {
	if query.View == "" {
		query.View = "dialogue"
	}
	if query.View != "dialogue" && query.View != "raw" && query.View != "windows" || query.Limit < 0 {
		return HistoryView{}, ErrInvalidReadQuery
	}
	var result HistoryView
	err := m.readView(func(store *Session, source ReadSnapshot) error {
		anchor := ""
		if source.LeafID != nil {
			anchor = *source.LeafID
		}
		if query.LeafID != nil {
			anchor = *query.LeafID
		}
		path, err := store.readPathLocked(anchor, source.Version.Log)
		if err != nil {
			return err
		}
		result = HistoryView{SessionID: source.Info.ID, LeafID: anchor, Version: source.Version, View: query.View, Records: []HistoryRecord{}, Windows: []ContextWindowInfo{{ID: InitialContextWindow, Method: "initial", Timestamp: store.header.Timestamp()}}}
		windows := make([]string, len(path))
		positions := make(map[string]int, len(path))
		window := InitialContextWindow
		for i, index := range path {
			entry := store.entries[index]
			positions[entry.ID()] = i
			if isContextBoundary(entry) {
				info := ContextWindowInfo{ID: entry.ID(), Previous: window, BoundaryID: entry.ID(), Method: "summary", Timestamp: entry.Timestamp()}
				info.BeforeEntryID, _ = entry.ParentID()
				if handoff, ok := entry.ContextHandoff(); ok {
					info.Method, info.Handoff = "handoff", handoff.Text
				}
				result.Windows = append(result.Windows, info)
				window = entry.ID()
			}
			windows[i] = window
		}
		if query.WindowID == "current" {
			query.WindowID = window
		}
		if query.View == "windows" {
			return nil
		}
		if query.WindowID != "" {
			found := false
			for _, info := range result.Windows {
				found = found || info.ID == query.WindowID
			}
			if !found {
				return fmt.Errorf("%w: context window is outside the selected branch", ErrEntryNotFound)
			}
		}
		start, end := 0, len(path)
		if query.FromID != "" {
			var ok bool
			start, ok = positions[query.FromID]
			if !ok {
				return fmt.Errorf("%w: from entry", ErrEntryNotFound)
			}
		}
		if query.ToID != "" {
			position, ok := positions[query.ToID]
			if !ok {
				return fmt.Errorf("%w: to entry", ErrEntryNotFound)
			}
			end = position + 1
		}
		if end < start {
			return ErrInvalidReadQuery
		}
		selected := make(map[string]bool, len(query.EntryIDs))
		for _, id := range query.EntryIDs {
			position, ok := positions[id]
			if !ok || position < start || position >= end || query.WindowID != "" && windows[position] != query.WindowID {
				return fmt.Errorf("%w: entry %s is outside the query", ErrEntryNotFound, id)
			}
			selected[id] = true
		}
		for i := start; i < end; i++ {
			entry := store.entries[path[i]]
			if query.WindowID != "" && windows[i] != query.WindowID || len(selected) != 0 && !selected[entry.ID()] {
				continue
			}
			if query.View == "dialogue" && (entry.typeName != "message" || entry.messageRole != "user" && entry.messageRole != "assistant") {
				continue
			}
			entry, err = materializeReadEntry(entry)
			if err != nil {
				return err
			}
			record, searchable := projectHistoryRecord(entry, windows[i], i, query.View)
			if query.View == "dialogue" && len(record.Blocks) == 0 {
				continue
			}
			if query.Search != "" {
				record.Matches = historyMatches(searchable, query.Search, query.IgnoreCase)
				if query.View == "raw" && len(record.Matches) == 0 {
					record.Matches = historyMatches([]HistoryTextBlock{{Index: -1, Text: string(record.Raw)}}, query.Search, query.IgnoreCase)
				}
				if len(record.Matches) == 0 {
					continue
				}
				record.Blocks, record.Raw = nil, nil
			}
			if query.Limit > 0 && len(result.Records) >= query.Limit {
				result.NextEntryID = entry.ID()
				break
			}
			result.Records = append(result.Records, record)
		}
		return nil
	})
	return result, err
}

func projectHistoryRecord(entry Entry, window string, position int, view string) (HistoryRecord, []HistoryTextBlock) {
	record := HistoryRecord{EntryID: entry.ID(), WindowID: window, Position: position, Timestamp: entry.Timestamp(), Type: entry.Type()}
	record.Role, _ = entry.MessageRole()
	var envelope struct {
		Message struct {
			Phase   string          `json:"phase"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	_ = json.Unmarshal(entry.RawJSON(), &envelope)
	record.Phase = envelope.Message.Phase
	var plain string
	var content []map[string]json.RawMessage
	if json.Unmarshal(envelope.Message.Content, &plain) == nil {
		record.Blocks = []HistoryTextBlock{{Index: 0, Type: "text", Text: plain, Phase: record.Phase}}
	} else if json.Unmarshal(envelope.Message.Content, &content) == nil {
		for i, block := range content {
			kind := historyString(block["type"])
			if kind == "text" {
				phase := historyString(block["phase"])
				if phase == "" {
					var signature struct {
						Phase string `json:"phase"`
					}
					_ = json.Unmarshal([]byte(historyString(block["textSignature"])), &signature)
					phase = signature.Phase
				}
				if phase == "" {
					phase = record.Phase
				}
				record.Blocks = append(record.Blocks, HistoryTextBlock{Index: i, Type: kind, Text: historyString(block["text"]), Phase: phase})
			} else if kind == "image" || kind == "file" || kind == "audio" || kind == "image_url" || kind == "input_file" || kind == "input_audio" {
				record.Blocks = append(record.Blocks, HistoryTextBlock{Index: i, Type: kind, MIMEType: historyString(block["mimeType"]), Name: historyString(block["name"])})
			}
		}
	}
	searchable := record.Blocks
	if view == "raw" {
		record.Raw, record.Blocks = entry.RawJSON(), nil
		// Raw queries include tool arguments, results, thinking, custom data and
		// original statuses without assembling an inferred execution narrative.
		searchable = nil
		for i, block := range content {
			text := historyString(block["text"])
			if text == "" {
				text = historyString(block["thinking"])
			}
			if text == "" {
				encoded, _ := json.Marshal(block)
				text = string(encoded)
			}
			searchable = append(searchable, HistoryTextBlock{Index: i, Text: text})
		}
		if len(searchable) == 0 {
			searchable = []HistoryTextBlock{{Index: -1, Text: string(record.Raw)}}
		}
	}
	return record, searchable
}

func historyString(raw json.RawMessage) string { var s string; _ = json.Unmarshal(raw, &s); return s }

func historyMatches(blocks []HistoryTextBlock, needle string, ignoreCase bool) []HistoryMatch {
	if ignoreCase {
		needle = strings.ToLower(needle)
	}
	var matches []HistoryMatch
	for _, block := range blocks {
		offset := 0
		for i, line := range strings.Split(block.Text, "\n") {
			candidate := line
			if ignoreCase {
				candidate = strings.ToLower(candidate)
			}
			if strings.Contains(candidate, needle) {
				matches = append(matches, HistoryMatch{Block: block.Index, Line: i + 1, Offset: offset, Text: line})
			}
			offset += len(line) + 1
		}
	}
	return matches
}

func (r *ReadIndex) ReadHistoryView(query HistoryViewQuery) (HistoryView, error) {
	return readIndexed(r, func(manager *SessionManager) (HistoryView, error) { return manager.ReadHistoryView(query) })
}
