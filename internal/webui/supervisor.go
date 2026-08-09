package webui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/host"
	"github.com/cat3399/pi-go/internal/provider"
	agentruntime "github.com/cat3399/pi-go/internal/runtime"
	"github.com/cat3399/pi-go/internal/session"
)

const defaultSessionIdleTimeout = 10 * time.Minute

type runtimeOpener func(context.Context, app.ProductionConfig, app.ProductionRuntimeOptions) (*agentruntime.Runtime, error)

type supervisorOptions struct {
	Context       context.Context
	Production    app.ProductionConfig
	IdleTimeout   time.Duration
	OpenRuntime   runtimeOpener
	DisableReaper bool
}

type NewSessionOptions struct {
	CWD           string
	Provider      string
	ModelID       string
	ToolNames     []string
	HasToolNames  bool
	ThinkingLevel *provider.ThinkingLevel
}

type managedSession struct {
	host *host.Host

	identityMu sync.RWMutex
	id         string
	cwd        string
	file       string

	lastAccess  atomic.Int64
	subscribers atomic.Int64
	closing     atomic.Bool
}

func (s *managedSession) identity() (id, cwd, file string) {
	if s == nil {
		return "", "", ""
	}
	s.identityMu.RLock()
	defer s.identityMu.RUnlock()
	return s.id, s.cwd, s.file
}

func (s *managedSession) updateIdentity(state host.State) (previous, current string) {
	s.identityMu.Lock()
	defer s.identityMu.Unlock()
	previous = s.id
	s.id, s.cwd = state.SessionID, state.CWD
	if state.SessionFile == nil {
		s.file = ""
	} else {
		s.file = *state.SessionFile
	}
	return previous, s.id
}

func (s *managedSession) touch() {
	if s != nil {
		s.lastAccess.Store(time.Now().UnixNano())
	}
}

func (s *managedSession) manager() *session.SessionManager {
	if s == nil || s.host == nil || s.host.Runtime() == nil || s.host.Runtime().Session() == nil {
		return nil
	}
	return s.host.Runtime().Session().SessionManager()
}

func (s *managedSession) busy() bool {
	if s == nil || s.host == nil {
		return false
	}
	state, err := s.host.State()
	return err == nil && (state.IsPromptRunning || state.IsStreaming || state.IsBashRunning || state.IsCompacting)
}

type openCall struct {
	done    chan struct{}
	session *managedSession
	err     error
}

// Supervisor owns the process-local set of independent Host/Runtime handles.
// It coordinates lifecycle only: every conversation remains authoritative in
// Runtime -> AgentSession -> Agent and its SessionManager.
type Supervisor struct {
	ctx         context.Context
	cancel      context.CancelFunc
	production  app.ProductionConfig
	paths       app.ProductionPaths
	idle        time.Duration
	openRuntime runtimeOpener

	mu         sync.Mutex
	sessions   map[string]*managedSession
	opening    map[string]*openCall
	closed     bool
	reaperDone chan struct{}
}

func newSupervisor(options supervisorOptions) (*Supervisor, error) {
	ctx := options.Context
	if ctx == nil {
		ctx = context.Background()
	}
	paths, err := app.ResolveProductionPaths(options.Production)
	if err != nil {
		return nil, fmt.Errorf("resolve WebUI production paths: %w", err)
	}
	idle := options.IdleTimeout
	if idle <= 0 {
		idle = defaultSessionIdleTimeout
	}
	opener := options.OpenRuntime
	if opener == nil {
		opener = app.OpenProductionRuntime
	}
	supervisorCtx, cancel := context.WithCancel(ctx)
	s := &Supervisor{
		ctx: supervisorCtx, cancel: cancel, production: cloneProductionConfig(options.Production),
		paths: paths, idle: idle, openRuntime: opener,
		sessions: make(map[string]*managedSession), opening: make(map[string]*openCall),
		reaperDone: make(chan struct{}),
	}
	if options.DisableReaper {
		close(s.reaperDone)
	} else {
		go s.reapIdleSessions()
	}
	return s, nil
}

func cloneProductionConfig(config app.ProductionConfig) app.ProductionConfig {
	config.Environment = append([]string(nil), config.Environment...)
	return config
}

func normalizeSupervisorContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func (s *Supervisor) AgentDir() string {
	if s == nil {
		return ""
	}
	return s.paths.AgentDir
}

func (s *Supervisor) DefaultCWD() string {
	if s == nil {
		return ""
	}
	return s.paths.WorkingDir
}

func validateCWD(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("cwd is required")
	}
	resolved, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve cwd: %w", err)
	}
	resolved = filepath.Clean(resolved)
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("directory does not exist: %s", value)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("path is not a directory: %s", value)
	}
	return resolved, nil
}

func (s *Supervisor) NewSession(ctx context.Context, options NewSessionOptions) (*managedSession, host.State, error) {
	if s == nil {
		return nil, host.State{}, errors.New("WebUI supervisor is unavailable")
	}
	ctx = normalizeSupervisorContext(ctx)
	cwd, err := validateCWD(options.CWD)
	if err != nil {
		return nil, host.State{}, err
	}
	if (options.Provider == "") != (options.ModelID == "") {
		return nil, host.State{}, errors.New("provider and modelId must be provided together")
	}
	config := cloneProductionConfig(s.production)
	config.WorkingDir = cwd
	config.AgentDir = s.paths.AgentDir
	// A long-lived surface may create many sessions. A process-level explicit ID
	// cannot safely name all of them; production generates a fresh pi ID per open.
	config.SessionID = ""
	runtime, err := s.openRuntime(ctx, config, app.ProductionRuntimeOptions{
		ProviderID: options.Provider,
		ModelID:    options.ModelID,
	})
	if err != nil {
		return nil, host.State{}, err
	}
	managed, err := s.adoptRuntime(ctx, runtime)
	if err != nil {
		return nil, host.State{}, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = managed.host.Dispose(context.Background())
		}
	}()
	if options.HasToolNames {
		if _, err := managed.host.Dispatch(ctx, host.SetToolsCommand{ToolNames: append([]string(nil), options.ToolNames...)}); err != nil {
			return nil, host.State{}, fmt.Errorf("set initial tools: %w", err)
		}
	}
	if options.ThinkingLevel != nil {
		if _, err := managed.host.Dispatch(ctx, host.SetThinkingLevelCommand{Level: *options.ThinkingLevel}); err != nil {
			return nil, host.State{}, fmt.Errorf("set initial thinking level: %w", err)
		}
	}
	state, err := managed.host.State()
	if err != nil {
		return nil, host.State{}, err
	}
	if err := s.register(managed); err != nil {
		return nil, host.State{}, err
	}
	managed.touch()
	cleanup = false
	return managed, state, nil
}

func (s *Supervisor) adoptRuntime(_ context.Context, runtime *agentruntime.Runtime) (*managedSession, error) {
	if runtime == nil {
		return nil, errors.New("production runtime is unavailable")
	}
	for _, diagnostic := range runtime.Diagnostics() {
		if diagnostic.Kind == agentruntime.DiagnosticError {
			_ = runtime.Dispose(context.Background())
			return nil, errors.New(diagnostic.Message)
		}
	}
	// The Host outlives the HTTP request that caused it to open. Bind it to the
	// surface lifetime so returning a POST response cannot cancel a live agent.
	agentHost, err := host.New(s.ctx, runtime)
	if err != nil {
		_ = runtime.Dispose(context.Background())
		return nil, err
	}
	state, err := agentHost.State()
	if err != nil {
		_ = agentHost.Dispose(context.Background())
		return nil, err
	}
	file := ""
	if state.SessionFile != nil {
		file = *state.SessionFile
	}
	managed := &managedSession{host: agentHost, id: state.SessionID, cwd: state.CWD, file: file}
	managed.touch()
	return managed, nil
}

func (s *Supervisor) register(managed *managedSession) error {
	id, _, _ := managed.identity()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("WebUI supervisor is closed")
	}
	if existing := s.sessions[id]; existing != nil && existing != managed {
		return fmt.Errorf("session %s is already active", id)
	}
	s.sessions[id] = managed
	return nil
}

func (s *Supervisor) Active(id string) (*managedSession, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	managed := s.sessions[id]
	s.mu.Unlock()
	if managed == nil || managed.closing.Load() {
		return nil, false
	}
	managed.touch()
	return managed, true
}

func (s *Supervisor) Open(ctx context.Context, id string) (*managedSession, error) {
	if s == nil {
		return nil, errors.New("WebUI supervisor is unavailable")
	}
	ctx = normalizeSupervisorContext(ctx)
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("session id is required")
	}
	if managed, ok := s.Active(id); ok {
		return managed, nil
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("WebUI supervisor is closed")
	}
	if call := s.opening[id]; call != nil {
		s.mu.Unlock()
		select {
		case <-call.done:
			return call.session, call.err
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
	call := &openCall{done: make(chan struct{})}
	s.opening[id] = call
	s.mu.Unlock()

	managed, err := s.openExisting(ctx, id)
	var redundant *managedSession
	s.mu.Lock()
	delete(s.opening, id)
	if err == nil {
		if existing := s.sessions[id]; existing != nil {
			redundant = managed
			managed = existing
		} else {
			s.sessions[id] = managed
		}
	}
	call.session, call.err = managed, err
	close(call.done)
	s.mu.Unlock()
	if redundant != nil {
		_ = redundant.host.Dispose(context.Background())
	}
	return managed, err
}

func (s *Supervisor) openExisting(ctx context.Context, id string) (*managedSession, error) {
	info, found, err := s.findSession(id)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, os.ErrNotExist
	}
	config := cloneProductionConfig(s.production)
	config.WorkingDir = info.Cwd
	config.AgentDir = s.paths.AgentDir
	config.SessionID = ""
	runtime, err := s.openRuntime(ctx, config, app.ProductionRuntimeOptions{SessionPath: info.Path})
	if err != nil {
		return nil, err
	}
	managed, err := s.adoptRuntime(ctx, runtime)
	if err != nil {
		return nil, err
	}
	managedID, _, _ := managed.identity()
	if managedID != id {
		_ = managed.host.Dispose(context.Background())
		return nil, fmt.Errorf("opened session id %q does not match %q", managedID, id)
	}
	return managed, nil
}

func (s *Supervisor) findSession(id string) (session.SessionInfo, bool, error) {
	values, err := session.ListAllSessionsInAgentDir(s.paths.AgentDir, nil)
	if err != nil {
		return session.SessionInfo{}, false, err
	}
	for _, value := range values {
		if value.ID == id {
			return value, true, nil
		}
	}
	return session.SessionInfo{}, false, nil
}

func (s *Supervisor) Dispatch(ctx context.Context, id string, command host.Command) (host.CommandResult, error) {
	ctx = normalizeSupervisorContext(ctx)
	managed, err := s.Open(ctx, id)
	if err != nil {
		return nil, err
	}
	managed.touch()
	result, dispatchErr := managed.host.Dispatch(ctx, command)
	identityErr := s.reconcileIdentity(managed)
	if dispatchErr != nil {
		return nil, dispatchErr
	}
	if identityErr != nil {
		return nil, identityErr
	}
	return result, nil
}

func (s *Supervisor) reconcileIdentity(managed *managedSession) error {
	if managed == nil || managed.host == nil {
		return errors.New("managed session is unavailable")
	}
	state, err := managed.host.State()
	if err != nil {
		return err
	}
	oldID, _, _ := managed.identity()
	if state.SessionID == oldID {
		managed.updateIdentity(state)
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.sessions[state.SessionID]; existing != nil && existing != managed {
		return fmt.Errorf("session identity changed to already-active session %s", state.SessionID)
	}
	if s.sessions[oldID] == managed {
		delete(s.sessions, oldID)
	}
	managed.updateIdentity(state)
	s.sessions[state.SessionID] = managed
	return nil
}

func (s *Supervisor) State(ctx context.Context, id string, open bool) (host.State, bool, error) {
	ctx = normalizeSupervisorContext(ctx)
	managed, ok := s.Active(id)
	if !ok && open {
		var err error
		managed, err = s.Open(ctx, id)
		if err != nil {
			return host.State{}, false, err
		}
		ok = true
	}
	if !ok {
		return host.State{}, false, nil
	}
	state, err := managed.host.State()
	return state, true, err
}

func (s *Supervisor) Subscribe(ctx context.Context, id string, observer host.Observer) (func(), error) {
	ctx = normalizeSupervisorContext(ctx)
	managed, err := s.Open(ctx, id)
	if err != nil {
		return nil, err
	}
	managed.touch()
	managed.subscribers.Add(1)
	unsubscribeHost := managed.host.Subscribe(observer)
	var once sync.Once
	return func() {
		once.Do(func() {
			unsubscribeHost()
			managed.subscribers.Add(-1)
			managed.touch()
		})
	}, nil
}

func (s *Supervisor) RunningIDs() []string {
	if s == nil {
		return []string{}
	}
	s.mu.Lock()
	values := make([]*managedSession, 0, len(s.sessions))
	for _, managed := range s.sessions {
		values = append(values, managed)
	}
	s.mu.Unlock()
	result := make([]string, 0, len(values))
	for _, managed := range values {
		if managed.busy() {
			id, _, _ := managed.identity()
			result = append(result, id)
		}
	}
	sort.Strings(result)
	return result
}

func (s *Supervisor) ActiveSessions() []*managedSession {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	values := make([]*managedSession, 0, len(s.sessions))
	for _, managed := range s.sessions {
		if !managed.closing.Load() {
			values = append(values, managed)
		}
	}
	s.mu.Unlock()
	return values
}

func (s *Supervisor) reapIdleSessions() {
	defer close(s.reaperDone)
	interval := s.idle / 2
	if interval > time.Minute {
		interval = time.Minute
	}
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.reapOnce(time.Now())
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *Supervisor) reapOnce(now time.Time) {
	type candidate struct {
		id      string
		managed *managedSession
	}
	candidates := make([]candidate, 0)
	s.mu.Lock()
	for id, managed := range s.sessions {
		last := time.Unix(0, managed.lastAccess.Load())
		if managed.subscribers.Load() != 0 || now.Sub(last) < s.idle || managed.closing.Load() {
			continue
		}
		candidates = append(candidates, candidate{id: id, managed: managed})
	}
	s.mu.Unlock()

	var expired []*managedSession
	for _, candidate := range candidates {
		if candidate.managed.busy() {
			candidate.managed.touch()
			continue
		}
		s.mu.Lock()
		last := time.Unix(0, candidate.managed.lastAccess.Load())
		if s.sessions[candidate.id] == candidate.managed &&
			candidate.managed.subscribers.Load() == 0 &&
			now.Sub(last) >= s.idle &&
			candidate.managed.closing.CompareAndSwap(false, true) {
			delete(s.sessions, candidate.id)
			expired = append(expired, candidate.managed)
		}
		s.mu.Unlock()
	}
	for _, managed := range expired {
		_ = managed.host.Dispose(context.Background())
	}
}

func (s *Supervisor) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.cancel()
	values := make([]*managedSession, 0, len(s.sessions))
	for id, managed := range s.sessions {
		delete(s.sessions, id)
		managed.closing.Store(true)
		values = append(values, managed)
	}
	s.mu.Unlock()
	var closeErr error
	select {
	case <-s.reaperDone:
	case <-ctx.Done():
		closeErr = errors.Join(closeErr, context.Cause(ctx))
	}
	for _, managed := range values {
		if err := managed.host.Dispose(ctx); err != nil {
			closeErr = errors.Join(closeErr, err)
		}
	}
	return closeErr
}
