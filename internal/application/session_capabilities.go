package application

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/session"
)

const MaxInlineBashOutputBytes int64 = 5 << 20

var (
	ErrSessionEntryNotFound = errors.New("session entry not found")
	ErrThinkingNotFound     = errors.New("thinking block not found")
	ErrInvalidBashOutput    = errors.New("invalid bash output path")
	ErrBashOutputForbidden  = errors.New("bash output is not referenced by the session")
	ErrBashOutputTooLarge   = errors.New("bash output is too large for inline display")
)

type GeneratedSessionTitle struct {
	Title string
	Usage agent.SessionTitleUsage
}

type SessionExport struct {
	FileName string
	HTML     []byte
}

type BashOutput struct {
	Reader io.ReadCloser
	Size   int64
}

func (s *Service) GenerateSessionTitle(ctx context.Context, id string) (GeneratedSessionTitle, error) {
	ctx = normalizeContext(ctx)
	managed, err := s.open(ctx, id)
	if err != nil {
		return GeneratedSessionTitle{}, err
	}
	runtime := managed.session.Runtime()
	if runtime == nil || runtime.Session() == nil {
		return GeneratedSessionTitle{}, errors.New("active agent session is unavailable")
	}
	generated, err := runtime.Session().GenerateSessionTitle(ctx)
	if err != nil {
		return GeneratedSessionTitle{}, err
	}
	if err := runtime.Session().SetSessionName(ctx, generated.Title); err != nil {
		return GeneratedSessionTitle{}, err
	}
	s.events.publish(Event{SessionID: id, Value: SessionCatalogEvent{Change: SessionUpdated}})
	managed.touch()
	return GeneratedSessionTitle{Title: generated.Title, Usage: generated.Usage}, nil
}

func (s *Service) SessionThinking(ctx context.Context, id, entryID string, blockIndex int) (string, error) {
	if cause := context.Cause(normalizeContext(ctx)); cause != nil {
		return "", cause
	}
	if blockIndex < 0 {
		return "", ErrThinkingNotFound
	}
	manager, _, _, closeManager, err := s.sessionManagerForRead(id)
	if err != nil {
		return "", err
	}
	if closeManager {
		defer manager.Close()
	}
	entry, ok := manager.Entry(entryID)
	if !ok {
		return "", ErrSessionEntryNotFound
	}
	message, ok := entry.AgentMessage()
	if !ok {
		return "", ErrSessionEntryNotFound
	}
	wrapper, ok := message.(agentmsg.LLM)
	if !ok {
		return "", ErrSessionEntryNotFound
	}
	assistant, ok := wrapper.Conversation().(llm.AssistantTerminal)
	if !ok {
		return "", ErrSessionEntryNotFound
	}
	blocks := assistant.Blocks()
	if blockIndex >= len(blocks) {
		return "", ErrThinkingNotFound
	}
	thinking, ok := blocks[blockIndex].(llm.ThinkingBlock)
	if !ok {
		return "", ErrThinkingNotFound
	}
	return thinking.Thinking(), nil
}

func (s *Service) DeleteSession(ctx context.Context, id string) error {
	if s == nil {
		return errors.New("application service is unavailable")
	}
	ctx = normalizeContext(ctx)
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()

	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("session id is required")
	}
	discovered, err := session.ListAllSessionsInAgentDir(s.paths.AgentDir, nil)
	if err != nil {
		return err
	}
	var target session.SessionInfo
	found := false
	for _, info := range discovered {
		if info.ID == id {
			target, found = info, true
			break
		}
	}
	if !found {
		if managed, ok := s.active(id); ok {
			manager := managed.manager()
			if manager != nil {
				target = session.SessionInfo{ID: id, Cwd: manager.Cwd()}
				target.Path, _ = manager.SessionFile()
				target.ParentSessionPath, target.HasParentSession = manager.Header().ParentSession()
				found = true
			}
		}
	}
	if !found {
		return os.ErrNotExist
	}
	targetPath := cleanPathKey(target.Path)
	type childSession struct {
		id   string
		path string
	}
	children := make([]childSession, 0)
	for _, info := range discovered {
		if info.ID == id || !info.HasParentSession || targetPath == "" {
			continue
		}
		if cleanPathKey(info.ParentSessionPath) == targetPath {
			children = append(children, childSession{id: info.ID, path: info.Path})
		}
	}

	ids := map[string]struct{}{id: {}}
	for _, child := range children {
		ids[child.id] = struct{}{}
	}
	managed := s.detachSessions(ids)
	var disposeErr error
	for _, value := range managed {
		disposeErr = errors.Join(disposeErr, value.dispose(ctx))
	}
	if disposeErr != nil {
		return fmt.Errorf("close sessions before deletion: %w", disposeErr)
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}

	parentPath := ""
	if target.HasParentSession {
		parentPath = target.ParentSessionPath
	}
	rewritten := make([]string, 0, len(children))
	for _, child := range children {
		if err := session.RewriteParentSession(ctx, child.path, parentPath); err != nil {
			rollbackErr := rollbackSessionParents(rewritten, target.Path)
			return errors.Join(fmt.Errorf("reparent child session %s: %w", child.id, err), rollbackErr)
		}
		rewritten = append(rewritten, child.path)
	}
	if target.Path != "" {
		if err := os.Remove(target.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			rollbackErr := rollbackSessionParents(rewritten, target.Path)
			return errors.Join(fmt.Errorf("delete session file: %w", err), rollbackErr)
		}
	}
	s.events.publish(Event{SessionID: id, Value: SessionCatalogEvent{Change: SessionDeleted}})
	return nil
}

func rollbackSessionParents(paths []string, parentPath string) error {
	var result error
	for index := len(paths) - 1; index >= 0; index-- {
		if err := session.RewriteParentSession(context.Background(), paths[index], parentPath); err != nil {
			result = errors.Join(result, fmt.Errorf("rollback child parent %s: %w", paths[index], err))
		}
	}
	return result
}

func (s *Service) detachSessions(ids map[string]struct{}) []*managedSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]*managedSession, 0, len(ids))
	for id := range ids {
		managed := s.sessions[id]
		if managed == nil || !managed.closing.CompareAndSwap(false, true) {
			continue
		}
		delete(s.sessions, id)
		result = append(result, managed)
	}
	return result
}

var bashOutputName = regexp.MustCompile(`^pi-bash-[A-Za-z0-9_-]+\.log$`)

func (s *Service) OpenBashOutput(ctx context.Context, id, path string) (BashOutput, error) {
	ctx = normalizeContext(ctx)
	if cause := context.Cause(ctx); cause != nil {
		return BashOutput{}, cause
	}
	root := s.production.BashArtifactDirectory
	if root == "" {
		root = os.TempDir()
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return BashOutput{}, ErrInvalidBashOutput
	}
	resolved, err := filepath.Abs(path)
	if err != nil {
		return BashOutput{}, ErrInvalidBashOutput
	}
	resolved = filepath.Clean(resolved)
	if filepath.Dir(resolved) != filepath.Clean(root) || !bashOutputName.MatchString(filepath.Base(resolved)) {
		return BashOutput{}, ErrInvalidBashOutput
	}
	manager, _, _, closeManager, err := s.sessionManagerForRead(id)
	if err != nil {
		return BashOutput{}, err
	}
	if closeManager {
		defer manager.Close()
	}
	referenced := false
	for _, entry := range manager.Entries() {
		message, ok := entry.AgentMessage()
		if !ok {
			continue
		}
		bash, ok := message.(agentmsg.BashExecution)
		if ok && bash.FullOutputPath != "" && cleanPathKey(bash.FullOutputPath) == cleanPathKey(resolved) {
			referenced = true
			break
		}
	}
	if !referenced {
		return BashOutput{}, ErrBashOutputForbidden
	}
	pathInfo, err := os.Lstat(resolved)
	if err != nil || !pathInfo.Mode().IsRegular() || pathInfo.Mode()&os.ModeSymlink != 0 {
		return BashOutput{}, os.ErrNotExist
	}
	file, err := os.Open(resolved)
	if err != nil {
		return BashOutput{}, os.ErrNotExist
	}
	fileInfo, err := file.Stat()
	if err != nil || !fileInfo.Mode().IsRegular() || !os.SameFile(pathInfo, fileInfo) {
		_ = file.Close()
		return BashOutput{}, os.ErrNotExist
	}
	return BashOutput{Reader: file, Size: fileInfo.Size()}, nil
}
