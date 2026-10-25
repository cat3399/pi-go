package session

import (
	"fmt"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
)

// HistoryRun is a branch-local browsing group anchored by a durable user
// entry. It is a read projection, not the Agent's process-local execution RunID:
// queued user inputs retain their own navigable groups, and imported pi logs
// have exactly the same boundaries as newly written logs.
type HistoryRun struct {
	ID               string
	Header           EntryNode
	Result           *EntryNode
	Start            int
	End              int
	LastEntryID      string
	LastActivity     time.Time
	ProcessCount     int
	ResultHasProcess bool
	StopReason       string
	Anchors          []EntryNode
}

type RunOverview struct {
	Run    HistoryRun
	Header *Entry
	Result *Entry
}

type RunsCursor struct {
	Version      ReadVersion
	AnchorID     string
	Position     int
	Direction    PageDirection
	MetadataOnly bool
}

type RunsQuery struct {
	LeafID       *string
	At           *ReadVersion
	Cursor       *RunsCursor
	AroundID     string
	AfterID      string
	Direction    PageDirection
	MetadataOnly bool
	Limit        int
	MaxBytes     int
}

type RunsPage struct {
	Source    ReadSnapshot
	Version   ReadVersion
	AnchorID  string
	Items     []RunOverview
	Start     int
	End       int
	Total     int
	Connected bool
	Older     *RunsCursor
	Newer     *RunsCursor
}

type historyRunSpan struct {
	start, end, result, process int
	resultHasProcess            bool
	stopReason                  string
}

func visibleRunEntry(entry Entry) bool {
	if entry.readRecord != nil {
		return entry.readRecord.visible
	}
	if entry.typeName == "compaction" || entry.typeName == "branch_summary" {
		return true
	}
	if entry.typeName == "custom_message" {
		if value, ok := entry.payload.(CustomMessagePayload); ok {
			return value.Message.Display()
		}
	}
	return entry.typeName == "message" && entry.messageRole != ""
}

func (s *Session) historyRunsLocked(path []int) []historyRunSpan {
	runs := make([]historyRunSpan, 0, len(path)/4+1)
	for position, index := range path {
		entry := s.entries[index]
		if entry.messageRole == "user" || len(runs) == 0 && visibleRunEntry(entry) {
			if len(runs) > 0 {
				runs[len(runs)-1].end = position
			}
			runs = append(runs, historyRunSpan{start: position, result: -1})
		}
		if len(runs) == 0 {
			continue
		}
		run := &runs[len(runs)-1]
		run.end = position + 1
		if position != run.start && visibleRunEntry(entry) {
			run.process++
		}
		if entry.messageRole == "assistant" && position != run.start {
			// Only the last assistant in the complete group can be its result.
			// A tool request with preceding commentary is not a final answer.
			run.result, run.resultHasProcess, run.stopReason = -1, false, ""
			if reason, ok := entryFinishReason(entry); ok {
				if reason != llm.FinishToolUse && reason != llm.FinishPending {
					run.result = position
					run.stopReason = reason.String()
					run.resultHasProcess = assistantProcessPrefix(entry)
				}
			}
		}
	}
	for index := range runs {
		if runs[index].result >= 0 && !runs[index].resultHasProcess {
			runs[index].process--
		}
	}
	return runs
}

func entryFinishReason(entry Entry) (llm.FinishReason, bool) {
	if entry.readRecord != nil {
		return entry.readRecord.finish, entry.readRecord.finished
	}
	if message, ok := entry.message.(interface{ FinishReason() llm.FinishReason }); ok {
		return message.FinishReason(), true
	}
	return llm.FinishPending, false
}

func assistantProcessPrefix(entry Entry) bool {
	if entry.readRecord != nil {
		return entry.readRecord.process
	}
	if message, ok := entry.message.(interface{ Blocks() []llm.AssistantBlock }); ok {
		for _, block := range message.Blocks() {
			switch block.(type) {
			case llm.TextBlock:
			default:
				return true
			}
		}
	}
	return false
}

func (s *Session) runNodeLocked(path []int, span historyRunSpan) HistoryRun {
	header := entryReadNode(s.entries[path[span.start]])
	header.Position = span.start
	run := HistoryRun{ID: header.ID, Header: header, Start: span.start, End: span.end, ProcessCount: span.process,
		LastEntryID: s.entries[path[span.end-1]].id, LastActivity: header.Timestamp, ResultHasProcess: span.resultHasProcess, StopReason: span.stopReason, Anchors: []EntryNode{}}
	if header.Role == "user" {
		run.Anchors = append(run.Anchors, header)
	}
	if span.result >= 0 {
		result := entryReadNode(s.entries[path[span.result]])
		result.Position = span.result
		run.Result = &result
	}
	for position := span.start + 1; position < span.end; position++ {
		entry := s.entries[path[position]]
		if visibleRunEntry(entry) && !entry.timestamp.IsZero() {
			run.LastActivity = entry.timestamp
		}
	}
	return run
}

func (s *Session) runOverviewLocked(path []int, span historyRunSpan, metadataOnly bool) (RunOverview, error) {
	value := RunOverview{Run: s.runNodeLocked(path, span)}
	if !metadataOnly {
		header := s.entries[path[span.start]]
		value.Run.Header.ContentToken = entryContentToken(header)
		copy, err := materializeReadEntry(header)
		if err != nil {
			return RunOverview{}, err
		}
		value.Header = &copy
		if span.result >= 0 {
			result := s.entries[path[span.result]]
			value.Run.Result.ContentToken = entryContentToken(result)
			copy, err := materializeReadEntry(result)
			if err != nil {
				return RunOverview{}, err
			}
			value.Result = &copy
		}
	}
	return value, nil
}

func (m *SessionManager) ReadRuns(query RunsQuery) (RunsPage, error) {
	limit, budget, err := readPageLimits(query.Limit, query.MaxBytes)
	if err != nil {
		return RunsPage{}, err
	}
	var result RunsPage
	err = m.readView(func(store *Session, source ReadSnapshot) error {
		if query.AroundID != "" && query.AfterID != "" || query.Direction != "" && query.Direction != PageOlder && query.Direction != PageNewer {
			return ErrInvalidReadQuery
		}
		at, anchor := query.At, ""
		if source.LeafID != nil {
			anchor = *source.LeafID
		}
		if query.LeafID != nil {
			anchor = *query.LeafID
		}
		if cursor := query.Cursor; cursor != nil {
			if query.AroundID != "" || query.AfterID != "" || query.Direction != "" || query.At != nil || query.MetadataOnly != cursor.MetadataOnly || query.LeafID != nil && *query.LeafID != cursor.AnchorID {
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
		runs := store.historyRunsLocked(path)
		result = RunsPage{Source: source, Version: version, AnchorID: anchor, Total: len(runs), Items: []RunOverview{}, Connected: query.AfterID == ""}
		position, direction, floor := len(runs), PageOlder, 0
		if query.Direction == PageNewer {
			position, direction = 0, PageNewer
		}
		if cursor := query.Cursor; cursor != nil {
			position, direction = cursor.Position, cursor.Direction
		}
		around, after := -1, -1
		for pos, index := range path {
			if store.entries[index].id == query.AroundID {
				around = pos
			}
			if store.entries[index].id == query.AfterID {
				after = pos
				result.Connected = true
			}
		}
		if query.AroundID != "" {
			if around < 0 {
				return fmt.Errorf("%w: node is outside the selected path", ErrEntryNotFound)
			}
			found := false
			for i, span := range runs {
				if around >= span.start && around < span.end {
					position, found = i+1, true
					break
				}
			}
			if !found {
				return fmt.Errorf("%w: node has no conversation group", ErrEntryNotFound)
			}
		}
		if after >= 0 {
			for i, span := range runs {
				if span.end <= after+1 {
					floor = i + 1
				} else {
					break
				}
			}
			if direction == PageNewer {
				position = floor
			}
		}
		if position < floor || position > len(runs) || direction != PageOlder && direction != PageNewer {
			return ErrReadCursorUnavailable
		}
		start, end, used := position, position, 0
		for len(result.Items) < limit {
			index := end
			if direction == PageOlder {
				index = start - 1
			}
			if index < floor || index >= len(runs) {
				break
			}
			span := runs[index]
			cost := 512 + len(store.entries[path[span.start]].id)*6
			if !query.MetadataOnly {
				cost += entryRawLength(store.entries[path[span.start]])
				if span.result >= 0 {
					cost += entryRawLength(store.entries[path[span.result]])
				}
			}
			if len(result.Items) > 0 && used+cost > budget {
				break
			}
			item, err := store.runOverviewLocked(path, span, query.MetadataOnly)
			if err != nil {
				return err
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
			result.Older = &RunsCursor{Version: version, AnchorID: anchor, Position: start, Direction: PageOlder, MetadataOnly: query.MetadataOnly}
		}
		if end < len(runs) {
			result.Newer = &RunsCursor{Version: version, AnchorID: anchor, Position: end, Direction: PageNewer, MetadataOnly: query.MetadataOnly}
		}
		return nil
	})
	return result, err
}
