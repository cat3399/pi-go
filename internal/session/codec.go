package session

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
)

func decodeSessionFile(path string, data []byte) (Header, []Entry, map[string]int, bool, error) {
	var header Header
	entries := make([]Entry, 0)
	byID := make(map[string]int)
	haveHeader := false

	for _, line := range physicalLines(data) {
		if len(bytes.TrimSpace(line.data)) == 0 {
			continue
		}
		if !utf8.Valid(line.data) {
			return Header{}, nil, nil, false, parseError(ErrInvalidSession, path, line.number, "record is not valid UTF-8", nil)
		}
		if !haveHeader {
			decoded, err := decodeHeader(line.data)
			if err != nil {
				return Header{}, nil, nil, false, parseError(ErrInvalidSession, path, line.number, "invalid header", err)
			}
			header = decoded
			haveHeader = true
			continue
		}

		entry, err := decodeEntry(line.data)
		if err != nil {
			return Header{}, nil, nil, false, parseError(ErrInvalidEntry, path, line.number, "invalid entry", err)
		}
		if _, duplicate := byID[entry.id]; duplicate {
			return Header{}, nil, nil, false, parseError(ErrInvalidEntry, path, line.number, "duplicate entry id", nil)
		}
		if entry.hasParent {
			if _, parentExists := byID[entry.parentID]; !parentExists {
				return Header{}, nil, nil, false, parseError(ErrUnsupportedTree, path, line.number, "parent must reference an earlier entry", nil)
			}
		}
		byID[entry.id] = len(entries)
		entries = append(entries, entry)
	}

	if !haveHeader {
		return Header{}, nil, nil, false, fmt.Errorf("%w: %s: missing header", ErrInvalidSession, path)
	}
	if err := validateCompactionEntries(entries, byID); err != nil {
		return Header{}, nil, nil, false, fmt.Errorf("%w: %s", ErrInvalidEntry, err)
	}
	needsSeparator := len(data) > 0 && data[len(data)-1] != '\n'
	return header, entries, byID, needsSeparator, nil
}

// decodeCompatibleSessionFile reads the same permissive physical JSONL stream
// as coding-agent's loadEntriesFromFile(): blank and malformed lines are
// skipped, and the first successfully parsed JSON value must be a valid
// session header. Unlike upstream, every skipped or structurally detached
// record is accounted for by bounded diagnostics. The source bytes are never
// rewritten by this function, so compatibility recovery cannot destroy
// forensic evidence.
//
// decodeSessionFile remains the fail-closed validator for freshly authored
// canonical records. Migration, recovery, and compatible branch snapshots use
// the structurally-clean boundary below so old payload envelopes are not
// erased merely because Go cannot project them.
func decodeCompatibleSessionFile(path string, data []byte) (Header, []Entry, map[string]int, bool, []LoadDiagnostic, error) {
	return decodeCompatibleSessionReader(path, bytes.NewReader(data))
}

// decodeStructurallyCleanCompatibleSessionFile accepts pi-compatible legacy
// payloads but rejects any recovery of the physical stream or parent graph.
// It is the pre-publication boundary for migrations, repairs, and snapshots:
// old payload envelopes survive, while damaged candidate structure never does.
func decodeStructurallyCleanCompatibleSessionFile(path string, data []byte) (Header, []Entry, map[string]int, bool, error) {
	header, entries, byID, needsSeparator, diagnostics, err := decodeCompatibleSessionFile(path, data)
	if err != nil {
		return Header{}, nil, nil, false, err
	}
	if len(diagnostics) == 0 {
		return header, entries, byID, needsSeparator, nil
	}
	diagnostic := diagnostics[0]
	kind := ErrInvalidSession
	switch diagnostic.Code {
	case LoadDiagnosticOrphanParent:
		kind = ErrUnsupportedTree
	case LoadDiagnosticCompaction:
		kind = ErrInvalidEntry
	}
	return Header{}, nil, nil, false, parseError(kind, path, diagnostic.Line, "candidate requires compatibility recovery: "+string(diagnostic.Code), nil)
}

func decodeCompatibleSessionReader(path string, source io.Reader) (Header, []Entry, map[string]int, bool, []LoadDiagnostic, error) {
	var header Header
	entries := make([]Entry, 0)
	byID := make(map[string]int)
	lineByID := make(map[string]int)
	diagnostics := make([]LoadDiagnostic, 0)
	diagnosticCounts := make(map[LoadDiagnosticCode]uint64)
	diagnosticSummaries := make(map[LoadDiagnosticCode]int)
	addDiagnostic := func(diagnostic LoadDiagnostic) {
		diagnostics = appendBoundedLoadDiagnostic(diagnostics, diagnosticCounts, diagnosticSummaries, diagnostic)
	}
	haveHeader := false
	lineNumber := 0
	sawBytes := false
	lastTerminated := false
	reader := bufio.NewReaderSize(source, sessionReadBufferSize)

	for {
		line, readErr := nextStreamedJSONLine(reader)
		if readErr != nil {
			return Header{}, nil, nil, false, diagnostics, fmt.Errorf("%w: read %s: %v", ErrStorage, path, readErr)
		}
		if !line.exists {
			break
		}
		lineNumber++
		sawBytes = true
		lastTerminated = line.terminated
		if line.blank {
			continue
		}
		if line.malformed {
			addDiagnostic(LoadDiagnostic{Code: LoadDiagnosticMalformedLine, Line: lineNumber})
			continue
		}
		if !haveHeader {
			version, versionErr := compatibleHeaderVersion(line.raw)
			if versionErr != nil {
				return Header{}, nil, nil, false, diagnostics, parseError(ErrInvalidSession, path, lineNumber, "invalid header", versionErr)
			}
			if version != 3 {
				return Header{}, nil, nil, false, diagnostics, parseError(ErrUnsupportedVersion, path, lineNumber, "unsupported session version", fmt.Errorf("version %d", version))
			}
			decoded, replacedUTF8, err := decodeCompatibleHeader(line.raw)
			if err != nil {
				return Header{}, nil, nil, false, diagnostics, parseError(ErrInvalidSession, path, lineNumber, "invalid header", err)
			}
			if replacedUTF8 {
				addDiagnostic(LoadDiagnostic{Code: LoadDiagnosticUTF8Replacement, Line: lineNumber})
			}
			header = decoded
			haveHeader = true
			continue
		}

		entry, replacedUTF8, err := decodeCompatibleEntry(line.raw)
		if err != nil {
			return Header{}, nil, nil, false, diagnostics, parseError(ErrInvalidEntry, path, lineNumber, "invalid entry envelope", err)
		}
		if _, duplicate := byID[entry.id]; duplicate {
			return Header{}, nil, nil, false, diagnostics, parseError(ErrInvalidEntry, path, lineNumber, "duplicate entry id", nil)
		}
		if replacedUTF8 {
			addDiagnostic(LoadDiagnostic{Code: LoadDiagnosticUTF8Replacement, Line: lineNumber, EntryID: entry.id})
		}
		byID[entry.id] = len(entries)
		lineByID[entry.id] = lineNumber
		entries = append(entries, entry)
	}

	if !haveHeader {
		return Header{}, nil, nil, false, diagnostics, fmt.Errorf("%w: %s: missing header", ErrInvalidSession, path)
	}
	parents := make([]int, len(entries))
	for index := range parents {
		parents[index] = -1
	}
	for index, entry := range entries {
		if !entry.hasParent {
			continue
		}
		parentIndex, exists := byID[entry.parentID]
		if !exists {
			addDiagnostic(LoadDiagnostic{Code: LoadDiagnosticOrphanParent, Line: lineByID[entry.id], EntryID: entry.id})
			continue
		}
		if parentIndex == index {
			return Header{}, nil, nil, false, diagnostics, parseError(ErrUnsupportedTree, path, lineByID[entry.id], "entry cannot parent itself", nil)
		}
		parents[index] = parentIndex
	}
	if cycleEntry := firstParentCycle(parents); cycleEntry >= 0 {
		return Header{}, nil, nil, false, diagnostics, parseError(ErrUnsupportedTree, path, lineByID[entries[cycleEntry].id], "entry parent cycle", nil)
	}
	visitInvalidCompatibleCompactions(entries, byID, parents, func(_ int, entry Entry) {
		addDiagnostic(LoadDiagnostic{Code: LoadDiagnosticCompaction, Line: lineByID[entry.id], EntryID: entry.id})
	})
	needsSeparator := sawBytes && !lastTerminated
	return header, entries, byID, needsSeparator, diagnostics, nil
}

const maxLoadDiagnosticSamplesPerCode = 16

// appendBoundedLoadDiagnostic retains exact early samples while collapsing a
// hostile tail into one count-bearing record per code. The represented total
// is always the sum of Count, and storage is bounded by the finite diagnostic
// code set rather than the number of damaged physical lines.
func appendBoundedLoadDiagnostic(records []LoadDiagnostic, counts map[LoadDiagnosticCode]uint64, summaries map[LoadDiagnosticCode]int, diagnostic LoadDiagnostic) []LoadDiagnostic {
	seen := counts[diagnostic.Code]
	counts[diagnostic.Code] = seen + 1
	diagnostic.Count = 1
	if seen < maxLoadDiagnosticSamplesPerCode {
		return append(records, diagnostic)
	}
	if index, exists := summaries[diagnostic.Code]; exists {
		records[index].Count++
		return records
	}
	// Line identifies the first occurrence whose detailed sample was omitted.
	// EntryID is intentionally cleared: the aggregate can represent many IDs.
	diagnostic.EntryID = ""
	diagnostic.Truncated = true
	summaries[diagnostic.Code] = len(records)
	return append(records, diagnostic)
}

func compatibleHeaderVersion(raw []byte) (int, error) {
	semantic, _ := replaceInvalidUTF8LikeNode(raw)
	object, err := decodeObject(semantic)
	if err != nil {
		return 0, err
	}
	typeName, err := requiredString(object, "type")
	if err != nil || typeName != "session" {
		return 0, fmt.Errorf("first record is not a session header")
	}
	version := 1
	if encoded, exists := object["version"]; exists {
		if err := json.Unmarshal(encoded, &version); err != nil || version < 1 {
			return 0, fmt.Errorf("invalid session version")
		}
	}
	return version, nil
}

type physicalLine struct {
	number int
	data   []byte
}

func physicalLines(data []byte) []physicalLine {
	lines := make([]physicalLine, 0, bytes.Count(data, []byte{'\n'})+1)
	start := 0
	lineNumber := 1
	for start < len(data) {
		index := bytes.IndexByte(data[start:], '\n')
		if index < 0 {
			lines = append(lines, physicalLine{number: lineNumber, data: data[start:]})
			break
		}
		lines = append(lines, physicalLine{number: lineNumber, data: data[start : start+index]})
		start += index + 1
		lineNumber++
	}
	return lines
}

func decodeHeader(raw []byte) (Header, error) {
	if !utf8.Valid(raw) {
		return Header{}, fmt.Errorf("invalid UTF-8 header")
	}
	return decodeHeaderWithRaw(raw, bytes.Clone(raw))
}

func decodeCompatibleHeader(raw []byte) (Header, bool, error) {
	semantic, replaced := replaceInvalidUTF8LikeNode(raw)
	header, err := decodeHeaderWithRaw(semantic, raw)
	return header, replaced, err
}

func decodeDiscoverableHeader(raw []byte) (Header, error) {
	version, err := compatibleHeaderVersion(raw)
	if err != nil || version > 3 {
		if err == nil {
			err = fmt.Errorf("unsupported session version %d", version)
		}
		return Header{}, err
	}
	if version == 3 {
		header, _, err := decodeCompatibleHeader(raw)
		return header, err
	}
	semantic, _ := replaceInvalidUTF8LikeNode(raw)
	object, err := decodeObject(semantic)
	if err != nil {
		return Header{}, err
	}
	object["version"] = json.RawMessage("3")
	normalized, err := json.Marshal(object)
	if err != nil {
		return Header{}, err
	}
	return decodeHeaderWithRaw(normalized, raw)
}

func decodeHeaderWithRaw(semantic, retainedRaw []byte) (Header, error) {
	object, err := decodeObject(semantic)
	if err != nil {
		return Header{}, err
	}
	typeName, err := requiredString(object, "type")
	if err != nil || typeName != "session" {
		return Header{}, fmt.Errorf("first record is not a session header")
	}
	var version int
	if value, exists := object["version"]; !exists || json.Unmarshal(value, &version) != nil {
		return Header{}, fmt.Errorf("invalid session version")
	}
	if version != 3 {
		return Header{}, fmt.Errorf("%w: version %d", ErrUnsupportedVersion, version)
	}
	id, err := requiredString(object, "id")
	if err != nil {
		return Header{}, err
	}
	if err := validateOpaqueID(id, "session id"); err != nil {
		return Header{}, err
	}
	timestampText, err := requiredString(object, "timestamp")
	if err != nil {
		return Header{}, err
	}
	timestamp, err := time.Parse(time.RFC3339, timestampText)
	if err != nil {
		return Header{}, fmt.Errorf("invalid header timestamp")
	}
	workingDir, err := requiredString(object, "cwd")
	if err != nil || strings.TrimSpace(workingDir) == "" {
		return Header{}, fmt.Errorf("invalid header cwd")
	}
	parentSession := ""
	hasParentSession := false
	if parent, exists := object["parentSession"]; exists {
		var value string
		if json.Unmarshal(parent, &value) != nil {
			return Header{}, fmt.Errorf("invalid header parentSession")
		}
		if !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
			return Header{}, fmt.Errorf("invalid header parentSession")
		}
		parentSession, hasParentSession = value, true
	}
	return Header{id: id, workingDir: workingDir, parentSession: parentSession, hasParentSession: hasParentSession, timestamp: timestamp, raw: retainedRaw}, nil
}

func decodeEntry(raw []byte) (Entry, error) {
	if !utf8.Valid(raw) {
		return Entry{}, fmt.Errorf("invalid UTF-8 entry")
	}
	return decodeEntryWithMode(raw, bytes.Clone(raw), false)
}

func decodeCompatibleEntry(raw []byte) (Entry, bool, error) {
	semantic, replaced := replaceInvalidUTF8LikeNode(raw)
	entry, err := decodeEntryWithMode(semantic, raw, true)
	return entry, replaced, err
}

func decodeEntryWithMode(semantic, retainedRaw []byte, compatible bool) (Entry, error) {
	object, err := decodeObject(semantic)
	if err != nil {
		return Entry{}, err
	}
	typeName, err := requiredString(object, "type")
	if err != nil || strings.TrimSpace(typeName) == "" || typeName == "session" {
		return Entry{}, fmt.Errorf("invalid entry type")
	}
	id, err := requiredString(object, "id")
	if err != nil {
		return Entry{}, err
	}
	if err := validateOpaqueID(id, "entry id"); err != nil {
		return Entry{}, err
	}
	parentID, hasParent, err := decodeParentID(object)
	if err != nil {
		return Entry{}, err
	}
	timestampText, err := requiredString(object, "timestamp")
	if err != nil {
		return Entry{}, err
	}
	timestamp, err := time.Parse(time.RFC3339, timestampText)
	if err != nil {
		return Entry{}, fmt.Errorf("invalid entry timestamp")
	}

	entry := Entry{
		id:        id,
		parentID:  parentID,
		hasParent: hasParent,
		timestamp: timestamp,
		typeName:  typeName,
		raw:       retainedRaw,
	}
	if typeName == "message" {
		messageRaw, exists := object["message"]
		if !exists || bytes.Equal(bytes.TrimSpace(messageRaw), []byte("null")) {
			if compatible {
				entry.diagnostics = append(entry.diagnostics, Diagnostic{Code: DiagnosticUnprojectablePayload, EntryID: id, ContentIndex: -1})
				return entry, nil
			}
			return Entry{}, fmt.Errorf("message entry is missing message")
		}
		if compatible {
			messageRaw = normalizeCompatibleMessage(messageRaw, timestamp)
		}
		if messageObject, objectErr := decodeObject(messageRaw); objectErr == nil {
			if role, roleErr := requiredString(messageObject, "role"); roleErr == nil {
				entry.messageRole = role
				entry.hasMsgRole = true
			}
		}
		entry.assistant, entry.hasAssistant, err = decodeAssistantProvenance(messageRaw)
		if err != nil && !compatible {
			return Entry{}, err
		}
		if err != nil {
			entry.assistant = AssistantProvenance{}
			entry.hasAssistant = false
		}
		entry.message, entry.diagnostics, err = decodeMessage(id, messageRaw)
		if err != nil {
			if compatible {
				entry.diagnostics = append(entry.diagnostics, Diagnostic{Code: DiagnosticUnprojectablePayload, EntryID: id, ContentIndex: -1})
				return entry, nil
			}
			return Entry{}, err
		}
		if entry.message != nil {
			wrapped, wrapErr := agentmsg.NewLLM(entry.message)
			if wrapErr != nil {
				return Entry{}, wrapErr
			}
			entry.payload = MessagePayload{Message: wrapped}
		} else {
			messageObject, _ := decodeObject(messageRaw)
			role, _ := requiredString(messageObject, "role")
			if role == "user" || role == "assistant" || role == "toolResult" {
				if compatible {
					entry.diagnostics = append(entry.diagnostics, Diagnostic{Code: DiagnosticUnprojectablePayload, EntryID: id, ContentIndex: -1})
				}
				return entry, nil
			}
			message, messageErr := decodeCodingAgentMessage(messageRaw)
			if messageErr != nil {
				if compatible {
					entry.diagnostics = append(entry.diagnostics, Diagnostic{Code: DiagnosticUnprojectablePayload, EntryID: id, ContentIndex: -1})
					return entry, nil
				}
				return Entry{}, messageErr
			}
			if message != nil {
				entry.payload = MessagePayload{Message: message}
			}
		}
	} else if typeName == "compaction" {
		compaction, err := decodeCompactionRecord(object)
		if err != nil {
			if compatible {
				entry.diagnostics = append(entry.diagnostics, Diagnostic{Code: DiagnosticUnprojectablePayload, EntryID: id, ContentIndex: -1})
				return entry, nil
			}
			return Entry{}, err
		}
		entry.compaction = &compaction
		_, hasFromHook := object["fromHook"]
		entry.payload = CompactionPayload{Record: compaction, Details: bytes.Clone(object["details"]), FromHook: decodeOptionalBool(object, "fromHook"), HasFromHook: hasFromHook}
	} else {
		if compatible && typeName == "custom_message" {
			object = normalizeCompatibleCustomMessage(object)
		}
		payload, payloadErr := decodeKnownEntryPayload(typeName, object, timestamp)
		if payloadErr != nil {
			if compatible {
				entry.diagnostics = append(entry.diagnostics, Diagnostic{Code: DiagnosticUnprojectablePayload, EntryID: id, ContentIndex: -1})
				return entry, nil
			}
			return Entry{}, payloadErr
		}
		if payload != nil {
			entry.payload = payload
		} else {
			entry.diagnostics = []Diagnostic{{Code: DiagnosticUnknownEntry, EntryID: id, ContentIndex: -1}}
		}
	}
	return entry, nil
}

func normalizeCompatibleMessage(raw []byte, entryTimestamp time.Time) []byte {
	object, err := decodeObject(raw)
	if err != nil {
		return raw
	}
	role, err := requiredString(object, "role")
	if err != nil {
		return raw
	}
	changed := false
	if role == "user" || role == "assistant" || role == "toolResult" {
		if content, exists := object["content"]; !exists || bytes.Equal(bytes.TrimSpace(content), []byte("null")) {
			object["content"] = json.RawMessage("[]")
			changed = true
		}
	}
	if timestamp, exists := object["timestamp"]; !exists || bytes.Equal(bytes.TrimSpace(timestamp), []byte("null")) {
		object["timestamp"] = json.RawMessage(strconv.FormatInt(entryTimestamp.UnixMilli(), 10))
		changed = true
	}
	if role == "assistant" {
		changed = normalizeCompatibleAssistantUsage(object) || changed
	}
	if !changed {
		return raw
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return raw
	}
	return encoded
}

func normalizeCompatibleAssistantUsage(message map[string]json.RawMessage) bool {
	rawUsage, exists := message["usage"]
	changed := false
	if !exists || bytes.Equal(bytes.TrimSpace(rawUsage), []byte("null")) {
		rawUsage = json.RawMessage("{}")
		changed = true
	}
	usage, err := decodeObject(rawUsage)
	if err != nil {
		return false
	}
	zero := json.RawMessage("0")
	for _, key := range []string{"input", "output", "cacheRead", "cacheWrite"} {
		if _, exists := usage[key]; !exists {
			usage[key] = zero
			changed = true
		}
	}
	rawCost, exists := usage["cost"]
	if !exists || bytes.Equal(bytes.TrimSpace(rawCost), []byte("null")) {
		rawCost = json.RawMessage("{}")
		changed = true
	}
	cost, err := decodeObject(rawCost)
	if err == nil {
		for _, key := range []string{"input", "output", "cacheRead", "cacheWrite", "total"} {
			if _, exists := cost[key]; !exists {
				cost[key] = zero
				changed = true
			}
		}
		if changed {
			if encoded, marshalErr := json.Marshal(cost); marshalErr == nil {
				usage["cost"] = encoded
			}
		}
	}
	if changed {
		encoded, marshalErr := json.Marshal(usage)
		if marshalErr != nil {
			return false
		}
		message["usage"] = encoded
	}
	return changed
}

func normalizeCompatibleCustomMessage(object map[string]json.RawMessage) map[string]json.RawMessage {
	content, exists := object["content"]
	if exists && !bytes.Equal(bytes.TrimSpace(content), []byte("null")) {
		return object
	}
	clone := make(map[string]json.RawMessage, len(object)+1)
	for key, value := range object {
		clone[key] = value
	}
	clone["content"] = json.RawMessage("[]")
	return clone
}

func decodeCodingAgentMessage(raw []byte) (agentmsg.Message, error) {
	object, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	role, err := requiredString(object, "role")
	if err != nil {
		return nil, err
	}
	timestamp, err := decodeMessageTimestamp(object)
	if err != nil {
		return nil, err
	}
	switch role {
	case "bashExecution":
		command, e1 := requiredString(object, "command")
		output, e2 := requiredString(object, "output")
		if e1 != nil || e2 != nil {
			return nil, fmt.Errorf("invalid bash execution")
		}
		value := agentmsg.BashExecution{Command: command, Output: output, Cancelled: decodeOptionalBool(object, "cancelled"), Truncated: decodeOptionalBool(object, "truncated"), At: timestamp}
		if raw, ok := object["exitCode"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			var code int
			if json.Unmarshal(raw, &code) != nil {
				return nil, fmt.Errorf("invalid bash exit code")
			}
			value.ExitCode = &code
		}
		if raw, ok := object["fullOutputPath"]; ok {
			if json.Unmarshal(raw, &value.FullOutputPath) != nil {
				return nil, fmt.Errorf("invalid bash full output path")
			}
		}
		value.ExcludeFromContext = decodeOptionalBool(object, "excludeFromContext")
		result, e := agentmsg.NewBashExecution(value)
		return result, e
	case "custom":
		customType, e := requiredString(object, "customType")
		if e != nil {
			return nil, fmt.Errorf("invalid custom agent message")
		}
		contentRaw, ok := object["content"]
		if !ok {
			return nil, fmt.Errorf("invalid custom agent content")
		}
		content, _, contentErr := decodeUserContentBlocks("", contentRaw)
		var stringContent *string
		if contentErr != nil {
			var text string
			if json.Unmarshal(contentRaw, &text) != nil {
				return nil, contentErr
			}
			stringContent = &text
		}
		value := agentmsg.CustomSpec{CustomType: customType, Content: content, StringContent: stringContent, Display: decodeOptionalBool(object, "display"), Details: bytes.Clone(object["details"]), At: timestamp}
		result, e := agentmsg.NewCustom(value)
		return result, e
	default:
		result, e := agentmsg.NewOpaque(agentmsg.OpaqueSpec{Type: role, Data: bytes.Clone(raw), At: timestamp})
		return result, e
	}
}

func decodeCompactionRecord(object map[string]json.RawMessage) (CompactionRecord, error) {
	summary, err := requiredString(object, "summary")
	if err != nil || !utf8.ValidString(summary) || strings.TrimSpace(summary) == "" {
		return CompactionRecord{}, fmt.Errorf("invalid compaction summary")
	}
	firstKept, err := requiredString(object, "firstKeptEntryId")
	if err != nil {
		return CompactionRecord{}, fmt.Errorf("invalid compaction firstKeptEntryId")
	}
	if err := validateOpaqueID(firstKept, "compaction first kept entry id"); err != nil {
		return CompactionRecord{}, err
	}
	tokensBefore, err := requiredUint64(object, "tokensBefore")
	if err != nil {
		return CompactionRecord{}, fmt.Errorf("invalid compaction tokensBefore")
	}
	record := CompactionRecord{Summary: summary, FirstKeptEntryID: firstKept, TokensBefore: tokensBefore}
	if raw, exists := object["usage"]; exists {
		usage, err := decodeCompactionUsage(raw)
		if err != nil {
			return CompactionRecord{}, err
		}
		record.Usage = &usage
	}
	return record, nil
}

func decodeCompactionUsage(raw []byte) (CompactionUsage, error) {
	object, err := decodeObject(raw)
	if err != nil {
		return CompactionUsage{}, fmt.Errorf("invalid compaction usage")
	}
	input, err := requiredUint64(object, "input")
	if err != nil {
		return CompactionUsage{}, err
	}
	output, err := requiredUint64(object, "output")
	if err != nil {
		return CompactionUsage{}, err
	}
	cacheRead, err := requiredUint64(object, "cacheRead")
	if err != nil {
		return CompactionUsage{}, err
	}
	cacheWrite, err := requiredUint64(object, "cacheWrite")
	if err != nil {
		return CompactionUsage{}, err
	}
	spec := llm.UsageSpec{Input: input, Output: output, CacheRead: cacheRead, CacheWrite: cacheWrite}
	if rawReasoning, exists := object["reasoning"]; exists {
		value, err := decodeUint64(rawReasoning)
		if err != nil {
			return CompactionUsage{}, err
		}
		spec.Reasoning = &value
	}
	if rawCacheWrite1h, exists := object["cacheWrite1h"]; exists {
		value, err := decodeUint64(rawCacheWrite1h)
		if err != nil {
			return CompactionUsage{}, err
		}
		spec.CacheWrite1h = &value
	}
	usage, err := llm.NewUsage(spec)
	if err != nil {
		return CompactionUsage{}, err
	}
	if rawTotal, exists := object["totalTokens"]; exists {
		total, err := decodeUint64(rawTotal)
		if err != nil || total != usage.TotalTokens() {
			return CompactionUsage{}, fmt.Errorf("invalid compaction totalTokens")
		}
	}
	costRaw, exists := object["cost"]
	if !exists {
		return CompactionUsage{}, fmt.Errorf("compaction usage is missing cost")
	}
	var cost UsageCost
	if err := json.Unmarshal(costRaw, &cost); err != nil || validateUsageCost(cost) != nil {
		return CompactionUsage{}, fmt.Errorf("invalid compaction usage cost")
	}
	return CompactionUsage{Usage: usage, Cost: cost}, nil
}

func validateCompactionEntries(entries []Entry, byID map[string]int) error {
	parents := make([]int, len(entries))
	for index := range parents {
		parents[index] = -1
		if entries[index].hasParent {
			parents[index] = byID[entries[index].parentID]
		}
	}
	forest := buildParentForestIndex(parents)
	for index, entry := range entries {
		if entry.compaction == nil {
			continue
		}
		if err := validateIndexedCompactionEntry(index, entry, byID, forest, true); err != nil {
			return err
		}
	}
	return nil
}

func visitInvalidCompatibleCompactions(entries []Entry, byID map[string]int, parents []int, visit func(index int, entry Entry)) {
	forest := buildParentForestIndex(parents)
	for index, entry := range entries {
		if entry.compaction == nil {
			continue
		}
		if err := validateIndexedCompactionEntry(index, entry, byID, forest, false); err != nil {
			visit(index, entry)
		}
	}
}

type parentForestIndex struct {
	enter []int
	leave []int
}

// buildParentForestIndex assigns one Euler interval per node. The caller has
// already rejected cycles, so construction is O(n) and every subsequent
// ancestor query is O(1), including histories with a compaction at every node.
func buildParentForestIndex(parents []int) parentForestIndex {
	n := len(parents)
	childCounts := make([]int, n)
	for _, parent := range parents {
		if parent >= 0 && parent < n {
			childCounts[parent]++
		}
	}
	offsets := make([]int, n+1)
	for index, count := range childCounts {
		offsets[index+1] = offsets[index] + count
	}
	children := make([]int, offsets[n])
	next := append([]int(nil), offsets[:n]...)
	for child, parent := range parents {
		if parent < 0 || parent >= n {
			continue
		}
		children[next[parent]] = child
		next[parent]++
	}
	type frame struct {
		node int
		next int
	}
	result := parentForestIndex{enter: make([]int, n), leave: make([]int, n)}
	stack := make([]frame, 0, n)
	timer := 0
	for root, parent := range parents {
		if parent >= 0 && parent < n {
			continue
		}
		result.enter[root] = timer
		timer++
		stack = append(stack, frame{node: root, next: offsets[root]})
		for len(stack) > 0 {
			top := &stack[len(stack)-1]
			end := offsets[top.node+1]
			if top.next < end {
				child := children[top.next]
				top.next++
				result.enter[child] = timer
				timer++
				stack = append(stack, frame{node: child, next: offsets[child]})
				continue
			}
			result.leave[top.node] = timer
			stack = stack[:len(stack)-1]
		}
	}
	return result
}

func (forest parentForestIndex) isAncestor(ancestor, node int) bool {
	return ancestor >= 0 && node >= 0 && ancestor < len(forest.enter) && node < len(forest.enter) &&
		forest.enter[ancestor] <= forest.enter[node] && forest.enter[node] < forest.leave[ancestor]
}

func validateIndexedCompactionEntry(index int, entry Entry, byID map[string]int, forest parentForestIndex, requireEarlier bool) error {
	if entry.compaction == nil {
		return nil
	}
	indexed, entryExists := byID[entry.id]
	if !entryExists || indexed != index {
		return fmt.Errorf("compaction %q is not indexed", entry.id)
	}
	if !entry.hasParent {
		return fmt.Errorf("compaction %q must have a parent", entry.id)
	}
	firstIndex, exists := byID[entry.compaction.FirstKeptEntryID]
	if !exists || (requireEarlier && firstIndex >= index) {
		return fmt.Errorf("compaction %q first kept entry is not earlier", entry.id)
	}
	parentIndex, parentExists := byID[entry.parentID]
	if !parentExists {
		return fmt.Errorf("compaction %q parent is missing", entry.id)
	}
	if !forest.isAncestor(firstIndex, parentIndex) {
		return fmt.Errorf("compaction %q first kept entry is outside its parent branch", entry.id)
	}
	return nil
}

func decodeAssistantProvenance(raw []byte) (AssistantProvenance, bool, error) {
	object, err := decodeObject(raw)
	if err != nil {
		return AssistantProvenance{}, false, err
	}
	role, err := requiredString(object, "role")
	if err != nil || role != "assistant" {
		return AssistantProvenance{}, false, nil
	}
	identity := AssistantProvenance{}
	identity.API, err = requiredString(object, "api")
	if err != nil {
		return AssistantProvenance{}, false, err
	}
	identity.Provider, err = requiredString(object, "provider")
	if err != nil {
		return AssistantProvenance{}, false, err
	}
	identity.Model, err = requiredString(object, "model")
	if err != nil {
		return AssistantProvenance{}, false, err
	}
	usage, err := decodeObject(object["usage"])
	if err != nil {
		return AssistantProvenance{}, false, fmt.Errorf("invalid assistant usage")
	}
	costRaw, exists := usage["cost"]
	if !exists {
		return AssistantProvenance{}, false, fmt.Errorf("assistant usage is missing cost")
	}
	if err := json.Unmarshal(costRaw, &identity.Cost); err != nil {
		return AssistantProvenance{}, false, fmt.Errorf("invalid assistant usage cost")
	}
	if err := validateAssistantProvenance(identity); err != nil {
		return AssistantProvenance{}, false, err
	}
	return identity, true, nil
}

func decodeParentID(object map[string]json.RawMessage) (string, bool, error) {
	raw, exists := object["parentId"]
	if !exists {
		return "", false, fmt.Errorf("entry is missing parentId")
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", false, nil
	}
	var parent string
	if json.Unmarshal(raw, &parent) != nil {
		return "", false, fmt.Errorf("invalid parentId")
	}
	if err := validateOpaqueID(parent, "parent id"); err != nil {
		return "", false, err
	}
	return parent, true, nil
}

func decodeMessage(entryID string, raw []byte) (llm.ConversationMessage, []Diagnostic, error) {
	object, err := decodeObject(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid message object: %w", err)
	}
	role, err := requiredString(object, "role")
	if err != nil {
		return nil, nil, err
	}
	switch role {
	case "user":
		return decodeUserMessage(entryID, object)
	case "assistant":
		return decodeAssistantMessage(entryID, object)
	case "toolResult":
		return decodeToolResultMessage(entryID, object)
	default:
		return nil, []Diagnostic{{Code: DiagnosticUnknownMessageRole, EntryID: entryID, ContentIndex: -1}}, nil
	}
}

func decodeUserMessage(entryID string, object map[string]json.RawMessage) (llm.ConversationMessage, []Diagnostic, error) {
	timestamp, err := decodeMessageTimestamp(object)
	if err != nil {
		return nil, nil, err
	}
	content, exists := object["content"]
	if !exists {
		return nil, nil, fmt.Errorf("user message is missing content")
	}
	var text string
	if json.Unmarshal(content, &text) == nil {
		message, err := llm.NewUserTextMessage(text, timestamp)
		return message, nil, err
	}
	userBlocks, diagnostics, err := decodeUserContentBlocks(entryID, content)
	if err != nil {
		return nil, nil, err
	}
	if len(userBlocks) == 0 && len(diagnostics) > 0 {
		diagnostics = append(diagnostics, Diagnostic{Code: DiagnosticUnprojectableMessage, EntryID: entryID, ContentIndex: -1})
		return nil, diagnostics, nil
	}
	message, err := llm.NewUserContentMessage(userBlocks, timestamp)
	return message, diagnostics, err
}

func decodeAssistantMessage(entryID string, object map[string]json.RawMessage) (llm.ConversationMessage, []Diagnostic, error) {
	timestamp, err := decodeMessageTimestamp(object)
	if err != nil {
		return nil, nil, err
	}
	provenance, err := decodeLLMAssistantProvenance(object)
	if err != nil {
		return nil, nil, err
	}
	stopReason, err := requiredString(object, "stopReason")
	if err != nil {
		return nil, nil, err
	}
	content, exists := object["content"]
	if !exists {
		return nil, nil, fmt.Errorf("assistant message is missing content")
	}
	blocks, diagnostics, err := decodeBlocks(entryID, content, true, true)
	if err != nil {
		return nil, nil, err
	}
	usage, err := decodeUsage(object["usage"])
	if err != nil {
		return nil, nil, err
	}
	response, unsafeResponse := decodeResponseMetadata(object)
	if unsafeResponse {
		diagnostics = append(diagnostics, Diagnostic{Code: DiagnosticUnsafeContentOmitted, EntryID: entryID, ContentIndex: -1})
	}
	assistantDiagnostics, err := decodeAssistantDiagnostics(object["diagnostics"])
	if err != nil {
		return nil, nil, err
	}

	switch stopReason {
	case "stop", "length":
		finish := llm.FinishStop
		if stopReason == "length" {
			finish = llm.FinishLength
		}
		var message llm.AssistantTerminal
		if hasToolCall(blocks) {
			message, err = llm.NewAssistantToolUseMessageWithFinishAndMetadata(blocks, finish, usage, timestamp, provenance, response, assistantDiagnostics)
		} else if hasThinking(blocks) {
			message, err = llm.NewAssistantRichMessageWithMetadata(blocks, finish, usage, timestamp, provenance, response, assistantDiagnostics)
		} else {
			message, err = llm.NewAssistantTextMessageWithMetadata(textBlocks(blocks), finish, usage, timestamp, provenance, response, assistantDiagnostics)
		}
		if err != nil {
			return nil, nil, err
		}
		return message, diagnostics, nil
	case "toolUse":
		message, err := llm.NewAssistantToolUseMessageWithMetadata(blocks, usage, timestamp, provenance, response, assistantDiagnostics)
		if err != nil {
			diagnostics = append(diagnostics, Diagnostic{Code: DiagnosticUnprojectableMessage, EntryID: entryID, ContentIndex: -1})
			return nil, diagnostics, nil
		}
		return message, diagnostics, nil
	case "error", "aborted":
		finish := llm.FinishError
		if stopReason == "aborted" {
			finish = llm.FinishAborted
		}
		errorMessage, errorMessageErr := requiredString(object, "errorMessage")
		if errorMessageErr != nil || strings.TrimSpace(errorMessage) == "" {
			diagnostics = append(diagnostics, Diagnostic{Code: DiagnosticUnprojectableMessage, EntryID: entryID, ContentIndex: -1})
			return nil, diagnostics, nil
		}
		failure, failureErr := llm.NewFailure(errorMessage, nil)
		if failureErr != nil {
			return nil, diagnostics, failureErr
		}
		message, err := llm.NewAssistantFailureMessageWithBlocksAndMetadata(blocks, finish, failure, usage, timestamp, provenance, response, assistantDiagnostics)
		return message, diagnostics, err
	default:
		diagnostics = append(diagnostics, Diagnostic{Code: DiagnosticUnprojectableMessage, EntryID: entryID, ContentIndex: -1})
		return nil, diagnostics, nil
	}
}

func decodeLLMAssistantProvenance(object map[string]json.RawMessage) (llm.AssistantProvenance, error) {
	provider, err := requiredString(object, "provider")
	if err != nil {
		return llm.AssistantProvenance{}, err
	}
	api, err := requiredString(object, "api")
	if err != nil {
		return llm.AssistantProvenance{}, err
	}
	model, err := requiredString(object, "model")
	if err != nil {
		return llm.AssistantProvenance{}, err
	}
	return llm.AssistantProvenance{Provider: provider, API: api, Model: model}, nil
}

func decodeAssistantDiagnostics(raw json.RawMessage) ([]llm.AssistantDiagnostic, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) != nil || values == nil {
		return nil, fmt.Errorf("invalid assistant diagnostics")
	}
	result := make([]llm.AssistantDiagnostic, 0, len(values))
	for _, value := range values {
		object, err := decodeObject(value)
		if err != nil {
			return nil, fmt.Errorf("invalid assistant diagnostic")
		}
		typ, err := requiredString(object, "type")
		if err != nil {
			return nil, fmt.Errorf("invalid assistant diagnostic type")
		}
		millis, err := requiredUint64(object, "timestamp")
		if err != nil || millis > uint64(^uint64(0)>>1) {
			return nil, fmt.Errorf("invalid assistant diagnostic timestamp")
		}
		spec := llm.AssistantDiagnosticSpec{Type: typ, Timestamp: time.UnixMilli(int64(millis))}
		if errorRaw, exists := object["error"]; exists {
			errorObject, err := decodeObject(errorRaw)
			if err != nil {
				return nil, fmt.Errorf("invalid assistant diagnostic error")
			}
			message, err := requiredString(errorObject, "message")
			if err != nil {
				return nil, fmt.Errorf("invalid assistant diagnostic error")
			}
			info := &llm.AssistantDiagnosticError{Message: message}
			for key, target := range map[string]*string{"name": &info.Name, "stack": &info.Stack} {
				if field, exists := errorObject[key]; exists && json.Unmarshal(field, target) != nil {
					return nil, fmt.Errorf("invalid assistant diagnostic error %s", key)
				}
			}
			if code, exists := errorObject["code"]; exists {
				info.Code = append(json.RawMessage(nil), code...)
			}
			spec.Error = info
		}
		if details, exists := object["details"]; exists {
			spec.Details = append(json.RawMessage(nil), details...)
		}
		diagnostic, err := llm.NewAssistantDiagnostic(spec)
		if err != nil {
			return nil, err
		}
		result = append(result, diagnostic)
	}
	return result, nil
}

func decodeResponseMetadata(object map[string]json.RawMessage) (*llm.AssistantResponseMetadata, bool) {
	rawID, hasID := object["responseId"]
	rawModel, hasModel := object["responseModel"]
	rawStop, hasStop := object["rawStopReason"]
	if !hasID && !hasModel && !hasStop {
		return nil, false
	}
	var value llm.AssistantResponseMetadata
	if hasID && !decodeJSONString(rawID, &value.ResponseID) {
		return nil, true
	}
	if hasModel && !decodeJSONString(rawModel, &value.ResponseModel) {
		return nil, true
	}
	if hasStop && !decodeJSONString(rawStop, &value.RawStopReason) {
		return nil, true
	}
	if !utf8.ValidString(value.ResponseID) || !utf8.ValidString(value.ResponseModel) || !utf8.ValidString(value.RawStopReason) || len(value.ResponseID) > 256 || len(value.ResponseModel) > 512 || len(value.RawStopReason) > 128 {
		return nil, true
	}
	return &value, false
}

func decodeToolResultMessage(entryID string, object map[string]json.RawMessage) (llm.ConversationMessage, []Diagnostic, error) {
	timestamp, err := decodeMessageTimestamp(object)
	if err != nil {
		return nil, nil, err
	}
	toolCallID, err := requiredString(object, "toolCallId")
	if err != nil {
		return nil, nil, err
	}
	toolName, err := requiredString(object, "toolName")
	if err != nil {
		return nil, nil, err
	}
	var isError bool
	if raw, exists := object["isError"]; !exists || json.Unmarshal(raw, &isError) != nil {
		return nil, nil, fmt.Errorf("tool result has invalid isError")
	}
	content, exists := object["content"]
	if !exists {
		return nil, nil, fmt.Errorf("tool result is missing content")
	}
	rich, diagnostics, err := decodeToolResultContentBlocks(entryID, content)
	if err != nil {
		return nil, nil, err
	}
	texts := make([]llm.TextBlock, 0, len(rich))
	for _, block := range rich {
		if text, ok := block.(llm.TextBlock); ok {
			texts = append(texts, text)
		}
	}
	if len(rich) != len(texts) {
		metadata, err := decodeToolResultMetadata(object)
		if err != nil {
			return nil, nil, err
		}
		message, err := llm.NewToolResultContentMessageWithMetadata(toolCallID, toolName, rich, isError, timestamp, metadata)
		return message, diagnostics, err
	}
	metadata, err := decodeToolResultMetadata(object)
	if err != nil {
		return nil, nil, err
	}
	message, err := llm.NewToolResultMessageWithMetadata(toolCallID, toolName, texts, isError, timestamp, metadata)
	return message, diagnostics, err
}

func toolResultDetails(object map[string]json.RawMessage) json.RawMessage {
	raw, ok := object["details"]
	if !ok || len(raw) == 0 || !json.Valid(raw) {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func decodeOptionalBool(object map[string]json.RawMessage, key string) bool {
	raw, ok := object[key]
	if !ok {
		return false
	}
	var value bool
	return json.Unmarshal(raw, &value) == nil && value
}

func decodeKnownEntryPayload(typeName string, object map[string]json.RawMessage, timestamp time.Time) (EntryPayload, error) {
	switch typeName {
	case "thinking_level_change":
		value, err := requiredString(object, "thinkingLevel")
		if err != nil {
			return nil, fmt.Errorf("invalid thinking level change")
		}
		return ThinkingLevelChangePayload{ThinkingLevel: value}, nil
	case "model_change":
		providerID, e1 := requiredString(object, "provider")
		modelID, e2 := requiredString(object, "modelId")
		if e1 != nil || e2 != nil {
			return nil, fmt.Errorf("invalid model change")
		}
		return ModelChangePayload{Provider: providerID, ModelID: modelID}, nil
	case "branch_summary":
		fromID, e1 := requiredString(object, "fromId")
		summary, e2 := requiredString(object, "summary")
		if e1 != nil || e2 != nil {
			return nil, fmt.Errorf("invalid branch summary")
		}
		payload := BranchSummaryPayload{FromID: fromID, Summary: summary, Details: bytes.Clone(object["details"]), FromHook: decodeOptionalBool(object, "fromHook")}
		_, payload.HasFromHook = object["fromHook"]
		if raw, ok := object["usage"]; ok {
			usage, e := decodeCompactionUsage(raw)
			if e != nil {
				return nil, e
			}
			payload.Usage = &usage
		}
		return payload, nil
	case "custom":
		customType, e := requiredString(object, "customType")
		if e != nil {
			return nil, fmt.Errorf("invalid custom entry")
		}
		return CustomPayload{CustomType: customType, Data: bytes.Clone(object["data"])}, nil
	case "custom_message":
		customType, e := requiredString(object, "customType")
		if e != nil {
			return nil, fmt.Errorf("invalid custom message")
		}
		display := decodeOptionalBool(object, "display")
		contentRaw, ok := object["content"]
		if !ok {
			return nil, fmt.Errorf("invalid custom message content")
		}
		content, _, e := decodeUserContentBlocks("", contentRaw)
		var stringContent *string
		if e != nil {
			var text string
			if json.Unmarshal(contentRaw, &text) != nil {
				return nil, e
			}
			stringContent = &text
		}
		message, e := agentmsg.NewCustom(agentmsg.CustomSpec{CustomType: customType, Content: content, StringContent: stringContent, Display: display, Details: bytes.Clone(object["details"]), At: timestamp})
		if e != nil {
			return nil, e
		}
		return CustomMessagePayload{Message: message}, nil
	case "label":
		target, e := requiredString(object, "targetId")
		if e != nil {
			return nil, fmt.Errorf("invalid label entry")
		}
		payload := LabelPayload{TargetID: target}
		if raw, ok := object["label"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			var label string
			if json.Unmarshal(raw, &label) != nil {
				return nil, fmt.Errorf("invalid label entry")
			}
			payload.Label = &label
		}
		return payload, nil
	case "session_info":
		payload := SessionInfoPayload{}
		if raw, ok := object["name"]; ok {
			var name string
			if json.Unmarshal(raw, &name) != nil {
				return nil, fmt.Errorf("invalid session info")
			}
			payload.Name = &name
		}
		return payload, nil
	default:
		return nil, nil
	}
}

func decodeToolResultMetadata(object map[string]json.RawMessage) (llm.ToolResultMetadata, error) {
	metadata := llm.ToolResultMetadata{Details: toolResultDetails(object)}
	if raw, exists := object["addedToolNames"]; exists {
		if err := json.Unmarshal(raw, &metadata.AddedToolNames); err != nil {
			return llm.ToolResultMetadata{}, fmt.Errorf("tool result has invalid addedToolNames")
		}
		metadata.HasAddedToolNames = true
	}
	if raw, exists := object["usage"]; exists {
		usage, err := decodePortableUsage(raw)
		if err != nil {
			return llm.ToolResultMetadata{}, err
		}
		metadata.Usage = &usage
	}
	return metadata, nil
}

func decodePortableUsage(raw []byte) (llm.Usage, error) {
	object, err := decodeObject(raw)
	if err != nil {
		return llm.Usage{}, fmt.Errorf("tool result has invalid usage")
	}
	input, err := requiredUint64(object, "input")
	if err != nil {
		return llm.Usage{}, fmt.Errorf("tool result has invalid usage")
	}
	output, err := requiredUint64(object, "output")
	if err != nil {
		return llm.Usage{}, fmt.Errorf("tool result has invalid usage")
	}
	cacheRead, err := requiredUint64(object, "cacheRead")
	if err != nil {
		return llm.Usage{}, fmt.Errorf("tool result has invalid usage")
	}
	cacheWrite, err := requiredUint64(object, "cacheWrite")
	if err != nil {
		return llm.Usage{}, fmt.Errorf("tool result has invalid usage")
	}
	spec := llm.UsageSpec{Input: input, Output: output, CacheRead: cacheRead, CacheWrite: cacheWrite}
	if value, exists := object["reasoning"]; exists {
		n, e := decodeUint64(value)
		if e != nil {
			return llm.Usage{}, fmt.Errorf("tool result has invalid usage")
		}
		spec.Reasoning = &n
	}
	if value, exists := object["cacheWrite1h"]; exists {
		n, e := decodeUint64(value)
		if e != nil {
			return llm.Usage{}, fmt.Errorf("tool result has invalid usage")
		}
		spec.CacheWrite1h = &n
	}
	if value, exists := object["cost"]; exists {
		var cost llm.Cost
		if err := json.Unmarshal(value, &cost); err != nil {
			return llm.Usage{}, fmt.Errorf("tool result has invalid usage cost")
		}
		spec.Cost = &cost
	}
	usage, err := llm.NewUsage(spec)
	if err != nil {
		return llm.Usage{}, err
	}
	if value, exists := object["totalTokens"]; exists {
		total, e := decodeUint64(value)
		if e != nil || total != usage.TotalTokens() {
			return llm.Usage{}, fmt.Errorf("tool result has invalid totalTokens")
		}
	}
	return usage, nil
}

func decodeBlocks(entryID string, raw []byte, allowToolCalls, allowSignatures bool) ([]llm.AssistantBlock, []Diagnostic, error) {
	var encoded []json.RawMessage
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return nil, nil, fmt.Errorf("message content must be an array")
	}
	blocks := make([]llm.AssistantBlock, 0, len(encoded))
	diagnostics := make([]Diagnostic, 0)
	for index, encodedBlock := range encoded {
		object, err := decodeObject(encodedBlock)
		if err != nil {
			return nil, nil, fmt.Errorf("content block %d is invalid", index)
		}
		typeName, err := requiredString(object, "type")
		if err != nil {
			return nil, nil, fmt.Errorf("content block %d has invalid type", index)
		}
		switch typeName {
		case "text":
			text, err := requiredString(object, "text")
			if err != nil {
				return nil, nil, fmt.Errorf("content block %d has invalid text", index)
			}
			signature, hasMetadata, trusted := decodeOpaqueSignature(object, "textSignature", allowSignatures)
			if hasMetadata && !trusted {
				diagnostics = append(diagnostics, Diagnostic{Code: DiagnosticUnsafeContentOmitted, EntryID: entryID, ContentIndex: index})
			}
			if hasMetadata && !trusted && strings.TrimSpace(text) == "" {
				continue
			}
			block, err := llm.NewTextBlockWithSignature(text, signature)
			if err != nil {
				return nil, nil, err
			}
			blocks = append(blocks, block)
		case "thinking":
			thinking, err := requiredString(object, "thinking")
			if err != nil {
				return nil, nil, fmt.Errorf("content block %d has invalid thinking", index)
			}
			signature, hasMetadata, trusted := decodeOpaqueSignature(object, "thinkingSignature", allowSignatures)
			redacted, redactedMetadata, redactedTrusted := decodeRedacted(object, allowSignatures)
			hasMetadata = hasMetadata || redactedMetadata
			trusted = trusted && redactedTrusted
			if hasMetadata && !trusted {
				diagnostics = append(diagnostics, Diagnostic{Code: DiagnosticUnsafeContentOmitted, EntryID: entryID, ContentIndex: index})
			}
			if signature == "" && hasMetadata && (redacted || strings.TrimSpace(thinking) == "") {
				continue
			}
			block, err := llm.NewThinkingBlockWithSignature(thinking, signature, redacted)
			if err != nil {
				return nil, nil, err
			}
			blocks = append(blocks, block)
		case "toolCall":
			if !allowToolCalls {
				diagnostics = append(diagnostics, Diagnostic{Code: DiagnosticUnknownContentBlock, EntryID: entryID, ContentIndex: index})
				continue
			}
			id, err := requiredString(object, "id")
			if err != nil {
				return nil, nil, fmt.Errorf("content block %d has invalid tool id", index)
			}
			name, err := requiredString(object, "name")
			if err != nil {
				return nil, nil, fmt.Errorf("content block %d has invalid tool name", index)
			}
			arguments, exists := object["arguments"]
			if !exists {
				return nil, nil, fmt.Errorf("content block %d is missing tool arguments", index)
			}
			thoughtSignature, hasMetadata, trusted := decodeOpaqueSignature(object, "thoughtSignature", allowSignatures)
			if hasMetadata && !trusted {
				diagnostics = append(diagnostics, Diagnostic{Code: DiagnosticUnsafeContentOmitted, EntryID: entryID, ContentIndex: index})
			}
			block, err := llm.NewToolCallBlockWithThoughtSignature(id, name, arguments, thoughtSignature)
			if err != nil {
				return nil, nil, err
			}
			blocks = append(blocks, block)
		default:
			diagnostics = append(diagnostics, Diagnostic{Code: DiagnosticUnknownContentBlock, EntryID: entryID, ContentIndex: index})
		}
	}
	return blocks, diagnostics, nil
}

func hasThinking(blocks []llm.AssistantBlock) bool {
	for _, block := range blocks {
		if _, ok := block.(llm.ThinkingBlock); ok {
			return true
		}
	}
	return false
}
func hasToolCall(blocks []llm.AssistantBlock) bool {
	for _, block := range blocks {
		if _, ok := block.(llm.ToolCallBlock); ok {
			return true
		}
	}
	return false
}

// Session storage treats content signatures as provider-owned opaque strings.
// Shape validation belongs to the adapter that replays a matching message.
func decodeOpaqueSignature(object map[string]json.RawMessage, field string, allow bool) (string, bool, bool) {
	raw, ok := object[field]
	if !ok {
		return "", false, true
	}
	if !allow {
		return "", true, false
	}
	var signature string
	if !decodeJSONString(raw, &signature) || !utf8.ValidString(signature) || len(signature) > 2<<20 {
		return "", true, false
	}
	return signature, true, true
}

func decodeRedacted(object map[string]json.RawMessage, allow bool) (bool, bool, bool) {
	raw, ok := object["redacted"]
	if !ok {
		return false, false, true
	}
	if !allow {
		return true, true, false
	}
	var redacted bool
	if !decodeJSONBool(raw, &redacted) {
		return true, true, false
	}
	return redacted, true, true
}

func decodeJSONString(raw json.RawMessage, value *string) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '"' && json.Unmarshal(trimmed, value) == nil
}

func decodeJSONBool(raw json.RawMessage, value *bool) bool {
	trimmed := bytes.TrimSpace(raw)
	return (bytes.Equal(trimmed, []byte("true")) || bytes.Equal(trimmed, []byte("false"))) && json.Unmarshal(trimmed, value) == nil
}

func decodeUserContentBlocks(entryID string, raw []byte) ([]llm.UserContentBlock, []Diagnostic, error) {
	return decodeRichInputBlocks(entryID, raw, true)
}
func decodeToolResultContentBlocks(entryID string, raw []byte) ([]llm.ToolResultContentBlock, []Diagnostic, error) {
	blocks, diags, err := decodeRichInputBlocks(entryID, raw, false)
	if err != nil {
		return nil, nil, err
	}
	out := make([]llm.ToolResultContentBlock, len(blocks))
	for i, b := range blocks {
		out[i] = b.(llm.ToolResultContentBlock)
	}
	return out, diags, nil
}
func decodeRichInputBlocks(entryID string, raw []byte, user bool) ([]llm.UserContentBlock, []Diagnostic, error) {
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, nil, fmt.Errorf("message content must be an array")
	}
	out := make([]llm.UserContentBlock, 0, len(values))
	diags := []Diagnostic{}
	for i, value := range values {
		obj, err := decodeObject(value)
		if err != nil {
			return nil, nil, fmt.Errorf("content block %d is invalid", i)
		}
		kind, err := requiredString(obj, "type")
		if err != nil {
			return nil, nil, fmt.Errorf("content block %d has invalid type", i)
		}
		switch kind {
		case "text":
			text, err := requiredString(obj, "text")
			if err != nil {
				return nil, nil, err
			}
			b, err := llm.NewTextBlock(text)
			if err != nil {
				return nil, nil, err
			}
			out = append(out, b)
		case "image":
			media, err := requiredString(obj, "mimeType")
			if err != nil {
				return nil, nil, err
			}
			var b llm.ImageBlock
			if data, ok := obj["data"]; ok {
				var encoded string
				if json.Unmarshal(data, &encoded) != nil {
					return nil, nil, fmt.Errorf("invalid image data")
				}
				decoded, err := base64.StdEncoding.DecodeString(encoded)
				if err != nil {
					return nil, nil, fmt.Errorf("invalid image base64")
				}
				b, err = llm.NewImageDataBlock(media, decoded)
				if err != nil {
					return nil, nil, err
				}
			} else if rawURL, ok := obj["url"]; ok {
				var url string
				if json.Unmarshal(rawURL, &url) != nil {
					return nil, nil, fmt.Errorf("invalid image url")
				}
				b, err = llm.NewImageURLBlock(media, url)
				if err != nil {
					return nil, nil, err
				}
			} else {
				return nil, nil, fmt.Errorf("image has no source")
			}
			out = append(out, b)
		default:
			diags = append(diags, Diagnostic{Code: DiagnosticUnknownContentBlock, EntryID: entryID, ContentIndex: i})
		}
	}
	return out, diags, nil
}

func textBlocks(blocks []llm.AssistantBlock) []llm.TextBlock {
	texts := make([]llm.TextBlock, 0, len(blocks))
	for _, block := range blocks {
		if text, ok := block.(llm.TextBlock); ok {
			texts = append(texts, text)
		}
	}
	return texts
}

func decodeUsage(raw []byte) (llm.Usage, error) {
	object, err := decodeObject(raw)
	if err != nil {
		return llm.Usage{}, fmt.Errorf("invalid assistant usage")
	}
	input, err := requiredUint64(object, "input")
	if err != nil {
		return llm.Usage{}, err
	}
	output, err := requiredUint64(object, "output")
	if err != nil {
		return llm.Usage{}, err
	}
	cacheRead, err := requiredUint64(object, "cacheRead")
	if err != nil {
		return llm.Usage{}, err
	}
	cacheWrite, err := requiredUint64(object, "cacheWrite")
	if err != nil {
		return llm.Usage{}, err
	}
	var reasoning *uint64
	if value, exists := object["reasoning"]; exists {
		decoded, err := decodeUint64(value)
		if err != nil {
			return llm.Usage{}, fmt.Errorf("invalid usage reasoning")
		}
		reasoning = &decoded
	}
	var cacheWrite1h *uint64
	if value, exists := object["cacheWrite1h"]; exists {
		decoded, err := decodeUint64(value)
		if err != nil {
			return llm.Usage{}, fmt.Errorf("invalid usage cacheWrite1h")
		}
		cacheWrite1h = &decoded
	}
	usage, err := llm.NewUsage(llm.UsageSpec{
		Input: input, Output: output, CacheRead: cacheRead, CacheWrite: cacheWrite,
		Reasoning: reasoning, CacheWrite1h: cacheWrite1h,
	})
	if err != nil {
		return llm.Usage{}, err
	}
	if value, exists := object["totalTokens"]; exists {
		total, err := decodeUint64(value)
		if err != nil || total != usage.TotalTokens() {
			return llm.Usage{}, fmt.Errorf("invalid usage totalTokens")
		}
	}
	costRaw, exists := object["cost"]
	if !exists {
		return llm.Usage{}, fmt.Errorf("assistant usage is missing cost")
	}
	var cost UsageCost
	if err := json.Unmarshal(costRaw, &cost); err != nil {
		return llm.Usage{}, fmt.Errorf("invalid assistant usage cost")
	}
	if err := validateUsageCost(cost); err != nil {
		return llm.Usage{}, err
	}
	llmCost, err := usageCostToLLM(cost)
	if err != nil {
		return llm.Usage{}, err
	}
	return usage.WithCost(llmCost)
}

func usageCostToLLM(cost UsageCost) (llm.Cost, error) {
	var values [5]float64
	numbers := []json.Number{cost.Input, cost.Output, cost.CacheRead, cost.CacheWrite, cost.Total}
	for index, number := range numbers {
		value, err := number.Float64()
		if err != nil {
			return llm.Cost{}, fmt.Errorf("invalid assistant usage cost")
		}
		values[index] = value
	}
	return llm.Cost{Input: values[0], Output: values[1], CacheRead: values[2], CacheWrite: values[3], Total: values[4]}, nil
}

func decodeMessageTimestamp(object map[string]json.RawMessage) (time.Time, error) {
	raw, exists := object["timestamp"]
	if !exists {
		return time.Time{}, fmt.Errorf("message is missing timestamp")
	}
	value, err := decodeInt64(raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid message timestamp")
	}
	return time.UnixMilli(value).UTC(), nil
}

func decodeObject(raw []byte) (map[string]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("invalid JSON object")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, fmt.Errorf("invalid JSON object")
	}
	return object, nil
}

func requiredString(object map[string]json.RawMessage, key string) (string, error) {
	raw, exists := object[key]
	if !exists {
		return "", fmt.Errorf("missing %s", key)
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || !utf8.ValidString(value) {
		return "", fmt.Errorf("invalid %s", key)
	}
	return value, nil
}

func requiredUint64(object map[string]json.RawMessage, key string) (uint64, error) {
	raw, exists := object[key]
	if !exists {
		return 0, fmt.Errorf("missing usage %s", key)
	}
	value, err := decodeUint64(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid usage %s", key)
	}
	return value, nil
}

func decodeUint64(raw []byte) (uint64, error) {
	text := string(bytes.TrimSpace(raw))
	if text == "" || strings.ContainsAny(text, ".eE+-") {
		return 0, fmt.Errorf("not an unsigned integer")
	}
	return strconv.ParseUint(text, 10, 64)
}

func decodeInt64(raw []byte) (int64, error) {
	text := string(bytes.TrimSpace(raw))
	if text == "" || strings.ContainsAny(text, ".eE+") {
		return 0, fmt.Errorf("not an integer")
	}
	return strconv.ParseInt(text, 10, 64)
}

func parseError(kind error, path string, line int, message string, cause error) error {
	if cause != nil {
		return fmt.Errorf("%w: %s: line %d: %s: %w", kind, path, line, message, cause)
	}
	return fmt.Errorf("%w: %s: line %d: %s", kind, path, line, message)
}
