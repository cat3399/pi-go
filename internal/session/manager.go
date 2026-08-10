package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
)

// SessionManager owns pi's session semantics. Session remains the Store-facing
// durable aggregate: the manager chooses the active leaf, projects context,
// resolves labels, extracts branches, and manages session lifecycle.
type SessionManager struct {
	mu          sync.RWMutex
	cwd         string
	sessionDir  string
	sessionFile string
	persist     bool
	flushed     bool
	runtime     runtimeConfig
	store       *Session
}

var managerLineBreakPattern = regexp.MustCompile(`[\r\n]+`)

// CreateSessionManager creates a persistent manager. Like pi, it reserves a
// path but delays creating the JSONL until the first assistant message.
func CreateSessionManager(cwd, sessionDir string, options NewSessionOptions) (*SessionManager, error) {
	return CreateSessionManagerWithOptions(cwd, sessionDir, ManagerOptions{NewSession: options})
}

func CreateSessionManagerWithOptions(cwd, sessionDir string, options ManagerOptions) (*SessionManager, error) {
	resolvedCwd, err := resolveWorkingDir(cwd)
	if err != nil {
		return nil, err
	}
	dir, err := resolveManagerSessionDir(resolvedCwd, sessionDir, true)
	if err != nil {
		return nil, err
	}
	m := &SessionManager{cwd: resolvedCwd, sessionDir: dir, persist: true, runtime: normalizeRuntime(options.Now, options.NewEntryID)}
	if err := m.startNew(options.NewSession); err != nil {
		return nil, err
	}
	return m, nil
}

// OpenSessionManager opens one explicit session path. Missing files start a
// delayed new session at that exact path; an existing empty file is initialized
// immediately, matching the original explicit --session behavior.
func OpenSessionManager(path, sessionDir, cwdOverride string) (*SessionManager, error) {
	return OpenSessionManagerWithOptions(path, sessionDir, cwdOverride, ManagerOptions{})
}

func OpenSessionManagerWithOptions(path, sessionDir, cwdOverride string, options ManagerOptions) (*SessionManager, error) {
	resolvedPath, err := resolveSessionPath(path)
	if err != nil {
		return nil, err
	}
	dir := sessionDir
	if dir == "" {
		dir = filepath.Dir(resolvedPath)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve session directory: %v", ErrInvalidSession, err)
	}
	dir = filepath.Clean(dir)
	runtime := normalizeRuntime(options.Now, options.NewEntryID)
	info, statErr := os.Stat(resolvedPath)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: stat %s: %v", ErrStorage, resolvedPath, statErr)
	}
	if errors.Is(statErr, os.ErrNotExist) {
		cwd := cwdOverride
		if cwd == "" {
			cwd, err = os.Getwd()
			if err != nil {
				return nil, err
			}
		}
		cwd, err = resolveWorkingDir(cwd)
		if err != nil {
			return nil, err
		}
		header, err := newManagerHeader(cwd, options.NewSession, runtime)
		if err != nil {
			return nil, err
		}
		return &SessionManager{cwd: cwd, sessionDir: dir, sessionFile: resolvedPath, persist: true, runtime: runtime, store: newVolatileSession(header, nil, runtime)}, nil
	}
	if info.Size() == 0 {
		cwd := cwdOverride
		if cwd == "" {
			cwd, err = os.Getwd()
			if err != nil {
				return nil, err
			}
		}
		resolvedCwd, err := resolveWorkingDir(cwd)
		if err != nil {
			return nil, err
		}
		header, err := newManagerHeader(resolvedCwd, options.NewSession, runtime)
		if err != nil {
			return nil, err
		}
		s, err := initializeEmptySession(resolvedPath, header, runtime)
		if err != nil {
			return nil, err
		}
		return &SessionManager{cwd: s.Header().WorkingDir(), sessionDir: dir, sessionFile: resolvedPath, persist: true, flushed: true, runtime: runtime, store: s}, nil
	}
	s, err := Open(resolvedPath, OpenOptions{Now: runtime.now, NewEntryID: runtime.newEntryID})
	if err != nil {
		return nil, fmt.Errorf("session file is not a valid pi session: %s: %w", resolvedPath, err)
	}
	cwd := s.Header().WorkingDir()
	if cwdOverride != "" {
		cwd, err = resolveWorkingDir(cwdOverride)
		if err != nil {
			s.Close()
			return nil, err
		}
	}
	return &SessionManager{cwd: cwd, sessionDir: dir, sessionFile: resolvedPath, persist: true, flushed: true, runtime: runtime, store: s}, nil
}

// InMemorySessionManager creates a manager with all normal tree/context
// semantics and no filesystem persistence.
func InMemorySessionManager(cwd string, options NewSessionOptions) (*SessionManager, error) {
	return InMemorySessionManagerWithOptions(cwd, ManagerOptions{NewSession: options})
}

func InMemorySessionManagerWithOptions(cwd string, options ManagerOptions) (*SessionManager, error) {
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	resolvedCwd, err := resolveWorkingDir(cwd)
	if err != nil {
		return nil, err
	}
	runtime := normalizeRuntime(options.Now, options.NewEntryID)
	header, err := newManagerHeader(resolvedCwd, options.NewSession, runtime)
	if err != nil {
		return nil, err
	}
	return &SessionManager{cwd: resolvedCwd, persist: false, runtime: runtime, store: newVolatileSession(header, nil, runtime)}, nil
}

// ContinueRecentSession opens the newest compatible session, or starts a new
// one if discovery finds none. A custom flat directory is scoped by header cwd.
func ContinueRecentSession(cwd, sessionDir string) (*SessionManager, error) {
	resolvedCwd, err := resolveWorkingDir(cwd)
	if err != nil {
		return nil, err
	}
	dir, err := resolveManagerSessionDir(resolvedCwd, sessionDir, true)
	if err != nil {
		return nil, err
	}
	filter := ""
	if sessionDir != "" && dir != defaultSessionDirPath(resolvedCwd, "") {
		filter = resolvedCwd
	}
	recent, err := FindMostRecentSession(dir, filter)
	if err != nil {
		return nil, err
	}
	if recent == "" {
		return CreateSessionManager(resolvedCwd, dir, NewSessionOptions{})
	}
	return OpenSessionManager(recent, dir, resolvedCwd)
}

func (m *SessionManager) IsPersisted() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.persist
}
func (m *SessionManager) Cwd() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cwd
}
func (m *SessionManager) SessionDir() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessionDir
}
func (m *SessionManager) SessionID() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.store.Header().ID()
}
func (m *SessionManager) Header() Header {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.store.Header()
}
func (m *SessionManager) Entries() []Entry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.store.Entries()
}
func (m *SessionManager) LoadDiagnostics() []LoadDiagnostic {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.store.LoadDiagnostics()
}
func (m *SessionManager) SessionFile() (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessionFile, m.sessionFile != ""
}
func (m *SessionManager) UsesDefaultSessionDir() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.persist && m.sessionDir == defaultSessionDirPath(m.cwd, "")
}

func (m *SessionManager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.store == nil {
		return nil
	}
	return m.store.Close()
}

func (m *SessionManager) SetSessionFile(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	resolved, err := resolveSessionPath(path)
	if err != nil {
		return err
	}
	if m.persist && m.flushed && resolved == m.sessionFile {
		return nil
	}
	if !m.persist {
		return m.setVolatileSessionFile(resolved)
	}
	next, err := OpenSessionManagerWithOptions(resolved, m.sessionDir, m.cwd, ManagerOptions{Now: m.runtime.now, NewEntryID: m.runtime.newEntryID})
	if err != nil {
		return err
	}
	old := m.store
	m.cwd, m.sessionDir, m.sessionFile = next.cwd, next.sessionDir, next.sessionFile
	m.persist, m.flushed, m.runtime, m.store = next.persist, next.flushed, next.runtime, next.store
	return old.Close()
}

func (m *SessionManager) setVolatileSessionFile(path string) error {
	info, statErr := os.Stat(path)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("%w: stat %s: %v", ErrStorage, path, statErr)
	}
	var next *Session
	flushed := false
	if statErr == nil && info.Size() != 0 {
		var err error
		next, err = loadVolatileSessionSnapshot(path, m.runtime)
		if err != nil {
			return fmt.Errorf("session file is not a valid pi session: %s: %w", path, err)
		}
		flushed = true
	} else {
		header, err := newManagerHeader(m.cwd, NewSessionOptions{}, m.runtime)
		if err != nil {
			return err
		}
		next = newVolatileSession(header, nil, m.runtime)
		flushed = statErr == nil
	}
	old := m.store
	m.store = next
	m.sessionFile = path
	m.flushed = flushed
	return old.Close()
}

func (m *SessionManager) NewSession(options NewSessionOptions) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.startNew(options); err != nil {
		return "", false, err
	}
	return m.sessionFile, m.sessionFile != "", nil
}

func (m *SessionManager) startNew(options NewSessionOptions) error {
	header, err := newManagerHeader(m.cwd, options, m.runtime)
	if err != nil {
		return err
	}
	next := newVolatileSession(header, nil, m.runtime)
	if m.store != nil {
		_ = m.store.Close()
	}
	m.store = next
	m.flushed = false
	if m.persist {
		m.sessionFile = filepath.Join(m.sessionDir, managerSessionFilename(header.Timestamp(), header.ID()))
	} else {
		m.sessionFile = ""
	}
	return nil
}

func managerSessionFilename(timestamp time.Time, id string) string {
	return strings.NewReplacer(":", "-", ".", "-").Replace(formatISOTime(timestamp)) + "_" + id + ".jsonl"
}

func (m *SessionManager) appendAndFlush(ctx context.Context, appendFn func(*Session) (Entry, error)) (Entry, error) {
	entry, err := appendFn(m.store)
	if err != nil {
		return Entry{}, err
	}
	if !m.persist || m.flushed || !managerHasAssistant(m.store.Entries()) {
		return entry, nil
	}
	durable, err := createSessionSnapshot(m.sessionFile, m.store.Header(), m.store.Entries(), m.runtime)
	if err != nil {
		return Entry{}, err
	}
	old := m.store
	m.store = durable
	m.flushed = true
	_ = old.Close()
	return entry, nil
}

func managerHasAssistant(entries []Entry) bool {
	for _, entry := range entries {
		if role, ok := entry.MessageRole(); ok && entry.Type() == "message" && role == string(agentmsg.RoleAssistant) {
			return true
		}
	}
	return false
}

func (m *SessionManager) AppendMessage(ctx context.Context, message agentmsg.Message) (Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.appendAndFlush(ctx, func(s *Session) (Entry, error) { return s.AppendAgentMessage(ctx, message, AppendOptions{}) })
}

func (m *SessionManager) AppendLLMMessage(ctx context.Context, message llm.ConversationMessage) (Entry, error) {
	wrapped, err := agentmsg.NewLLM(message)
	if err != nil {
		return Entry{}, err
	}
	return m.AppendMessage(ctx, wrapped)
}

func (m *SessionManager) AppendThinkingLevelChange(ctx context.Context, level string) (Entry, error) {
	return m.appendPayload(ctx, ThinkingLevelChangePayload{ThinkingLevel: level})
}

func (m *SessionManager) AppendModelChange(ctx context.Context, provider, modelID string) (Entry, error) {
	return m.appendPayload(ctx, ModelChangePayload{Provider: provider, ModelID: modelID})
}

// AppendModelControlChange publishes the model change and its optional
// re-clamped thinking level as one linear storage append.
func (m *SessionManager) AppendModelControlChange(ctx context.Context, provider, modelID string, thinkingLevel *string) ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// If an earlier assistant left this manager in its delayed volatile form,
	// publish that existing snapshot before adding the coupled control records.
	// A snapshot failure therefore cannot leave either new control entry in the
	// manager while the caller rolls settings back.
	if m.persist && !m.flushed && managerHasAssistant(m.store.Entries()) {
		durable, err := createSessionSnapshot(m.sessionFile, m.store.Header(), m.store.Entries(), m.runtime)
		if err != nil {
			return nil, err
		}
		old := m.store
		m.store = durable
		m.flushed = true
		_ = old.Close()
	}
	payloads := []EntryPayload{ModelChangePayload{Provider: provider, ModelID: modelID}}
	if thinkingLevel != nil {
		payloads = append(payloads, ThinkingLevelChangePayload{ThinkingLevel: *thinkingLevel})
	}
	entries, err := m.store.AppendPayloads(ctx, payloads)
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func (m *SessionManager) AppendCompaction(ctx context.Context, summary, firstKeptEntryID string, tokensBefore uint64, details json.RawMessage, fromHook *bool, usage *CompactionUsage) (Entry, error) {
	value, present := optionalManagerBool(fromHook)
	return m.appendPayload(ctx, CompactionPayload{Record: CompactionRecord{Summary: summary, FirstKeptEntryID: firstKeptEntryID, TokensBefore: tokensBefore, Usage: usage}, Details: details, FromHook: value, HasFromHook: present})
}

func (m *SessionManager) AppendCustomEntry(ctx context.Context, customType string, data json.RawMessage) (Entry, error) {
	return m.appendPayload(ctx, CustomPayload{CustomType: customType, Data: data})
}

func (m *SessionManager) AppendCustomMessage(ctx context.Context, message agentmsg.Custom) (Entry, error) {
	return m.appendPayload(ctx, CustomMessagePayload{Message: message})
}

func (m *SessionManager) AppendSessionInfo(ctx context.Context, name string) (Entry, error) {
	sanitized := strings.TrimSpace(managerLineBreakPattern.ReplaceAllString(name, " "))
	return m.appendPayload(ctx, SessionInfoPayload{Name: &sanitized})
}

func (m *SessionManager) AppendLabelChange(ctx context.Context, targetID string, label *string) (Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.store.Entry(targetID); !ok {
		return Entry{}, fmt.Errorf("%w: %s", ErrEntryNotFound, targetID)
	}
	return m.appendAndFlush(ctx, func(s *Session) (Entry, error) {
		return s.AppendPayload(ctx, LabelPayload{TargetID: targetID, Label: label})
	})
}

func (m *SessionManager) appendPayload(ctx context.Context, payload EntryPayload) (Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.appendAndFlush(ctx, func(s *Session) (Entry, error) { return s.AppendPayload(ctx, payload) })
}

func (m *SessionManager) SessionName() (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	entries := m.store.Entries()
	for index := len(entries) - 1; index >= 0; index-- {
		if value, ok := entries[index].Payload().(SessionInfoPayload); ok {
			if value.Name == nil || strings.TrimSpace(*value.Name) == "" {
				return "", false
			}
			return strings.TrimSpace(*value.Name), true
		}
	}
	return "", false
}

func (m *SessionManager) LeafID() (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.store.LeafID()
}
func (m *SessionManager) LeafEntry() (Entry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.store.LeafEntry()
}
func (m *SessionManager) Entry(id string) (Entry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.store.Entry(id)
}
func (m *SessionManager) Children(parentID string) []Entry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	entries := m.store.Entries()
	children := make([]Entry, 0)
	for _, entry := range entries {
		if parent, ok := entry.ParentID(); ok && parent == parentID {
			children = append(children, entry)
		}
	}
	return children
}

func (m *SessionManager) Label(id string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result string
	found := false
	for _, entry := range m.store.Entries() {
		value, ok := entry.Payload().(LabelPayload)
		if !ok || value.TargetID != id {
			continue
		}
		if value.Label == nil || *value.Label == "" {
			result, found = "", false
		} else {
			result, found = *value.Label, true
		}
	}
	return result, found
}

func (m *SessionManager) Branch(fromID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.store.SelectLeaf(fromID)
}
func (m *SessionManager) ResetLeaf() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.store.ResetLeaf()
}

// NavigateTreePosition selects an existing entry (or the virtual root) and,
// when requested, appends the label as a child of that selected position in
// the same durable operation. This matches navigateTree's post-navigation
// label placement without exposing Session's append-parent mechanics.
func (m *SessionManager) NavigateTreePosition(ctx context.Context, newLeafID *string, labelTargetID string, label *string) (*Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if label != nil && *label != "" {
		if _, ok := m.store.Entry(labelTargetID); !ok {
			return nil, fmt.Errorf("%w: %s", ErrEntryNotFound, labelTargetID)
		}
		entry, err := m.appendAndFlush(ctx, func(s *Session) (Entry, error) {
			return s.AppendPayloadAt(ctx, newLeafID, LabelPayload{TargetID: labelTargetID, Label: label})
		})
		if err != nil {
			return nil, err
		}
		return &entry, nil
	}
	if newLeafID == nil {
		return nil, m.store.ResetLeaf()
	}
	return nil, m.store.SelectLeaf(*newLeafID)
}
func (m *SessionManager) BranchPath(fromID string) ([]Entry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if fromID == "" {
		return m.store.BranchPath(), nil
	}
	return m.store.PathTo(fromID)
}
func (m *SessionManager) ContextEntries() []Entry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return managerContextEntries(m.store.BranchPath())
}
func (m *SessionManager) BuildContext() Context {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.store.BuildContext()
}

// ProjectContextAt exposes one arbitrary leaf's compaction-aware semantic
// context without moving the manager's append position.
func (m *SessionManager) ProjectContextAt(leafID string) (ContextProjection, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.store.projectContextAt(leafID)
}
func (m *SessionManager) Tree() []TreeNode {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.store.Tree()
}

// PrepareCompaction captures the immutable selected-branch input consumed by
// AgentSession's summarization orchestration. It never calls a provider or an
// extension hook.
func (m *SessionManager) PrepareCompaction(ctx context.Context, keepRecentTokens uint64, instructions string) (SummaryInput, error) {
	return m.PrepareCompactionWithOptions(ctx, PrepareCompactionOptions{KeepRecentTokens: keepRecentTokens, Instructions: instructions})
}

// PrepareCompactionWithOptions preserves an explicitly configured zero keep
// budget while keeping PrepareCompaction source-compatible for low-level users.
func (m *SessionManager) PrepareCompactionWithOptions(ctx context.Context, options PrepareCompactionOptions) (SummaryInput, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.store.compactionSnapshot(ctx, CompactRequest{
		KeepRecentTokens: options.KeepRecentTokens, KeepRecentTokensSet: options.KeepRecentTokensSet,
		ReserveTokens: options.ReserveTokens, ReserveTokensSet: options.ReserveTokensSet, Instructions: options.Instructions,
		Enabled: options.Enabled, EnabledSet: options.EnabledSet,
	})
}

// CommitCompaction validates and persists one real summary result. Summary
// selection, provider execution, retry, hooks, and events remain owned by
// AgentSession, as in the original architecture.
func (m *SessionManager) CommitCompaction(ctx context.Context, input SummaryInput, output SummaryOutput) (CompactResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	if cause := context.Cause(ctx); cause != nil {
		return CompactResult{}, fmt.Errorf("%w: %w", ErrAppendCanceled, cause)
	}
	var err error
	output, err = finalizeCompactionOutput(input, output)
	if err != nil {
		return CompactResult{}, fmt.Errorf("%w: %w", ErrSummaryFailed, err)
	}
	if err := validateSummaryOutput(output); err != nil {
		return CompactResult{}, err
	}
	entry, err := m.store.commitCompaction(ctx, input, output)
	if err != nil {
		return CompactResult{}, err
	}
	var estimatedTokensAfter uint64
	if estimate, estimateErr := EstimateAgentMessagesTokens(m.store.BuildContext().AgentMessages()); estimateErr == nil {
		estimatedTokensAfter = estimate
	}
	return CompactResult{Entry: entry, Input: input, Output: cloneSummaryOutput(output), EstimatedTokensAfter: estimatedTokensAfter, Committed: true}, nil
}

// LatestCompactionEntry mirrors pi's standalone helper. Callers choose whether
// to search the selected branch or another explicit entry slice.
func LatestCompactionEntry(entries []Entry) (Entry, bool) {
	for index := len(entries) - 1; index >= 0; index-- {
		if _, ok := entries[index].Compaction(); ok {
			return entries[index].clone(), true
		}
	}
	return Entry{}, false
}

func managerContextEntries(path []Entry) []Entry {
	indexes := make([]int, len(path))
	for index := range indexes {
		indexes[index] = index
	}
	projected := contextProjectionIndexes(indexes, path)
	result := make([]Entry, len(projected))
	for index, entryIndex := range projected {
		// path already consists of immutable snapshots (BranchPath/PathTo or
		// projectContextAt's explicit clone). Avoid duplicating every retained
		// raw JSON record a second time for the semantic projection.
		result[index] = path[entryIndex]
	}
	return result
}

// BranchWithSummary consumes an actual summary produced by AgentSession or an
// extension hook. SessionManager never fabricates model output.
func (m *SessionManager) BranchWithSummary(ctx context.Context, branchFromID *string, summary string, details json.RawMessage, fromHook *bool, usage *CompactionUsage) (Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fromID := "root"
	if branchFromID != nil {
		fromID = *branchFromID
	}
	value, present := optionalManagerBool(fromHook)
	return m.appendAndFlush(ctx, func(s *Session) (Entry, error) {
		return s.AppendPayloadAt(ctx, branchFromID, BranchSummaryPayload{FromID: fromID, Summary: summary, Details: details, Usage: usage, FromHook: value, HasFromHook: present})
	})
}

func optionalManagerBool(value *bool) (bool, bool) {
	if value == nil {
		return false, false
	}
	return *value, true
}

// CreateBranchedSession replaces the manager with a new session containing
// exactly one root-to-leaf path. Label records are removed from the path,
// retained entries are re-chained, and effective labels are appended after the
// selected leaf exactly as in pi's SessionManager.
func (m *SessionManager) CreateBranchedSession(ctx context.Context, leafID string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next, err := m.cloneBranchedManagerLocked(ctx, leafID)
	if err != nil {
		return "", false, err
	}
	old := m.store
	m.cwd, m.sessionDir, m.sessionFile = next.cwd, next.sessionDir, next.sessionFile
	m.persist, m.flushed, m.runtime, m.store = next.persist, next.flushed, next.runtime, next.store
	_ = old.Close()
	return m.sessionFile, m.sessionFile != "", nil
}

// CloneBranchedSession creates an independently owned manager containing the
// selected root-to-leaf path without mutating the source manager. This is the
// Go ownership-safe equivalent of opening the current JSONL a second time
// before createBranchedSession: an open SessionManager already owns the sole
// writer lock for its file.
func (m *SessionManager) CloneBranchedSession(ctx context.Context, leafID string) (*SessionManager, error) {
	if m == nil {
		return nil, fmt.Errorf("%w: nil session manager", ErrInvalidSession)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cloneBranchedManagerLocked(ctx, leafID)
}

func (m *SessionManager) cloneBranchedManagerLocked(ctx context.Context, leafID string) (*SessionManager, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cause := context.Cause(ctx); cause != nil {
		return nil, fmt.Errorf("%w: %v", ErrAppendCanceled, cause)
	}
	for _, diagnostic := range m.store.LoadDiagnostics() {
		if diagnostic.Code == LoadDiagnosticMalformedLine {
			return nil, fmt.Errorf("%w: line %d must be preserved or explicitly repaired before branch extraction", ErrMalformedRecords, diagnostic.Line)
		}
	}
	path, err := m.store.PathTo(leafID)
	if err != nil {
		return nil, err
	}
	resolvedLabels := resolvedManagerLabels(m.store.Entries())
	retained := make([]Entry, 0, len(path))
	parentID := ""
	hasParent := false
	for _, entry := range path {
		if _, label := entry.Payload().(LabelPayload); label {
			continue
		}
		reparented, err := reparentManagerEntry(entry, parentID, hasParent)
		if err != nil {
			return nil, err
		}
		retained = append(retained, reparented)
		parentID, hasParent = reparented.ID(), true
	}
	previousFile := m.sessionFile
	parentSession := ""
	if m.persist {
		parentSession = previousFile
	}
	header, err := newManagerHeader(m.cwd, NewSessionOptions{ParentSession: parentSession}, m.runtime)
	if err != nil {
		return nil, err
	}
	retainedIDs := make(map[string]struct{}, len(retained))
	for _, entry := range retained {
		retainedIDs[entry.ID()] = struct{}{}
	}
	for _, label := range resolvedLabels {
		if _, keep := retainedIDs[label.targetID]; !keep {
			continue
		}
		id, err := nextManagerEntryID(m.runtime, retainedIDs)
		if err != nil {
			return nil, err
		}
		labelCopy := label.label
		raw, err := encodePayloadEntry(id, parentID, hasParent, label.timestamp, LabelPayload{TargetID: label.targetID, Label: &labelCopy})
		if err != nil {
			return nil, err
		}
		labelEntry, err := decodeEntry(raw)
		if err != nil {
			return nil, err
		}
		retained = append(retained, labelEntry)
		retainedIDs[id] = struct{}{}
		parentID, hasParent = id, true
	}
	next := newVolatileSession(header, retained, m.runtime)
	newFile := ""
	if m.persist {
		newFile = filepath.Join(m.sessionDir, managerSessionFilename(header.Timestamp(), header.ID()))
		if managerHasAssistant(retained) {
			durable, err := createSessionSnapshot(newFile, header, retained, m.runtime)
			if err != nil {
				return nil, err
			}
			next = durable
		}
	}
	return &SessionManager{
		cwd: m.cwd, sessionDir: m.sessionDir, sessionFile: newFile,
		persist: m.persist, flushed: m.persist && managerHasAssistant(retained),
		runtime: m.runtime, store: next,
	}, nil
}

type managerLabel struct {
	targetID  string
	label     string
	timestamp time.Time
}

func resolvedManagerLabels(entries []Entry) []managerLabel {
	labels := make(map[string]managerLabel)
	order := make([]string, 0)
	for _, entry := range entries {
		payload, ok := entry.Payload().(LabelPayload)
		if !ok {
			continue
		}
		if payload.Label == nil || *payload.Label == "" {
			delete(labels, payload.TargetID)
			for index, targetID := range order {
				if targetID == payload.TargetID {
					order = append(order[:index], order[index+1:]...)
					break
				}
			}
			continue
		}
		if _, exists := labels[payload.TargetID]; !exists {
			order = append(order, payload.TargetID)
		}
		labels[payload.TargetID] = managerLabel{targetID: payload.TargetID, label: *payload.Label, timestamp: entry.Timestamp()}
	}
	result := make([]managerLabel, 0, len(order))
	for _, targetID := range order {
		result = append(result, labels[targetID])
	}
	return result
}

func reparentManagerEntry(entry Entry, parentID string, hasParent bool) (Entry, error) {
	object := make(map[string]json.RawMessage)
	if err := json.Unmarshal(entry.RawJSON(), &object); err != nil {
		return Entry{}, fmt.Errorf("%w: decode branch entry: %v", ErrInvalidEntry, err)
	}
	if hasParent {
		encoded, _ := json.Marshal(parentID)
		object["parentId"] = encoded
	} else {
		object["parentId"] = json.RawMessage("null")
	}
	raw, err := json.Marshal(object)
	if err != nil {
		return Entry{}, fmt.Errorf("%w: encode branch entry: %v", ErrInvalidEntry, err)
	}
	decoded, _, err := decodeCompatibleEntry(raw)
	return decoded, err
}

func nextManagerEntryID(runtime runtimeConfig, existing map[string]struct{}) (string, error) {
	for attempt := 0; attempt < entryIDAttempts; attempt++ {
		id, err := runtime.newEntryID()
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrIDGeneration, err)
		}
		if _, found := existing[id]; !found {
			return id, nil
		}
	}
	return "", ErrEntryIDExhausted
}

// ForkSessionFrom copies the complete source forest into a new persisted
// session. Unlike CreateBranchedSession it intentionally retains all branches.
func ForkSessionFrom(ctx context.Context, sourcePath, targetCwd, sessionDir string, options NewSessionOptions) (*SessionManager, error) {
	resolvedCwd, err := resolveWorkingDir(targetCwd)
	if err != nil {
		return nil, err
	}
	dir, err := resolveManagerSessionDir(resolvedCwd, sessionDir, true)
	if err != nil {
		return nil, err
	}
	runtime := normalizeRuntime(nil, nil)
	timestamp := canonicalTime(runtime.now())
	id := options.ID
	if id == "" {
		id, err = newSessionID(timestamp)
		if err != nil {
			return nil, err
		}
	}
	if err := ValidateSessionID(id); err != nil {
		return nil, err
	}
	target := filepath.Join(dir, managerSessionFilename(timestamp, id))
	store, err := ForkFrom(ctx, sourcePath, ExtractOptions{TargetPath: target, ID: id, WorkingDir: resolvedCwd, Now: func() time.Time { return timestamp }, NewEntryID: runtime.newEntryID})
	if err != nil {
		return nil, err
	}
	return &SessionManager{cwd: resolvedCwd, sessionDir: dir, sessionFile: target, persist: true, flushed: true, runtime: runtime, store: store}, nil
}

func resolveManagerSessionDir(cwd, sessionDir string, create bool) (string, error) {
	dir := sessionDir
	if dir == "" {
		dir = defaultSessionDirPath(cwd, "")
	}
	resolved, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("%w: resolve session directory: %v", ErrInvalidSession, err)
	}
	resolved = filepath.Clean(resolved)
	if create {
		if err := os.MkdirAll(resolved, 0o755); err != nil {
			return "", fmt.Errorf("%w: create session directory: %v", ErrStorage, err)
		}
	}
	return resolved, nil
}

func defaultSessionDirPath(cwd, agentDir string) string {
	if agentDir == "" {
		agentDir = defaultAgentDir()
	}
	resolvedCwd, _ := filepath.Abs(cwd)
	trimmed := resolvedCwd
	if strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, "\\") {
		trimmed = trimmed[1:]
	}
	replacer := strings.NewReplacer("/", "-", "\\", "-", ":", "-")
	return filepath.Join(agentDir, "sessions", "--"+replacer.Replace(trimmed)+"--")
}

func defaultAgentDir() string {
	if configured := os.Getenv("PI_CODING_AGENT_DIR"); configured != "" {
		if configured == "~" || strings.HasPrefix(configured, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				if configured == "~" {
					configured = home
				} else {
					configured = filepath.Join(home, configured[2:])
				}
			}
		}
		if resolved, err := filepath.Abs(configured); err == nil {
			return filepath.Clean(resolved)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".pi", "agent")
	}
	return filepath.Join(home, ".pi", "agent")
}
