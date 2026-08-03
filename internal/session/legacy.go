package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// These bounds are deliberately above the upstream large-session fixture but
// keep an accidental pipe, sparse file, or hostile JSONL from consuming an
// unbounded amount of memory during an Open or migration rewrite.
const (
	maxSessionBytes = 64 << 20
	maxSessionLines = 1_000_000
	maxSessionLine  = 4 << 20
)

func checkSessionLimits(data []byte) error {
	if len(data) > maxSessionBytes {
		return fmt.Errorf("%w: %d bytes exceeds %d", ErrSessionTooLarge, len(data), maxSessionBytes)
	}
	lines := 0
	start := 0
	for start < len(data) {
		lines++
		if lines > maxSessionLines {
			return fmt.Errorf("%w: more than %d records", ErrSessionTooLarge, maxSessionLines)
		}
		end := bytes.IndexByte(data[start:], '\n')
		if end < 0 {
			end = len(data) - start
		} else {
			end++
		}
		if end > maxSessionLine {
			return fmt.Errorf("%w: line %d exceeds %d bytes", ErrSessionTooLarge, lines, maxSessionLine)
		}
		start += end
	}
	return nil
}

// sessionVersion identifies only a rigorously shaped coding-agent header. It
// intentionally does not guess based on an entry: a malformed/future file is
// rejected before a migration writer can touch it.
func sessionVersion(data []byte) (int, error) {
	if err := checkSessionLimits(data); err != nil {
		return 0, err
	}
	for _, line := range physicalLines(data) {
		if len(bytes.TrimSpace(line.data)) == 0 {
			continue
		}
		if !utf8.Valid(line.data) {
			return 0, parseError(ErrInvalidSession, "session", line.number, "record is not valid UTF-8", nil)
		}
		object, err := decodeObject(line.data)
		if err != nil {
			return 0, parseError(ErrInvalidSession, "session", line.number, "invalid header", err)
		}
		typeName, err := requiredString(object, "type")
		if err != nil || typeName != "session" {
			return 0, fmt.Errorf("%w: first record is not a session header", ErrInvalidSession)
		}
		version := 1
		if raw, exists := object["version"]; exists {
			if err := json.Unmarshal(raw, &version); err != nil || version < 1 {
				return 0, fmt.Errorf("%w: invalid session version", ErrInvalidSession)
			}
		}
		if version > 3 {
			return 0, fmt.Errorf("%w: version %d", ErrUnsupportedVersion, version)
		}
		return version, nil
	}
	return 0, fmt.Errorf("%w: missing header", ErrInvalidSession)
}

// migrateLegacySession is a pure source-snapshot transformation. The caller
// owns publication; keeping that separation is what lets Open fail without
// releasing a writable aggregate when a rename/sync outcome is unknown.
func migrateLegacySession(path string, source []byte, version int, newID IDGenerator) ([]byte, error) {
	if version != 1 && version != 2 {
		return nil, fmt.Errorf("%w: legacy migration received version %d", ErrInvalidSession, version)
	}
	lines := physicalLines(source)
	objects := make([]map[string]json.RawMessage, 0, len(lines))
	lineNumbers := make([]int, 0, len(lines))
	for _, line := range lines {
		if len(bytes.TrimSpace(line.data)) == 0 {
			continue
		}
		if !utf8.Valid(line.data) {
			return nil, parseError(ErrInvalidSession, path, line.number, "record is not valid UTF-8", nil)
		}
		object, err := decodeObject(line.data)
		if err != nil {
			return nil, parseError(ErrInvalidSession, path, line.number, "malformed legacy record", err)
		}
		objects = append(objects, object)
		lineNumbers = append(lineNumbers, line.number)
	}
	if len(objects) == 0 {
		return nil, fmt.Errorf("%w: %s: missing header", ErrInvalidSession, path)
	}
	if typeName, err := requiredString(objects[0], "type"); err != nil || typeName != "session" {
		return nil, fmt.Errorf("%w: %s: first record is not a session header", ErrInvalidSession, path)
	}
	if _, err := requiredString(objects[0], "id"); err != nil {
		return nil, fmt.Errorf("%w: %s: invalid legacy header id", ErrInvalidSession, path)
	}
	if _, err := requiredString(objects[0], "timestamp"); err != nil {
		return nil, fmt.Errorf("%w: %s: invalid legacy header timestamp", ErrInvalidSession, path)
	}
	if cwd, err := requiredString(objects[0], "cwd"); err != nil || strings.TrimSpace(cwd) == "" {
		return nil, fmt.Errorf("%w: %s: invalid legacy header cwd", ErrInvalidSession, path)
	}

	ids := make(map[string]struct{}, len(objects))
	previous := ""
	for index := 1; index < len(objects); index++ {
		object := objects[index]
		typeName, err := requiredString(object, "type")
		if err != nil || typeName == "session" {
			return nil, parseError(ErrInvalidEntry, path, lineNumbers[index], "invalid legacy entry type", err)
		}
		if version == 1 {
			if _, exists := object["id"]; exists {
				return nil, parseError(ErrInvalidEntry, path, lineNumbers[index], "v1 entry unexpectedly has id", nil)
			}
			if _, exists := object["parentId"]; exists {
				return nil, parseError(ErrInvalidEntry, path, lineNumbers[index], "v1 entry unexpectedly has parentId", nil)
			}
			var id string
			for attempts := 0; attempts < entryIDAttempts; attempts++ {
				candidate, generationErr := newID()
				if generationErr != nil {
					return nil, fmt.Errorf("%w: %w", ErrIDGeneration, generationErr)
				}
				if validateOpaqueID(candidate, "entry id") == nil {
					if _, exists := ids[candidate]; !exists {
						id = candidate
						break
					}
				}
			}
			if id == "" {
				return nil, ErrEntryIDExhausted
			}
			object["id"], _ = json.Marshal(id)
			if previous == "" {
				object["parentId"] = json.RawMessage("null")
			} else {
				object["parentId"], _ = json.Marshal(previous)
			}
			ids[id] = struct{}{}
			previous = id
			if typeName == "compaction" {
				if err := migrateV1Compaction(object, objects); err != nil {
					return nil, parseError(ErrInvalidEntry, path, lineNumbers[index], "invalid v1 compaction", err)
				}
			}
		} else {
			// v2 must already have a complete tree envelope; accepting a mixture
			// would invent ancestry and make a corrupt file look recoverable.
			if _, exists := object["id"]; !exists {
				return nil, parseError(ErrInvalidEntry, path, lineNumbers[index], "v2 entry is missing id", nil)
			}
			if _, exists := object["parentId"]; !exists {
				return nil, parseError(ErrInvalidEntry, path, lineNumbers[index], "v2 entry is missing parentId", nil)
			}
		}
		if typeName == "message" {
			migrateV2Message(object)
		}
		// v1/v2 accepted provider-only model changes. A v3 typed model_change
		// does not: preserving a missing ID would make later branch context
		// silently select an unusable model. Keep the legacy record opaque
		// instead of claiming it is a valid v3 model selection.
		if typeName == "model_change" {
			if _, exists := object["modelId"]; !exists {
				object["type"] = json.RawMessage(`"custom"`)
				object["customType"] = json.RawMessage(`"legacy_model_change"`)
				object["data"] = json.RawMessage(`{"missingModelId":true}`)
			}
		}
	}
	objects[0]["version"] = json.RawMessage("3")

	encoded := make([][]byte, 0, len(objects))
	for _, object := range objects {
		line, err := json.Marshal(object)
		if err != nil {
			return nil, fmt.Errorf("%w: encode migrated record: %w", ErrInvalidSession, err)
		}
		encoded = append(encoded, line)
	}
	return append(bytes.Join(encoded, []byte{'\n'}), '\n'), nil
}

func migrateV1Compaction(entry map[string]json.RawMessage, records []map[string]json.RawMessage) error {
	raw, exists := entry["firstKeptEntryIndex"]
	if !exists {
		return nil // A native firstKeptEntryId is validated by the v3 decoder.
	}
	var index int
	if err := json.Unmarshal(raw, &index); err != nil || index <= 0 || index >= len(records) {
		return fmt.Errorf("firstKeptEntryIndex does not identify an entry")
	}
	if typeName, _ := requiredString(records[index], "type"); typeName == "session" {
		return fmt.Errorf("firstKeptEntryIndex identifies header")
	}
	id, err := requiredString(records[index], "id")
	if err != nil || id == "" {
		return fmt.Errorf("firstKeptEntryIndex target has no generated id")
	}
	entry["firstKeptEntryId"], _ = json.Marshal(id)
	delete(entry, "firstKeptEntryIndex")
	return nil
}

func migrateV2Message(entry map[string]json.RawMessage) {
	raw, exists := entry["message"]
	if !exists {
		return
	}
	message, err := decodeObject(raw)
	if err != nil {
		return // v3 strict validation reports the malformed known payload later.
	}
	role, err := requiredString(message, "role")
	if err == nil && role == "hookMessage" {
		message["role"] = json.RawMessage(`"custom"`)
		if _, exists := message["customType"]; !exists {
			message["customType"] = json.RawMessage(`"hookMessage"`)
		}
		if encoded, marshalErr := json.Marshal(message); marshalErr == nil {
			entry["message"] = encoded
		}
	}
}

// RecoverTrailingPartial is deliberately separate from Open. It snapshots the
// original bytes into a no-clobber backup, then atomically replaces the source
// only when the sole bad data is one unterminated final, non-JSON line.
func RecoverTrailingPartial(path string) (RecoveryResult, error) {
	return recoverTrailingPartialWithStorage(osSessionStorage{}, path)
}

func recoverTrailingPartialWithStorage(storage sessionStorage, path string) (RecoveryResult, error) {
	resolved, err := resolveSessionPath(path)
	if err != nil {
		return RecoveryResult{}, err
	}
	claim, err := claimSessionWriter(resolved)
	if err != nil {
		return RecoveryResult{}, err
	}
	defer releaseSessionWriter(claim)
	data, err := storage.read(resolved)
	if err != nil {
		return RecoveryResult{}, fmt.Errorf("%w: read %s: %w", ErrStorage, resolved, err)
	}
	if err := checkSessionLimits(data); err != nil {
		return RecoveryResult{}, err
	}
	if len(data) == 0 || data[len(data)-1] == '\n' {
		return RecoveryResult{}, ErrRecoveryNotApplicable
	}
	start := bytes.LastIndexByte(data, '\n') + 1
	partial := data[start:]
	var ignored any
	if utf8.Valid(partial) && json.Unmarshal(partial, &ignored) == nil {
		return RecoveryResult{}, ErrRecoveryNotApplicable
	}
	prefix := data[:start]
	if _, _, _, _, err := decodeSessionFile(resolved, prefix); err != nil {
		return RecoveryResult{}, fmt.Errorf("%w: prefix is not a complete v3 session: %w", ErrRecoveryNotApplicable, err)
	}
	backup := backupName(resolved)
	if err := storage.validateReplace(resolved); err != nil {
		return RecoveryResult{}, err
	}
	created, backupErr := storage.create(backup, data)
	if backupErr != nil {
		if created {
			return RecoveryResult{}, fmt.Errorf("%w: create recovery backup: %w", ErrDurabilityUnknown, backupErr)
		}
		if os.IsExist(backupErr) {
			return RecoveryResult{}, fmt.Errorf("%w: %s", ErrRecoveryBackupExists, backup)
		}
		return RecoveryResult{}, fmt.Errorf("%w: create recovery backup: %w", ErrStorage, backupErr)
	}
	replaced, replaceErr := storage.replace(resolved, prefix)
	if replaceErr != nil {
		if replaced {
			return RecoveryResult{BackupPath: backup}, fmt.Errorf("%w: recovery publication: %w", ErrDurabilityUnknown, replaceErr)
		}
		return RecoveryResult{BackupPath: backup}, fmt.Errorf("%w: recovery publication: %w", ErrStorage, replaceErr)
	}
	result := RecoveryResult{BackupPath: backup, TruncatedBytes: int64(len(partial))}
	if err := refreshSessionWriterAfterRewrite(claim, resolved); err != nil {
		// Replacement has committed the recovered prefix. Preserve the writer
		// adoption cause, but classify the operation as post-publication.
		return result, fmt.Errorf("%w: adopt recovered session identity: %w", ErrDurabilityUnknown, err)
	}
	return result, nil
}

// backupName is intentionally fixed and no-clobber. It prevents a second
// recovery attempt from replacing the only forensic copy without a user first
// moving that copy aside.
func backupName(path string) string { return filepath.Clean(path) + ".partial-recovery.backup" }
