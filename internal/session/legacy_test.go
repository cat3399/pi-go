package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

type lockNewIdentityAfterReplaceStorage struct {
	osSessionStorage
	identityUnlock func()
	identityErr    error
	replacement    []byte
}

func (storage *lockNewIdentityAfterReplaceStorage) replace(path string, data []byte) (bool, error) {
	replaced, err := storage.osSessionStorage.replace(path, data)
	if err != nil || !replaced {
		return replaced, err
	}
	storage.replacement = append([]byte(nil), data...)
	storage.identityUnlock, storage.identityErr = claimProcessIdentityWriter(path)
	if storage.identityErr == nil && storage.identityUnlock == nil {
		storage.identityErr = errors.New("test identity lock was not acquired")
	}
	return true, nil
}

func (storage *lockNewIdentityAfterReplaceStorage) releaseIdentity() {
	if storage.identityUnlock != nil {
		storage.identityUnlock()
		storage.identityUnlock = nil
	}
}

func TestOpenMigratesV1ThroughDurableV3Rewrite(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "v1.jsonl")
	legacy := []byte(`{"type":"session","id":"old","timestamp":"2026-08-01T00:00:00Z","cwd":"/workspace","futureHeader":{"keep":true}}` + "\n" +
		`{"type":"message","timestamp":"2026-08-01T00:00:01Z","futureEntry":{"keep":1},"message":{"role":"user","content":"hello","timestamp":1,"futureMessage":{"keep":2}}}` + "\n" +
		`{"type":"model_change","timestamp":"2026-08-01T00:00:02Z","provider":"future"}` + "\n" +
		`{"type":"message","timestamp":"2026-08-01T00:00:03Z","message":{"role":"hookMessage","content":"opaque","timestamp":3,"customType":"x"}}` + "\n")
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}

	session, err := Open(path, OpenOptions{NewEntryID: sequenceIDs("one", "two", "three", "after")})
	if err != nil {
		t.Fatal(err)
	}
	if session.Header().Version() != 3 {
		t.Fatalf("version = %d, want 3", session.Header().Version())
	}
	entries := session.Entries()
	if got, want := entryIDs(entries), []string{"one", "two", "three"}; !equalIDs(got, want) {
		t.Fatalf("migrated IDs = %v, want %v", got, want)
	}
	for index, entry := range entries {
		parent, hasParent := entry.ParentID()
		if index == 0 && hasParent {
			t.Fatalf("root parent = %q", parent)
		}
		if index > 0 && (!hasParent || parent != entries[index-1].ID()) {
			t.Fatalf("entry %d parent = (%q, %t)", index, parent, hasParent)
		}
	}
	if !bytes.Contains(entries[0].RawJSON(), []byte(`"futureEntry":{"keep":1}`)) || !bytes.Contains(session.Header().RawJSON(), []byte(`"futureHeader":{"keep":true}`)) {
		t.Fatal("migration did not preserve unknown JSON values")
	}
	if !bytes.Contains(entries[2].RawJSON(), []byte(`"role":"custom"`)) {
		t.Fatalf("hook message was not renamed: %s", entries[2].RawJSON())
	}
	if _, err := session.Append(context.Background(), mustUserMessage(t, "next", time.UnixMilli(4)), AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	afterFirstOpen, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	afterSecondOpen, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterFirstOpen, afterSecondOpen) {
		t.Fatal("opening an already migrated session rewrote it again")
	}
}

func TestOpenMigratesV2WithoutChangingExistingIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.jsonl")
	data := []byte(`{"type":"session","version":2,"id":"old","timestamp":"2026-08-01T00:00:00Z","cwd":"/workspace"}` + "\n" +
		`{"type":"message","id":"root","parentId":null,"timestamp":"2026-08-01T00:00:01Z","message":{"role":"hookMessage","content":"opaque","timestamp":1,"details":{"unknown":true}}}` + "\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := Open(path, OpenOptions{NewEntryID: sequenceIDs("unused")})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if got := session.Entries()[0].ID(); got != "root" {
		t.Fatalf("v2 id = %q, want root", got)
	}
	if !bytes.Contains(session.Entries()[0].RawJSON(), []byte(`"role":"custom"`)) || !bytes.Contains(session.Entries()[0].RawJSON(), []byte(`"details":{"unknown":true}`)) {
		t.Fatalf("v2 payload = %s", session.Entries()[0].RawJSON())
	}
}

func TestOpenMigratesOfficialLegacyAssistantPayloadCompatibilityAndReopens(t *testing.T) {
	for _, test := range []struct {
		name           string
		header         string
		entryEnvelope  string
		content        string
		wantContentKey bool
	}{
		{
			name:          "v1 missing content",
			header:        `{"type":"session","id":"old-v1","timestamp":"2025-01-01T00:00:00Z","cwd":"/tmp"}`,
			entryEnvelope: `{"type":"message","timestamp":"2025-01-01T00:00:01Z",`,
		},
		{
			name:           "v2 null content",
			header:         `{"type":"session","version":2,"id":"old-v2","timestamp":"2025-01-01T00:00:00Z","cwd":"/tmp"}`,
			entryEnvelope:  `{"type":"message","id":"assistant-v2","parentId":null,"timestamp":"2025-01-01T00:00:01Z",`,
			content:        `"content":null,`,
			wantContentKey: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "legacy-assistant.jsonl")
			// This usage shape is copied from pi's current migration fixture: it
			// predates both totalTokens and the nested cost object.
			assistant := test.entryEnvelope + `"message":{"role":"assistant",` + test.content + `"api":"test","provider":"test","model":"test","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0},"stopReason":"stop","timestamp":2}}`
			legacy := []byte(test.header + "\n" + assistant + "\n")
			if err := os.WriteFile(path, legacy, 0o600); err != nil {
				t.Fatal(err)
			}
			volatilePath := filepath.Join(directory, "legacy-assistant-volatile.jsonl")
			if err := os.WriteFile(volatilePath, legacy, 0o600); err != nil {
				t.Fatal(err)
			}
			volatile, err := loadVolatileSessionSnapshot(volatilePath, normalizeRuntime(nil, sequenceIDs("assistant-v1")))
			if err != nil {
				t.Fatalf("load compatible legacy assistant in memory: %v", err)
			}
			volatileEntries := volatile.Entries()
			if len(volatileEntries) != 1 || len(volatileEntries[0].Diagnostics()) != 0 {
				_ = volatile.Close()
				t.Fatalf("volatile legacy entries = %#v", volatileEntries)
			}
			if role, ok := volatileEntries[0].MessageRole(); !ok || role != "assistant" {
				_ = volatile.Close()
				t.Fatalf("volatile legacy role = %q, %t", role, ok)
			}
			if err := volatile.Close(); err != nil {
				t.Fatal(err)
			}
			if unchanged, readErr := os.ReadFile(volatilePath); readErr != nil || !bytes.Equal(unchanged, legacy) {
				t.Fatalf("volatile migration changed source: %v", readErr)
			}
			opened, err := Open(path, OpenOptions{NewEntryID: sequenceIDs("assistant-v1")})
			if err != nil {
				t.Fatalf("migrate compatible legacy assistant: %v", err)
			}
			entries := opened.Entries()
			if len(entries) != 1 {
				t.Fatalf("migrated entries = %#v", entries)
			}
			if role, ok := entries[0].MessageRole(); !ok || role != "assistant" {
				t.Fatalf("migrated message role = %q, %t", role, ok)
			}
			if _, ok := entries[0].Message(); !ok || len(entries[0].Diagnostics()) != 0 {
				t.Fatalf("legacy assistant was not compatibly projected: %#v", entries[0].Diagnostics())
			}
			raw := entries[0].RawJSON()
			if bytes.Contains(raw, []byte(`"totalTokens"`)) || bytes.Contains(raw, []byte(`"cost"`)) {
				t.Fatalf("migration fabricated legacy usage fields in retained raw: %s", raw)
			}
			if got := bytes.Contains(raw, []byte(`"content"`)); got != test.wantContentKey {
				t.Fatalf("retained content presence = %t, want %t: %s", got, test.wantContentKey, raw)
			}
			if err := opened.Close(); err != nil {
				t.Fatal(err)
			}
			migrated, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(path, OpenOptions{})
			if err != nil {
				t.Fatalf("reopen migrated compatible payload: %v", err)
			}
			if second := reopened.Entries(); len(second) != 1 || len(second[0].Diagnostics()) != 0 {
				_ = reopened.Close()
				t.Fatalf("reopened entries = %#v", second)
			}
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
			afterReopen, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(afterReopen, migrated) {
				t.Fatalf("reopen rewrote migrated legacy payload: %v", err)
			}
		})
	}
}

func TestOpenMigratesV1CompactionIndexToParentPathID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1-compaction.jsonl")
	data := []byte(`{"type":"session","id":"old","timestamp":"2026-08-01T00:00:00Z","cwd":"/workspace"}` + "\n" +
		`{"type":"message","timestamp":"2026-08-01T00:00:01Z","message":{"role":"user","content":"before","timestamp":1}}` + "\n" +
		`{"type":"compaction","timestamp":"2026-08-01T00:00:02Z","summary":"summary","firstKeptEntryIndex":1,"tokensBefore":1}` + "\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := Open(path, OpenOptions{NewEntryID: sequenceIDs("root", "compact")})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	compaction, ok := session.Entries()[1].Compaction()
	if !ok || compaction.FirstKeptEntryID != "root" || bytes.Contains(session.Entries()[1].RawJSON(), []byte("firstKeptEntryIndex")) {
		t.Fatalf("migrated compaction = %#v / %s", compaction, session.Entries()[1].RawJSON())
	}
}

func TestLegacyMigrationRejectsMixedOrMalformedInputWithoutOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mixed-v1.jsonl")
	original := []byte(`{"type":"session","id":"old","timestamp":"2026-08-01T00:00:00Z","cwd":"/workspace"}` + "\n" +
		`{"type":"message","id":"unexpected","timestamp":"2026-08-01T00:00:01Z","message":{"role":"user","content":"hi","timestamp":1}}` + "\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, OpenOptions{NewEntryID: sequenceIDs("one")}); !errors.Is(err, ErrInvalidEntry) {
		t.Fatalf("Open mixed v1 = %v, want invalid entry", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("rejected legacy input was overwritten")
	}
}

func TestLegacyMigrationValidatesCandidateBeforeOverwrite(t *testing.T) {
	for name, original := range map[string][]byte{
		"v1 malformed physical line": []byte("not-json\n" + `{"type":"session","id":"old","timestamp":"2026-08-01T00:00:00Z","cwd":"/workspace"}` + "\n"),
		"v1 missing entry timestamp": []byte(`{"type":"session","id":"old","timestamp":"2026-08-01T00:00:00Z","cwd":"/workspace"}` + "\n" +
			`{"type":"message","message":{"role":"user","timestamp":1}}` + "\n"),
		"v2 broken parent": []byte(`{"type":"session","version":2,"id":"old","timestamp":"2026-08-01T00:00:00Z","cwd":"/workspace"}` + "\n" +
			`{"type":"future","id":"orphan","parentId":"missing","timestamp":"2026-08-01T00:00:01Z"}` + "\n"),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "invalid-legacy.jsonl")
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path, OpenOptions{NewEntryID: sequenceIDs("one")}); err == nil {
				t.Fatal("Open succeeded for invalid migrated candidate")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, original) {
				t.Fatalf("invalid candidate changed source: %v / %q", err, after)
			}
		})
	}
}

func TestMigrationPublicationUncertaintyDoesNotPublishWritableSession(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "legacy.jsonl")
	data := []byte(`{"type":"session","id":"old","timestamp":"2026-08-01T00:00:00Z","cwd":"/workspace"}` + "\n" +
		`{"type":"message","timestamp":"2026-08-01T00:00:01Z","message":{"role":"user","content":"hi","timestamp":1}}` + "\n")
	storage := &fakeStorage{readData: data, replaceDone: true, replaceErr: errors.New("directory sync failed")}
	if _, err := openWithStorage(storage, path, OpenOptions{NewEntryID: sequenceIDs("one")}); !errors.Is(err, ErrDurabilityUnknown) {
		t.Fatalf("open migration = %v, want durability unknown", err)
	}
	if storage.replaceCalled != 1 || len(storage.replaceData) == 0 {
		t.Fatalf("migration replace = calls %d data %q", storage.replaceCalled, storage.replaceData)
	}
}

func TestMigrationPostPublicationIdentityLockFailureIsDurabilityUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.jsonl")
	legacy := []byte(`{"type":"session","id":"old","timestamp":"2026-08-01T00:00:00Z","cwd":"/workspace"}` + "\n" +
		`{"type":"message","timestamp":"2026-08-01T00:00:01Z","message":{"role":"user","content":"hello","timestamp":1}}` + "\n")
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	storage := &lockNewIdentityAfterReplaceStorage{}
	defer storage.releaseIdentity()
	session, err := openWithStorage(storage, path, OpenOptions{NewEntryID: sequenceIDs("root")})
	if session != nil {
		_ = session.Close()
		t.Fatal("post-publication identity failure returned a writable session")
	}
	if storage.identityErr != nil {
		t.Fatalf("install post-publication identity lock: %v", storage.identityErr)
	}
	if !errors.Is(err, ErrDurabilityUnknown) || !errors.Is(err, ErrWriterActive) {
		t.Fatalf("post-publication migration error = %v, want durability-unknown + writer-active", err)
	}
	published, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(published, storage.replacement) || bytes.Equal(published, legacy) {
		t.Fatalf("post-publication migration bytes = %q, read %v", published, readErr)
	}
	blocked, blockedErr := Open(path, OpenOptions{})
	if blocked != nil {
		_ = blocked.Close()
	}
	if !errors.Is(blockedErr, ErrWriterActive) {
		t.Fatalf("reopen while new identity is locked = %v, want writer-active", blockedErr)
	}
	storage.releaseIdentity()
	reopened, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatalf("reopen migrated publication: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	afterReopen, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(afterReopen, published) {
		t.Fatalf("reopen rewrote migrated publication: %v / %q", err, afterReopen)
	}
}

func TestMigrationUnsupportedAtomicReplacementFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.jsonl")
	data := []byte(`{"type":"session","id":"old","timestamp":"2026-08-01T00:00:00Z","cwd":"/workspace"}` + "\n")
	storage := &fakeStorage{readData: data, replaceErr: ErrAtomicReplaceUnsupported}
	if _, err := openWithStorage(storage, path, OpenOptions{}); !errors.Is(err, ErrStorage) || !errors.Is(err, ErrAtomicReplaceUnsupported) {
		t.Fatalf("unsupported migration replacement = %v, want storage + atomic-replace-unsupported", err)
	}
	if storage.replaceCalled != 1 || storage.replaceDone {
		t.Fatalf("unsupported migration replacement = calls %d, published %t", storage.replaceCalled, storage.replaceDone)
	}
}

func TestRecoverTrailingPartialRequiresExplicitBackupThenReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial.jsonl")
	prefix := []byte(testHeader + "\n" + userEntryJSON("root", "entry-1", "null", 1) + "\n")
	original := append(append([]byte(nil), prefix...), []byte(`{"type":"message"`)...)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatalf("ordinary Open partial = %v, want compatible read", err)
	}
	diagnostics := opened.LoadDiagnostics()
	if len(diagnostics) != 1 || diagnostics[0].Code != LoadDiagnosticMalformedLine {
		t.Fatalf("ordinary Open diagnostics = %#v", diagnostics)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(unchanged, original) {
		t.Fatalf("ordinary Open changed partial: %v / %q", err, unchanged)
	}
	result, err := RecoverTrailingPartial(path)
	if err != nil {
		t.Fatal(err)
	}
	if result.TruncatedBytes != int64(len(original)-len(prefix)) {
		t.Fatalf("truncated = %d", result.TruncatedBytes)
	}
	backup, err := os.ReadFile(result.BackupPath)
	if err != nil || !bytes.Equal(backup, original) {
		t.Fatalf("backup = %q, err %v", backup, err)
	}
	if repaired, err := os.ReadFile(path); err != nil || !bytes.Equal(repaired, prefix) {
		t.Fatalf("repaired = %q, err %v", repaired, err)
	}
	session, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := RecoverTrailingPartial(path); !errors.Is(err, ErrWriterActive) {
		t.Fatalf("recovery during active Open = %v, want writer active", err)
	}
}

func TestRecoverTrailingPartialAcceptsCompatibleOldPayloadPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old-payload-partial.jsonl")
	oldAssistant := `{"type":"message","id":"old-assistant","parentId":null,"timestamp":"2026-08-01T00:00:01Z","message":{"role":"assistant","content":null,"api":"test","provider":"test","model":"test","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0},"stopReason":"stop","timestamp":2}}`
	prefix := []byte(testHeader + "\n" + oldAssistant + "\n")
	original := append(append([]byte(nil), prefix...), '{')
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverTrailingPartial(path); err != nil {
		t.Fatalf("recover compatible old-payload prefix: %v", err)
	}
	repaired, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(repaired, prefix) {
		t.Fatalf("repaired compatible prefix = %q, %v", repaired, err)
	}
	reopened, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	entries := reopened.Entries()
	if len(entries) != 1 || len(entries[0].Diagnostics()) != 0 {
		t.Fatalf("recovered old payload entries = %#v", entries)
	}
}

func TestRecoveryPostPublicationIdentityLockFailureIsDurabilityUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial.jsonl")
	prefix := []byte(testHeader + "\n" + userEntryJSON("root", "entry-1", "null", 1) + "\n")
	original := append(append([]byte(nil), prefix...), '{')
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	storage := &lockNewIdentityAfterReplaceStorage{}
	defer storage.releaseIdentity()
	result, err := recoverTrailingPartialWithStorage(storage, path)
	if storage.identityErr != nil {
		t.Fatalf("install post-publication identity lock: %v", storage.identityErr)
	}
	if !errors.Is(err, ErrDurabilityUnknown) || !errors.Is(err, ErrWriterActive) {
		t.Fatalf("post-publication recovery error = %v, want durability-unknown + writer-active", err)
	}
	if result.BackupPath != backupName(path) || result.TruncatedBytes != 1 {
		t.Fatalf("post-publication recovery result = %#v", result)
	}
	published, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(published, prefix) || !bytes.Equal(published, storage.replacement) {
		t.Fatalf("post-publication recovery bytes = %q, read %v", published, readErr)
	}
	backup, backupErr := os.ReadFile(result.BackupPath)
	if backupErr != nil || !bytes.Equal(backup, original) {
		t.Fatalf("post-publication recovery backup = %q, read %v", backup, backupErr)
	}
	blocked, blockedErr := Open(path, OpenOptions{})
	if blocked != nil {
		_ = blocked.Close()
	}
	if !errors.Is(blockedErr, ErrWriterActive) {
		t.Fatalf("reopen recovered file while new identity is locked = %v, want writer-active", blockedErr)
	}
	storage.releaseIdentity()
	reopened, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatalf("reopen recovered publication: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverTrailingPartial(path); !errors.Is(err, ErrRecoveryNotApplicable) {
		t.Fatalf("repeat recovery = %v, want not-applicable", err)
	}
}

func TestRecoveryRefusesMiddleCorruptionAndCompleteTail(t *testing.T) {
	for name, data := range map[string][]byte{
		"middle":   []byte(testHeader + "\nnot-json\n" + userEntryJSON("root", "entry-1", "null", 1) + "\n"),
		"complete": []byte(testHeader + "\n" + userEntryJSON("root", "entry-1", "null", 1)),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "unsafe.jsonl")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := RecoverTrailingPartial(path); !errors.Is(err, ErrRecoveryNotApplicable) {
				t.Fatalf("RecoverTrailingPartial = %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, data) {
				t.Fatalf("recovery changed unsafe file: %v", err)
			}
		})
	}
}

func TestRecoveryRejectsHardlinkedSourceBeforeBackupOrRewrite(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "partial.jsonl")
	original := []byte(testHeader + "\n" + userEntryJSON("root", "entry-1", "null", 1) + "\n{")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	hardLink := filepath.Join(directory, "partial-hardlink.jsonl")
	if err := os.Link(path, hardLink); err != nil {
		t.Skipf("hardlink alias unavailable: %v", err)
	}
	if _, err := RecoverTrailingPartial(path); !errors.Is(err, ErrUnsafeWriterAlias) {
		t.Fatalf("hardlinked recovery = %v, want unsafe alias", err)
	}
	if _, err := os.Stat(backupName(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe recovery created backup: %v", err)
	}
	for _, candidate := range []string{path, hardLink} {
		after, err := os.ReadFile(candidate)
		if err != nil || !bytes.Equal(after, original) {
			t.Fatalf("unsafe recovery changed %s: %v / %q", candidate, err, after)
		}
	}
}

func TestLegacyMigrationPreservesJSONValuesRatherThanDecodingUnknownNumbers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "numbers.jsonl")
	data := []byte(`{"type":"session","id":"old","timestamp":"2026-08-01T00:00:00Z","cwd":"/workspace","future":900719925474099312345}` + "\n" +
		`{"type":"future","timestamp":"2026-08-01T00:00:01Z","payload":{"n":1.00e2}}` + "\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := Open(path, OpenOptions{NewEntryID: sequenceIDs("one")})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	var header map[string]json.RawMessage
	if err := json.Unmarshal(session.Header().RawJSON(), &header); err != nil || string(header["future"]) != "900719925474099312345" {
		t.Fatalf("header future = %s, err %v", header["future"], err)
	}
}

func TestOpenClaimsWriterAcrossProcesses(t *testing.T) {
	if os.Getenv("PI_GO_SESSION_LOCK_HELPER") == "1" {
		_, err := Open(os.Getenv("PI_GO_SESSION_LOCK_PATH"), OpenOptions{})
		want := ErrWriterActive
		if os.Getenv("PI_GO_SESSION_LOCK_WANT") == "unsafe-alias" {
			want = ErrUnsafeWriterAlias
		}
		if errors.Is(err, want) {
			os.Exit(0)
		}
		os.Exit(1)
	}
	path := filepath.Join(t.TempDir(), "locked.jsonl")
	session, err := Create(path, CreateOptions{ID: "locked", WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	runWriterClaimHelper(t, path, "active")

	t.Run("hardlink alias", func(t *testing.T) {
		hardLink := filepath.Join(filepath.Dir(path), "locked-hardlink.jsonl")
		if err := os.Link(path, hardLink); err != nil {
			t.Skipf("hardlink alias unavailable: %v", err)
		}
		runWriterClaimHelper(t, hardLink, "active")
	})

	t.Run("symlink alias", func(t *testing.T) {
		symlink := filepath.Join(filepath.Dir(path), "locked-symlink.jsonl")
		if err := os.Symlink(path, symlink); err != nil {
			t.Skipf("symlink alias unavailable: %v", err)
		}
		runWriterClaimHelper(t, symlink, "active")
	})
}

func TestLegacyMigrationRejectsHardlinkAliasAcrossProcesses(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "legacy.jsonl")
	original := []byte(`{"type":"session","id":"old","timestamp":"2026-08-01T00:00:00Z","cwd":"/workspace"}` + "\n" +
		`{"type":"message","timestamp":"2026-08-01T00:00:01Z","message":{"role":"user","content":"hi","timestamp":1}}` + "\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	hardLink := filepath.Join(directory, "legacy-hardlink.jsonl")
	if err := os.Link(path, hardLink); err != nil {
		t.Skipf("hardlink alias unavailable: %v", err)
	}
	if _, err := Open(path, OpenOptions{NewEntryID: sequenceIDs("one")}); !errors.Is(err, ErrUnsafeWriterAlias) {
		t.Fatalf("legacy hardlink migration = %v, want unsafe alias", err)
	}
	runWriterClaimHelper(t, hardLink, "unsafe-alias")
	for _, candidate := range []string{path, hardLink} {
		after, err := os.ReadFile(candidate)
		if err != nil || !bytes.Equal(after, original) {
			t.Fatalf("rejected alias %s changed: %v / %q", candidate, err, after)
		}
	}
}

func TestLegacyMigrationThroughSymlinkKeepsAliasAndLocksCanonicalTarget(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "legacy-target.jsonl")
	legacy := []byte(`{"type":"session","id":"old","timestamp":"2026-08-01T00:00:00Z","cwd":"/workspace"}` + "\n")
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(directory, "legacy-symlink.jsonl")
	if err := os.Symlink(path, symlink); err != nil {
		t.Skipf("symlink alias unavailable: %v", err)
	}
	session, err := Open(symlink, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if info, err := os.Lstat(symlink); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("migration replaced symlink itself: %v / %v", info, err)
	}
	runWriterClaimHelper(t, path, "active")
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(data, []byte(`"version":3`)) {
		t.Fatalf("canonical target was not migrated: %v / %q", err, data)
	}
}

func runWriterClaimHelper(t *testing.T, path, want string) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=TestOpenClaimsWriterAcrossProcesses")
	command.Env = append(os.Environ(),
		"PI_GO_SESSION_LOCK_HELPER=1",
		"PI_GO_SESSION_LOCK_PATH="+path,
		"PI_GO_SESSION_LOCK_WANT="+want,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("cross-process writer claim for %s (%s): %v: %s", path, want, err, output)
	}
}

func TestSessionAdmissionAcceptsLinesBeyondFormerPiGoLimit(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "large-line.jsonl")
	payload := strings.Repeat("x", 4<<20+1)
	data := testHeader + "\n" + `{"type":"future","id":"large","parentId":null,"timestamp":"2026-08-01T00:00:01Z","payload":` + strconv.Quote(payload) + "}\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatalf("Open() rejected a line accepted by current pi: %v", err)
	}
	defer session.Close()
	if entries := session.Entries(); len(entries) != 1 || entries[0].ID() != "large" {
		t.Fatalf("large-line entries = %#v", entries)
	}
}

func TestOpenStreamsSparseMalformedTailWithBoundedWorkingMemory(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "sparse-tail.jsonl")
	// The bad line begins with a complete JSON value. The trailing non-space is
	// enough to reject it; the remaining sparse extent must be drained without
	// retaining either the first value or the tail.
	prefix := []byte(testHeader + "\n{}x")
	if err := os.WriteFile(path, prefix, 0o600); err != nil {
		t.Fatal(err)
	}
	const sparseTailBytes = int64(96 << 20)
	if err := os.Truncate(path, int64(len(prefix))+sparseTailBytes); err != nil {
		t.Fatal(err)
	}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	session, err := Open(path, OpenOptions{})
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if diagnostics := session.LoadDiagnostics(); len(diagnostics) != 1 || diagnostics[0].Code != LoadDiagnosticMalformedLine || diagnostics[0].Line != 2 {
		t.Fatalf("sparse-tail diagnostics = %#v", diagnostics)
	}
	// A whole-file read necessarily allocates at least the 96 MiB logical file.
	// The streaming decoder retains only its 1 MiB read buffer plus decoded
	// records; leave generous headroom for the runtime and filesystem wrappers.
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("streamed %d-byte sparse tail with %d bytes of total allocation", sparseTailBytes, allocated)
	if allocated >= 32<<20 {
		t.Fatalf("streaming sparse open allocated %d bytes", allocated)
	}
}

func TestOpenAggregatesMalformedShortLineFloodWithBoundedRetainedMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "malformed-flood.jsonl")
	const malformedLines = 250_000
	data := make([]byte, 0, len(testHeader)+1+malformedLines*2)
	data = append(data, testHeader...)
	data = append(data, '\n')
	data = append(data, strings.Repeat("x\n", malformedLines)...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	data = nil

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	session, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	runtime.GC()
	runtime.ReadMemStats(&after)
	diagnostics := session.LoadDiagnostics()
	if len(diagnostics) != maxLoadDiagnosticSamplesPerCode+1 {
		t.Fatalf("bounded diagnostics length = %d, want %d: %#v", len(diagnostics), maxLoadDiagnosticSamplesPerCode+1, diagnostics)
	}
	var represented uint64
	for index, diagnostic := range diagnostics {
		if diagnostic.Code != LoadDiagnosticMalformedLine || diagnostic.Count == 0 {
			t.Fatalf("diagnostic %d = %#v", index, diagnostic)
		}
		represented += diagnostic.Count
	}
	summary := diagnostics[len(diagnostics)-1]
	if !summary.Truncated || summary.Line != maxLoadDiagnosticSamplesPerCode+2 || summary.Count != malformedLines-maxLoadDiagnosticSamplesPerCode {
		t.Fatalf("malformed summary = %#v", summary)
	}
	if represented != malformedLines {
		t.Fatalf("represented malformed count = %d, want %d", represented, malformedLines)
	}
	retained := uint64(0)
	if after.HeapAlloc > before.HeapAlloc {
		retained = after.HeapAlloc - before.HeapAlloc
	}
	t.Logf("retained %d bytes for %d malformed physical lines in %d diagnostics", retained, malformedLines, len(diagnostics))
	if retained >= 4<<20 {
		t.Fatalf("malformed diagnostic flood retained %d bytes", retained)
	}
	runtime.KeepAlive(session)
}

func FuzzMigrateLegacySessionNeverPanics(f *testing.F) {
	f.Add([]byte(`{"type":"session","id":"s","timestamp":"2026-08-01T00:00:00Z","cwd":"/w"}` + "\n"))
	f.Add([]byte(`{"type":"session","version":2,"id":"s","timestamp":"2026-08-01T00:00:00Z","cwd":"/w"}` + "\n" + `{"type":"message","id":"a","parentId":null,"timestamp":"2026-08-01T00:00:01Z","message":{"role":"hookMessage","timestamp":1}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		version, err := sessionVersion(data)
		if err != nil || version >= 3 {
			return
		}
		_, _ = migrateLegacySession("fuzz", data, version, sequenceIDs("a", "b", "c", "d"))
	})
}

func FuzzRecoverTrailingPartialNeverMutatesWithoutSuccess(f *testing.F) {
	f.Add([]byte(testHeader + "\n" + userEntryJSON("root", "entry-1", "null", 1) + "\n{"))
	f.Add([]byte(testHeader + "\nnot-json\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(t.TempDir(), "candidate.jsonl")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		before := append([]byte(nil), data...)
		result, err := RecoverTrailingPartial(path)
		after, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if err != nil {
			if !bytes.Equal(after, before) {
				t.Fatalf("failed recovery changed input: %v", err)
			}
			return
		}
		backup, backupErr := os.ReadFile(result.BackupPath)
		if backupErr != nil || !bytes.Equal(backup, before) {
			t.Fatalf("successful recovery backup = %q, err %v", backup, backupErr)
		}
		opened, openErr := Open(path, OpenOptions{})
		if openErr != nil {
			t.Fatalf("successful recovery did not produce strict session: %v", openErr)
		}
		_ = opened.Close()
	})
}
