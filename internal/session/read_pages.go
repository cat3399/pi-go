package session

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
)

var (
	ErrReadCursorUnavailable = errors.New("session read cursor is unavailable")
	ErrInvalidReadQuery      = errors.New("invalid session read query")
)

const (
	DefaultPageEntries = 64
	MaxPageEntries     = 256
	DefaultPageBytes   = 512 << 10
	MaxPageBytes       = 4 << 20
	MaxEntryChunkBytes = 512 << 10
)

type PageDirection string

const (
	PageOlder PageDirection = "older"
	PageNewer PageDirection = "newer"
)

// HistoryCursor fixes both the log prefix and path anchor. The prefix matters
// even for immutable entries: a previously missing forward parent may arrive
// in a later append and must not change pages already being traversed.
type HistoryCursor struct {
	Version   ReadVersion
	AnchorID  string
	Position  int
	Direction PageDirection
}

type HistoryQuery struct {
	LeafID   *string // nil selects the current leaf; pointer to "" selects root
	At       *ReadVersion
	Cursor   *HistoryCursor
	AroundID string // put this node at the end of the returned window
	AfterID  string // stop before this ancestor when requesting a tail update
	Limit    int
	MaxBytes int
}

type EntryNode struct {
	ID          string
	ParentID    *string
	Type        string
	Role        string
	Timestamp   time.Time
	Preview     string
	Label       string
	HasChildren bool
	RawBytes    int
}

type PageEntry struct {
	Node  EntryNode
	Entry *Entry // nil for an oversized record; ReadEntryChunk supplies all bytes
}

type HistoryPage struct {
	Source    ReadSnapshot // current metadata, independently of the fixed path
	Version   ReadVersion
	AnchorID  string
	Items     []PageEntry
	Start     int
	End       int
	Total     int
	Connected bool // AfterID was an ancestor in this fixed path
	Older     *HistoryCursor
	Newer     *HistoryCursor
}

func readPageLimits(limit, maxBytes int) (int, int, error) {
	if limit < 0 || maxBytes < 0 {
		return 0, 0, ErrInvalidReadQuery
	}
	if limit == 0 {
		limit = DefaultPageEntries
	}
	if maxBytes == 0 {
		maxBytes = DefaultPageBytes
	}
	return min(limit, MaxPageEntries), min(maxBytes, MaxPageBytes), nil
}

func validateReadVersion(current ReadVersion, requested *ReadVersion) (ReadVersion, error) {
	if requested == nil {
		return current, nil
	}
	if requested.Generation != current.Generation || requested.Log > current.Log || requested.View > current.View {
		return ReadVersion{}, ErrReadCursorUnavailable
	}
	return *requested, nil
}

func (m *SessionManager) ReadHistory(query HistoryQuery) (HistoryPage, error) {
	limit, budget, err := readPageLimits(query.Limit, query.MaxBytes)
	if err != nil {
		return HistoryPage{}, err
	}
	var result HistoryPage
	err = m.readView(func(store *Session, source ReadSnapshot) error {
		if query.AroundID != "" && query.AfterID != "" {
			return ErrInvalidReadQuery
		}
		at := query.At
		anchor := ""
		if source.LeafID != nil {
			anchor = *source.LeafID
		}
		if query.LeafID != nil {
			anchor = *query.LeafID
		}
		if cursor := query.Cursor; cursor != nil {
			if query.AroundID != "" || query.AfterID != "" || query.At != nil || query.LeafID != nil && *query.LeafID != cursor.AnchorID {
				return ErrInvalidReadQuery
			}
			at, anchor = &cursor.Version, cursor.AnchorID
		}
		version, err := validateReadVersion(source.Version, at)
		if err != nil {
			return err
		}
		path, err := store.readPathLocked(anchor, version.Log)
		if err != nil {
			return err
		}
		result = HistoryPage{Source: source, Version: version, AnchorID: anchor, Total: len(path), Items: []PageEntry{}, Connected: query.AfterID == ""}
		position, direction := len(path), PageOlder
		if cursor := query.Cursor; cursor != nil {
			position, direction = cursor.Position, cursor.Direction
		}
		floor := 0
		if query.AroundID != "" || query.AfterID != "" {
			foundAround := query.AroundID == ""
			for i, index := range path {
				id := store.entries[index].id
				if id == query.AroundID {
					position, foundAround = i+1, true
				}
				if id == query.AfterID {
					floor, result.Connected = i+1, true
				}
			}
			if !foundAround {
				return fmt.Errorf("%w: node is outside the selected path", ErrEntryNotFound)
			}
		}
		if position < floor || position > len(path) || direction != PageOlder && direction != PageNewer {
			return ErrReadCursorUnavailable
		}
		start, end, used := position, position, 0
		for len(result.Items) < limit {
			index := end
			if direction == PageOlder {
				index = start - 1
			}
			if index < floor || index >= len(path) {
				break
			}
			entry := store.entries[path[index]]
			// Metadata references remain small even for a single huge record.
			// Do not eagerly copy/decode its payload just to return this page.
			item := PageEntry{Node: entryReadNode(entry)}
			// Bound reference metadata too, using a conservative upper bound
			// for JSON escaping. The fixed page envelope is outside this budget.
			metadataBytes := 256 + 6*(len(entry.id)+len(entry.parentID)+len(entry.typeName)+len(entry.messageRole)+len(item.Node.Preview))
			cost, include := len(entry.raw)+metadataBytes, len(entry.raw)+metadataBytes <= budget
			if !include {
				cost = metadataBytes
			}
			if used+cost > budget {
				if len(result.Items) == 0 {
					return fmt.Errorf("%w: byte budget cannot fit an entry reference", ErrInvalidReadQuery)
				}
				break
			}
			if include {
				copy := entry.clone()
				item.Entry = &copy
			}
			result.Items = append(result.Items, item)
			used += cost
			if direction == PageOlder {
				start--
			} else {
				end++
			}
		}
		if direction == PageOlder {
			for left, right := 0, len(result.Items)-1; left < right; left, right = left+1, right-1 {
				result.Items[left], result.Items[right] = result.Items[right], result.Items[left]
			}
		}
		result.Start, result.End = start, end
		if start > 0 {
			result.Older = &HistoryCursor{Version: version, AnchorID: anchor, Position: start, Direction: PageOlder}
		}
		if end < len(path) {
			result.Newer = &HistoryCursor{Version: version, AnchorID: anchor, Position: end, Direction: PageNewer}
		}
		return nil
	})
	return result, err
}

func (s *Session) readPathLocked(anchor string, log uint64) ([]int, error) {
	if anchor == "" {
		return nil, nil
	}
	index, exists := s.byID[anchor]
	if !exists || uint64(index) >= log {
		return nil, fmt.Errorf("%w: %s", ErrEntryNotFound, anchor)
	}
	path := make([]int, 0, min(int(log), 256))
	seen := make(map[int]struct{})
	for index >= 0 && uint64(index) < log {
		if _, duplicate := seen[index]; duplicate {
			break // same cycle-safe path boundary as BranchPath
		}
		seen[index] = struct{}{}
		path = append(path, index)
		entry := s.entries[index]
		if !entry.hasParent {
			break
		}
		parent, found := s.byID[entry.parentID]
		if !found || uint64(parent) >= log {
			break
		}
		index = parent
	}
	for left, right := 0, len(path)-1; left < right; left, right = left+1, right-1 {
		path[left], path[right] = path[right], path[left]
	}
	return path, nil
}

type NodeQuery struct {
	ParentID *string // nil is the complete log, "" is the forest roots
	Role     string
	Search   string
	At       *ReadVersion
	Cursor   *NodeCursor
	Limit    int
}

type NodeCursor struct {
	Version  ReadVersion
	Position int
	ParentID *string
	Role     string
	Search   string
}

type NodePage struct {
	Source  ReadSnapshot
	Version ReadVersion
	Items   []EntryNode
	Next    *NodeCursor
}

func (m *SessionManager) ReadNodes(query NodeQuery) (NodePage, error) {
	limit, _, err := readPageLimits(query.Limit, 0)
	if err != nil {
		return NodePage{}, err
	}
	var result NodePage
	err = m.readView(func(store *Session, source ReadSnapshot) error {
		position, at := 0, query.At
		if cursor := query.Cursor; cursor != nil {
			if query.At != nil || !equalReadString(query.ParentID, cursor.ParentID) || query.Role != cursor.Role || query.Search != cursor.Search {
				return ErrInvalidReadQuery
			}
			position, at = cursor.Position, &cursor.Version
		}
		version, err := validateReadVersion(source.Version, at)
		if err != nil {
			return err
		}
		if position < 0 || uint64(position) > version.Log {
			return ErrReadCursorUnavailable
		}
		labels := make(map[string]string)
		children := make(map[string]bool)
		for _, entry := range store.entries[:version.Log] {
			if label, ok := entry.payload.(LabelPayload); ok {
				if label.Label == nil {
					delete(labels, label.TargetID)
				} else {
					labels[label.TargetID] = *label.Label
				}
			}
			if entry.hasParent {
				children[entry.parentID] = true
			}
		}
		result = NodePage{Source: source, Version: version, Items: []EntryNode{}}
		needle := []byte(strings.ToLower(query.Search))
		for position < int(version.Log) {
			entry := store.entries[position]
			position++
			parent := ""
			if parentIndex, exists := store.byID[entry.parentID]; entry.hasParent && exists && uint64(parentIndex) < version.Log {
				parent = entry.parentID
			}
			if query.ParentID != nil && *query.ParentID != parent || query.Role != "" && query.Role != entry.messageRole {
				continue
			}
			if len(needle) != 0 && !bytes.Contains(bytes.ToLower(entry.raw), needle) {
				continue
			}
			node := entryReadNode(entry)
			node.Label, node.HasChildren = labels[entry.id], children[entry.id]
			result.Items = append(result.Items, node)
			if len(result.Items) == limit {
				break
			}
		}
		if position < int(version.Log) {
			var parent *string
			if query.ParentID != nil {
				value := *query.ParentID
				parent = &value
			}
			result.Next = &NodeCursor{Version: version, Position: position, ParentID: parent, Role: query.Role, Search: query.Search}
		}
		return nil
	})
	return result, err
}

func equalReadString(left, right *string) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

type EntryChunk struct {
	Version   ReadVersion
	SessionID string
	EntryID   string
	Offset    int
	Total     int
	Data      []byte
}

func (m *SessionManager) ReadEntryChunk(id, generation string, offset, length int) (EntryChunk, error) {
	if offset < 0 || length < 0 {
		return EntryChunk{}, ErrInvalidReadQuery
	}
	if length == 0 {
		length = 64 << 10
	}
	length = min(length, MaxEntryChunkBytes)
	var result EntryChunk
	err := m.readView(func(store *Session, source ReadSnapshot) error {
		if generation != "" && generation != source.Version.Generation {
			return ErrReadCursorUnavailable
		}
		index, exists := store.byID[id]
		if !exists {
			return ErrEntryNotFound
		}
		raw := store.entries[index].raw
		if offset > len(raw) {
			return ErrInvalidReadQuery
		}
		end := offset + min(length, len(raw)-offset)
		result = EntryChunk{Version: source.Version, SessionID: source.Info.ID, EntryID: id, Offset: offset, Total: len(raw), Data: bytes.Clone(raw[offset:end])}
		return nil
	})
	return result, err
}

func entryReadNode(entry Entry) EntryNode {
	node := EntryNode{ID: entry.id, Type: entry.typeName, Role: entry.messageRole, Timestamp: entry.timestamp, RawBytes: len(entry.raw), Preview: entryPreview(entry)}
	if entry.hasParent {
		id := entry.parentID
		node.ParentID = &id
	}
	return node
}

func entryPreview(entry Entry) string {
	var text string
	switch message := entry.message.(type) {
	case interface{ Content() []llm.TextBlock }:
		if blocks := message.Content(); len(blocks) != 0 {
			text = blocks[0].Text()
		}
	case llm.UserContentMessage:
		for _, block := range message.Content() {
			if value, ok := block.(llm.TextBlock); ok {
				text = value.Text()
				break
			}
		}
	case llm.ToolResultContentMessage:
		for _, block := range message.Content() {
			if value, ok := block.(llm.TextBlock); ok {
				text = value.Text()
				break
			}
		}
	case interface{ Blocks() []llm.AssistantBlock }:
		for _, block := range message.Blocks() {
			switch value := block.(type) {
			case llm.TextBlock:
				text = value.Text()
			case llm.ToolCallBlock:
				text = value.Name()
			}
			if text != "" {
				break
			}
		}
	}
	if text == "" {
		switch payload := entry.payload.(type) {
		case CustomMessagePayload:
			text, _ = payload.Message.StringContent()
			if text == "" {
				text = payload.Message.CustomType()
			}
		case CompactionPayload:
			text = payload.Record.Summary
		case BranchSummaryPayload:
			text = payload.Summary
		case MessagePayload:
			if bash, ok := payload.Message.(agentmsg.BashExecution); ok {
				text = bash.Command
			}
		}
	}
	end := 0
	for count := 0; end < len(text) && count < 160; count++ {
		_, size := utf8.DecodeRuneInString(text[end:])
		end += size
	}
	return strings.Clone(text[:end])
}
