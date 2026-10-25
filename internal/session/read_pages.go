package session

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
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
	DefaultPageEntries = 128
	MaxPageEntries     = 256
	DefaultPageBytes   = 512 << 10
	MaxPageBytes       = 4 << 20
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
	RunID     string
}

type HistoryQuery struct {
	LeafID    *string // nil selects the current leaf; pointer to "" selects root
	At        *ReadVersion
	Cursor    *HistoryCursor
	AroundID  string        // put this node at the end of the returned window
	AfterID   string        // stop before this ancestor when requesting a tail update
	Direction PageDirection // optional forward tail reads after AfterID
	Limit     int
	MaxBytes  int
	RunID     string // restrict the page to one complete historical browsing group
}

type EntryNode struct {
	ID           string
	ParentID     *string
	Type         string
	Role         string
	Timestamp    time.Time
	Preview      string
	Label        string
	HasChildren  bool
	RawBytes     int
	ContentToken string // raw immutable entry identity; supplied with history/entry reads
	BranchDepth  int
	Position     int // position in this node query
}

type PageEntry struct {
	Node  EntryNode
	Entry *Entry // complete entry; the byte budget is a soft page boundary
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
	Run       *HistoryRun
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
		if query.AroundID != "" && query.AfterID != "" || query.Direction != "" && query.Direction != PageOlder && query.Direction != PageNewer {
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
			if query.AroundID != "" || query.AfterID != "" || query.Direction != "" || query.At != nil || query.LeafID != nil && *query.LeafID != cursor.AnchorID || query.RunID != "" && query.RunID != cursor.RunID {
				return ErrInvalidReadQuery
			}
			at, anchor = &cursor.Version, cursor.AnchorID
			query.RunID = cursor.RunID
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
		base, ceiling := 0, len(path)
		if query.RunID != "" {
			for _, span := range store.historyRunsLocked(path) {
				if store.entries[path[span.start]].id == query.RunID {
					run := store.runNodeLocked(path, span)
					result.Run, base, ceiling = &run, span.start, span.end
					result.Total = ceiling - base
					break
				}
			}
			if result.Run == nil {
				return fmt.Errorf("%w: run is outside the selected path", ErrEntryNotFound)
			}
		}
		position, direction := ceiling, PageOlder
		if query.Direction == PageNewer {
			position, direction = base, PageNewer
		}
		if cursor := query.Cursor; cursor != nil {
			position, direction = cursor.Position, cursor.Direction
		}
		floor := base
		if query.AroundID != "" || query.AfterID != "" {
			foundAround := query.AroundID == ""
			for i, index := range path {
				id := store.entries[index].id
				if id == query.AroundID {
					if i < base || i >= ceiling {
						return fmt.Errorf("%w: node is outside the selected run", ErrEntryNotFound)
					}
					position, foundAround = i+1, true
				}
				if id == query.AfterID {
					floor, result.Connected = max(base, i+1), true
					if direction == PageNewer {
						position = floor
					}
				}
			}
			if !foundAround {
				return fmt.Errorf("%w: node is outside the selected path", ErrEntryNotFound)
			}
		}
		if position < floor || position > ceiling || direction != PageOlder && direction != PageNewer {
			return ErrReadCursorUnavailable
		}
		start, end, used := position, position, 0
		for len(result.Items) < limit {
			index := end
			if direction == PageOlder {
				index = start - 1
			}
			if index < floor || index >= ceiling {
				break
			}
			entry := store.entries[path[index]]
			item := PageEntry{Node: entryReadNode(entry)}
			item.Node.Position = index
			metadataBytes := 256 + 6*(len(entry.id)+len(entry.parentID)+len(entry.typeName)+len(entry.messageRole)+len(item.Node.Preview))
			cost := entryRawLength(entry) + metadataBytes
			// A page always makes progress and returns a complete entry. Large
			// text/thinking records occupy a page on their own, without truncation.
			if len(result.Items) > 0 && used+cost > budget {
				break
			}
			item.Node.ContentToken = entryContentToken(entry)
			copy, err := materializeReadEntry(entry)
			if err != nil {
				return err
			}
			item.Entry = &copy
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
		result.Start, result.End = start-base, end-base
		if start > base {
			result.Older = &HistoryCursor{Version: version, AnchorID: anchor, Position: start, Direction: PageOlder, RunID: query.RunID}
		}
		if end < ceiling {
			result.Newer = &HistoryCursor{Version: version, AnchorID: anchor, Position: end, Direction: PageNewer, RunID: query.RunID}
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
	LeafID   *string // non-nil restricts candidates to this complete branch
	AfterID  string  // tail since an ancestor; requires LeafID
	Type     string
	Role     string
	Search   string
	Tree     bool // timestamp-ordered depth-first forest, as returned by Tree
	At       *ReadVersion
	Cursor   *NodeCursor
	Limit    int
}

type NodeCursor struct {
	LeafID   *string
	AfterID  string
	Type     string
	Version  ReadVersion
	Position int
	ParentID *string
	Role     string
	Search   string
	Tree     bool
}

type NodePage struct {
	Connected bool // AfterID was an ancestor of the selected branch
	Source    ReadSnapshot
	Version   ReadVersion
	Items     []EntryNode
	Next      *NodeCursor
}

func (m *SessionManager) ReadNodes(query NodeQuery) (NodePage, error) {
	limit, _, err := readPageLimits(query.Limit, 0)
	if err != nil {
		return NodePage{}, err
	}
	if query.AfterID != "" && query.LeafID == nil || query.LeafID != nil && query.Tree {
		return NodePage{}, ErrInvalidReadQuery
	}
	var result NodePage
	err = m.readView(func(store *Session, source ReadSnapshot) error {
		position, at := 0, query.At
		if cursor := query.Cursor; cursor != nil {
			if query.At != nil || !equalReadString(query.ParentID, cursor.ParentID) || query.Role != cursor.Role || query.Search != cursor.Search || query.Tree != cursor.Tree || !equalReadString(query.LeafID, cursor.LeafID) || query.AfterID != cursor.AfterID || query.Type != cursor.Type {
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
		// Node candidates and labels depend on the immutable log prefix, not
		// the manager's selected leaf. Branch selection must not invalidate it.
		version.View = 0
		result = NodePage{Source: source, Version: version, Items: []EntryNode{}, Connected: query.AfterID == ""}
		var order, depths []int
		if query.Tree {
			order, depths = readNodeOrder(store.entries[:version.Log])
		}
		total := int(version.Log)
		if query.LeafID != nil {
			order, err = store.readPathLocked(*query.LeafID, version.Log)
			if err != nil {
				return err
			}
			total = len(order)
			if query.AfterID != "" {
				for index, physical := range order {
					if store.entries[physical].id == query.AfterID {
						result.Connected = true
						if query.Cursor == nil {
							position = index + 1
						}
						break
					}
				}
			}
		}
		if position > total {
			return ErrReadCursorUnavailable
		}
		needle := []byte(strings.ToLower(query.Search))
		for position < total {
			entryIndex := position
			if query.Tree || query.LeafID != nil {
				entryIndex = order[position]
			}
			entry := store.entries[entryIndex]
			position++
			parent := ""
			if parentIndex, exists := store.byID[entry.parentID]; entry.hasParent && exists && uint64(parentIndex) < version.Log {
				parent = entry.parentID
			}
			if query.ParentID != nil && *query.ParentID != parent || query.Role != "" && query.Role != entry.messageRole || query.Type != "" && query.Type != entry.typeName {
				continue
			}
			if len(needle) != 0 {
				raw, err := readEntryRaw(entry)
				if err != nil {
					return err
				}
				if !bytes.Contains(bytes.ToLower(raw), needle) {
					continue
				}
			}
			node := entryReadNode(entry)
			node.Position = position - 1
			node.Label, node.HasChildren = labels[entry.id], children[entry.id]
			if query.Tree {
				node.BranchDepth = depths[entryIndex]
			}
			result.Items = append(result.Items, node)
			if len(result.Items) == limit {
				break
			}
		}
		if position < total {
			var parent *string
			if query.ParentID != nil {
				value := *query.ParentID
				parent = &value
			}
			var leaf *string
			if query.LeafID != nil {
				value := *query.LeafID
				leaf = &value
			}
			result.Next = &NodeCursor{Version: version, Position: position, ParentID: parent, LeafID: leaf, AfterID: query.AfterID, Type: query.Type, Role: query.Role, Search: query.Search, Tree: query.Tree}
		}
		return nil
	})
	return result, err
}

// This metadata traversal follows Tree's repaired parent graph and sibling
// ordering without cloning message payloads or constructing the full tree.
func readNodeOrder(entries []Entry) ([]int, []int) {
	order, depths := make([]int, 0, len(entries)), make([]int, len(entries))
	byID := make(map[string]int, len(entries))
	for index, entry := range entries {
		byID[entry.id] = index
	}
	parents := compatibleTreeParents(entries, byID)
	children, roots := make([][]int, len(entries)), []int{}
	for index, parent := range parents {
		if parent < 0 {
			roots = append(roots, index)
		} else {
			children[parent] = append(children[parent], index)
		}
	}
	for _, siblings := range children {
		sort.SliceStable(siblings, func(i, j int) bool { return entries[siblings[i]].timestamp.Before(entries[siblings[j]].timestamp) })
	}
	stack := make([]int, 0, len(roots))
	for i := len(roots) - 1; i >= 0; i-- {
		stack = append(stack, roots[i])
	}
	for len(stack) > 0 {
		index := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		order = append(order, index)
		depth := depths[index]
		if len(children[index]) > 1 {
			depth++
		}
		for i := len(children[index]) - 1; i >= 0; i-- {
			child := children[index][i]
			depths[child] = depth
			stack = append(stack, child)
		}
	}
	return order, depths
}

func equalReadString(left, right *string) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

// EntrySnapshot reads one immutable entry and its source identity together.
// Media readers must validate here, rather than checking a version before a
// separate Entry call which could race a logical session replacement.
type EntrySnapshot struct {
	ContentToken string
	Version      ReadVersion
	SessionID    string
	Entry        Entry
}

func (m *SessionManager) ReadEntry(id, generation string) (EntrySnapshot, error) {
	var result EntrySnapshot
	err := m.readView(func(store *Session, source ReadSnapshot) error {
		if generation != "" && generation != source.Version.Generation {
			return ErrReadCursorUnavailable
		}
		index, ok := store.byID[id]
		if !ok {
			return ErrEntryNotFound
		}
		entry, err := materializeReadEntry(store.entries[index])
		if err != nil {
			return err
		}
		result = EntrySnapshot{Version: source.Version, SessionID: source.Info.ID, Entry: entry, ContentToken: entryContentToken(store.entries[index])}
		return nil
	})
	return result, err
}

func entryContentToken(entry Entry) string {
	if entry.readRecord != nil {
		return entry.readRecord.token
	}
	return fmt.Sprintf("%x", sha256.Sum256(entry.raw))
}

func entryReadNode(entry Entry) EntryNode {
	node := EntryNode{ID: entry.id, Type: entry.typeName, Role: entry.messageRole, Timestamp: entry.timestamp, RawBytes: entryRawLength(entry), Preview: entryPreview(entry)}
	if entry.hasParent {
		id := entry.parentID
		node.ParentID = &id
	}
	return node
}

func entryPreview(entry Entry) string {
	if entry.readRecord != nil {
		return entry.readRecord.preview
	}
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
