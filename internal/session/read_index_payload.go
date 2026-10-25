package session

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"

	"github.com/cat3399/pi-go/internal/llm"
)

// readEntryRecord retains only the information used by the shared read-page
// algorithms. Bodies remain in the authoritative JSONL. This is never attached
// to a mutable Session/Runtime or returned across the public Entry boundary.
type readEntryRecord struct {
	source   *ReadIndex
	offset   int64
	length   int
	preview  string
	token    string
	visible  bool
	finished bool
	finish   llm.FinishReason
	process  bool
	usage    *AssistantUsage
}

func (r *ReadIndex) projectEntry(entry Entry, offset int64) Entry {
	// Prime a bounded recent-body working set during the one required decode.
	r.bodies.put(offset, entry)
	record := &readEntryRecord{
		source: r, offset: offset, length: len(entry.raw),
		preview: entryPreview(entry), token: entryContentToken(entry),
		visible: visibleRunEntry(entry), process: assistantProcessPrefix(entry),
		usage: entryAssistantUsage(entry),
	}
	if message, ok := entry.message.(interface{ FinishReason() llm.FinishReason }); ok {
		record.finished, record.finish = true, message.FinishReason()
	}
	// Keep only the structural part needed by compatibility validation.
	if entry.compaction != nil {
		entry.compaction = &CompactionRecord{FirstKeptEntryID: entry.compaction.FirstKeptEntryID}
	}
	if _, label := entry.payload.(LabelPayload); !label {
		entry.payload = nil
	}
	entry.raw, entry.message, entry.diagnostics = nil, nil, nil
	entry.readRecord = record
	return entry
}

func entryRawLength(entry Entry) int {
	if entry.readRecord != nil {
		return entry.readRecord.length
	}
	return len(entry.raw)
}

func materializeReadEntry(entry Entry) (Entry, error) {
	if entry.readRecord == nil {
		return entry.clone(), nil
	}
	record := entry.readRecord
	if cached, ok := record.source.bodies.get(record.offset); ok {
		return cached.clone(), nil
	}
	raw, err := readEntryRaw(entry)
	if err != nil {
		return Entry{}, err
	}
	value, _, err := decodeCompatibleEntry(raw)
	if err != nil {
		return Entry{}, err
	}
	record.source.bodies.put(record.offset, value)
	return value.clone(), nil
}

func readEntryRaw(entry Entry) ([]byte, error) {
	record := entry.readRecord
	if record == nil {
		return entry.raw, nil
	}
	r := record.source
	file, err := os.Open(r.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrReadSourceChanged
		}
		return nil, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !sameReadFile(r.info, before) {
		return nil, ErrReadSourceChanged
	}
	raw := make([]byte, record.length)
	if _, err := file.ReadAt(raw, record.offset); err != nil {
		return nil, fmt.Errorf("%w: read entry: %v", ErrReadSourceChanged, err)
	}
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	// Validate both the opened inode and the path. A replacement must not mix
	// old and new records in one page, even when IDs/length/mtime are reused.
	current, err := os.Stat(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrReadSourceChanged
	}
	if err != nil {
		return nil, err
	}
	if !sameReadFile(r.info, after) || !sameReadFile(r.info, current) || fmt.Sprintf("%x", sha256.Sum256(raw)) != record.token {
		return nil, ErrReadSourceChanged
	}
	return raw, nil
}

func entryAssistantUsage(entry Entry) *AssistantUsage {
	if entry.readRecord != nil {
		if entry.readRecord.usage == nil {
			return nil
		}
		copy := *entry.readRecord.usage
		return &copy
	}
	if message, ok := entry.message.(llm.AssistantTerminal); ok && message.FinishReason() != llm.FinishAborted && message.FinishReason() != llm.FinishError {
		usage := message.Usage()
		if usage.TotalTokens() > 0 || usage.Input() > 0 || usage.Output() > 0 || usage.CacheRead() > 0 || usage.CacheWrite() > 0 {
			return &AssistantUsage{EntryID: entry.id, Timestamp: message.Timestamp(), Provenance: message.AssistantProvenance(), Usage: usage}
		}
	}
	return nil
}

// An immutable index retains structural data and bounded previews, not a
// multiple of the file size. Include map/slice slack and all variable strings;
// this remains a conservative eviction estimate, not a runtime heap metric.
func readIndexBytes(header Header, entries []Entry, summary sessionInfoSummary, diagnostics []LoadDiagnostic) int64 {
	total := int64(4096 + cap(entries)*1024 + cap(header.raw) + len(header.id) + len(header.workingDir) + len(header.parentSession) + len(summary.firstMessage) + len(summary.name))
	for _, entry := range entries {
		total += int64(len(entry.id) + len(entry.parentID) + len(entry.typeName) + len(entry.messageRole))
		total += int64(len(entry.assistant.API) + len(entry.assistant.Provider) + len(entry.assistant.Model))
		if record := entry.readRecord; record != nil {
			total += int64(len(record.preview) + len(record.token))
			if record.usage != nil {
				// Provenance strings are also held by the original entry; leave
				// room for the usage struct and any independent string storage.
				provenance := record.usage.Provenance
				total += int64(512 + len(provenance.API) + len(provenance.Provider) + len(provenance.Model))
			}
		}
		if entry.compaction != nil {
			total += int64(256 + len(entry.compaction.FirstKeptEntryID))
		}
		if label, ok := entry.payload.(LabelPayload); ok {
			total += int64(64 + len(label.TargetID))
			if label.Label != nil {
				total += int64(len(*label.Label))
			}
		}
	}
	for _, diagnostic := range diagnostics {
		total += int64(128 + len(diagnostic.EntryID))
	}
	return total
}

func readIndexed[T any](r *ReadIndex, read func(*SessionManager) (T, error)) (T, error) {
	var empty T
	valid, err := r.ValidateSource()
	if err != nil {
		return empty, err
	}
	if !valid {
		return empty, ErrReadSourceChanged
	}
	value, err := read(r.manager)
	if err != nil {
		return empty, err
	}
	valid, err = r.ValidateSource()
	if err != nil {
		return empty, err
	}
	if !valid {
		return empty, ErrReadSourceChanged
	}
	return value, nil
}
