// Package application owns the process-local API shared by every product
// surface. It coordinates ApplicationSession and Runtime lifecycles without
// owning a second copy of Agent or Session product state.
package application

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/provider"
	agentruntime "github.com/cat3399/pi-go/internal/runtime"
	"github.com/cat3399/pi-go/internal/session"
)

const defaultSessionIdleTimeout = 10 * time.Minute

type RuntimeOpener func(context.Context, app.ProductionConfig, app.ProductionRuntimeOptions) (*agentruntime.Runtime, error)

type ServiceOptions struct {
	Context         context.Context
	Production      app.ProductionConfig
	IdleTimeout     time.Duration
	OpenRuntime     RuntimeOpener
	DisableReaper   bool
	SkillHTTP       HTTPDoer
	ModelHTTP       HTTPDoer
	ModelCatalogURL string
	SkillsAPIURL    string
	GitHubAPIURL    string
}

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
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
	session          *ApplicationSession
	eventUnsubscribe func()

	identityMu sync.RWMutex
	id         string
	cwd        string
	file       string

	lastAccess atomic.Int64
	closing    atomic.Bool
}

func (s *managedSession) identity() (id, cwd, file string) {
	if s == nil {
		return "", "", ""
	}
	s.identityMu.RLock()
	defer s.identityMu.RUnlock()
	return s.id, s.cwd, s.file
}

func (s *managedSession) updateIdentity(state State) {
	s.identityMu.Lock()
	defer s.identityMu.Unlock()
	s.id, s.cwd = state.SessionID, state.CWD
	if state.SessionFile == nil {
		s.file = ""
	} else {
		s.file = *state.SessionFile
	}
}

func (s *managedSession) touch() {
	if s != nil {
		s.lastAccess.Store(time.Now().UnixNano())
	}
}

func (s *managedSession) manager() *session.SessionManager {
	if s == nil || s.session == nil || s.session.Runtime() == nil || s.session.Runtime().Session() == nil {
		return nil
	}
	return s.session.Runtime().Session().SessionManager()
}

func (s *managedSession) busy() bool {
	if s == nil || s.session == nil {
		return false
	}
	state, err := s.session.State()
	return err == nil && (state.IsPromptRunning || state.IsStreaming || state.IsBashRunning || state.IsCompacting)
}

func (s *managedSession) dispose(ctx context.Context) error {
	if s == nil || s.session == nil {
		return nil
	}
	err := s.session.Dispose(ctx)
	if s.eventUnsubscribe != nil {
		s.eventUnsubscribe()
		s.eventUnsubscribe = nil
	}
	return err
}

type openCall struct {
	done    chan struct{}
	session *managedSession
	err     error
}

// Service owns the process-local set of independent ApplicationSession/Runtime handles.
// It coordinates lifecycle only: every conversation remains authoritative in
// Runtime -> AgentSession -> Agent and its SessionManager.
type Service struct {
	ctx                 context.Context
	cancel              context.CancelFunc
	production          app.ProductionConfig
	paths               app.ProductionPaths
	idle                time.Duration
	openRuntime         RuntimeOpener
	mutationMu          sync.Mutex
	resourceMu          sync.Mutex
	modelMu             sync.Mutex
	modelHTTP           HTTPDoer
	modelCatalogURL     string
	modelCatalogMu      sync.Mutex
	modelCatalogEntries []ModelCatalogEntry
	modelCatalogExpires time.Time
	allowedRootMu       sync.RWMutex
	allowedRoots        map[string]struct{}
	fileIndexMu         sync.Mutex
	fileIndexCache      map[string]fileIndexCacheEntry
	skillHTTP           HTTPDoer
	skillsAPI           string
	githubAPI           string

	mu         sync.Mutex
	sessions   map[string]*managedSession
	opening    map[string]*openCall
	events     *eventStream
	closed     bool
	reaperDone chan struct{}
}

func NewService(options ServiceOptions) (*Service, error) {
	ctx := options.Context
	if ctx == nil {
		ctx = context.Background()
	}
	paths, err := app.ResolveProductionPaths(options.Production)
	if err != nil {
		return nil, fmt.Errorf("resolve application production paths: %w", err)
	}
	idle := options.IdleTimeout
	if idle <= 0 {
		idle = defaultSessionIdleTimeout
	}
	opener := options.OpenRuntime
	if opener == nil {
		opener = app.OpenProductionRuntime
	}
	skillHTTP := options.SkillHTTP
	if skillHTTP == nil {
		skillHTTP = &http.Client{Timeout: 60 * time.Second}
	}
	modelHTTP := options.ModelHTTP
	if modelHTTP == nil {
		modelHTTP = &http.Client{Timeout: 30 * time.Second}
	}
	modelCatalogURL := strings.TrimRight(strings.TrimSpace(options.ModelCatalogURL), "/")
	if modelCatalogURL == "" {
		modelCatalogURL = "https://models.dev/api.json"
	}
	skillsAPI := strings.TrimRight(strings.TrimSpace(options.SkillsAPIURL), "/")
	if skillsAPI == "" {
		skillsAPI = strings.TrimRight(environmentValue(options.Production.Environment, "SKILLS_API_URL"), "/")
	}
	if skillsAPI == "" {
		skillsAPI = "https://skills.sh"
	}
	githubAPI := strings.TrimRight(strings.TrimSpace(options.GitHubAPIURL), "/")
	if githubAPI == "" {
		githubAPI = "https://api.github.com"
	}
	serviceCtx, cancel := context.WithCancel(ctx)
	s := &Service{
		ctx: serviceCtx, cancel: cancel, production: cloneProductionConfig(options.Production),
		paths: paths, idle: idle, openRuntime: opener,
		modelHTTP: modelHTTP, modelCatalogURL: modelCatalogURL,
		skillHTTP: skillHTTP, skillsAPI: skillsAPI, githubAPI: githubAPI,
		allowedRoots:   make(map[string]struct{}),
		fileIndexCache: make(map[string]fileIndexCacheEntry),
		sessions:       make(map[string]*managedSession), opening: make(map[string]*openCall),
		events: newEventStream(defaultEventHistoryCapacity), reaperDone: make(chan struct{}),
	}
	if options.DisableReaper {
		close(s.reaperDone)
	} else {
		go s.reapIdleSessions()
	}
	return s, nil
}

func cloneProductionConfig(config app.ProductionConfig) app.ProductionConfig {
	if config.Environment != nil {
		config.Environment = append([]string{}, config.Environment...)
	}
	return config
}

func normalizeContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func (s *Service) AgentDir() string {
	if s == nil {
		return ""
	}
	return s.paths.AgentDir
}

func (s *Service) DefaultCWD() string {
	if s == nil {
		return ""
	}
	return s.paths.WorkingDir
}

func ValidateCWD(value string) (string, error) {
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

func (s *Service) NewSession(ctx context.Context, options NewSessionOptions) (State, error) {
	if s == nil {
		return State{}, errors.New("application service is unavailable")
	}
	ctx = normalizeContext(ctx)
	cwd, err := ValidateCWD(options.CWD)
	if err != nil {
		return State{}, err
	}
	if (options.Provider == "") != (options.ModelID == "") {
		return State{}, errors.New("provider and modelId must be provided together")
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
		return State{}, err
	}
	managed, err := s.adoptRuntime(runtime)
	if err != nil {
		return State{}, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = managed.dispose(context.Background())
		}
	}()
	if options.HasToolNames {
		if _, err := managed.session.Dispatch(ctx, SetToolsCommand{ToolNames: append([]string(nil), options.ToolNames...)}); err != nil {
			return State{}, fmt.Errorf("set initial tools: %w", err)
		}
	}
	if options.ThinkingLevel != nil {
		if _, err := managed.session.Dispatch(ctx, SetThinkingLevelCommand{Level: *options.ThinkingLevel}); err != nil {
			return State{}, fmt.Errorf("set initial thinking level: %w", err)
		}
	}
	state, err := managed.session.State()
	if err != nil {
		return State{}, err
	}
	if err := s.register(managed); err != nil {
		return State{}, err
	}
	s.events.publish(Event{SessionID: state.SessionID, Value: SessionCatalogEvent{Change: SessionCreated}})
	managed.touch()
	cleanup = false
	return state, nil
}

func (s *Service) adoptRuntime(runtime *agentruntime.Runtime) (*managedSession, error) {
	if runtime == nil {
		return nil, errors.New("production runtime is unavailable")
	}
	for _, diagnostic := range runtime.Diagnostics() {
		if diagnostic.Kind == agentruntime.DiagnosticError {
			_ = runtime.Dispose(context.Background())
			return nil, errors.New(diagnostic.Message)
		}
	}
	// The application session outlives the request that caused it to open. Bind it to the
	// surface lifetime so returning a POST response cannot cancel a live agent.
	applicationSession, err := NewApplicationSession(s.ctx, runtime)
	if err != nil {
		_ = runtime.Dispose(context.Background())
		return nil, err
	}
	state, err := applicationSession.State()
	if err != nil {
		_ = applicationSession.Dispose(context.Background())
		return nil, err
	}
	file := ""
	if state.SessionFile != nil {
		file = *state.SessionFile
	}
	managed := &managedSession{session: applicationSession, id: state.SessionID, cwd: state.CWD, file: file}
	managed.eventUnsubscribe = applicationSession.Subscribe(func(_ context.Context, event Event) {
		s.events.publish(event)
	})
	managed.touch()
	return managed, nil
}

func (s *Service) register(managed *managedSession) error {
	id, _, _ := managed.identity()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("application service is closed")
	}
	if existing := s.sessions[id]; existing != nil && existing != managed {
		return fmt.Errorf("session %s is already active", id)
	}
	s.sessions[id] = managed
	return nil
}

func (s *Service) active(id string) (*managedSession, bool) {
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

func (s *Service) open(ctx context.Context, id string) (*managedSession, error) {
	if s == nil {
		return nil, errors.New("application service is unavailable")
	}
	ctx = normalizeContext(ctx)
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("session id is required")
	}
	if managed, ok := s.active(id); ok {
		return managed, nil
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("application service is closed")
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
		_ = redundant.dispose(context.Background())
	}
	return managed, err
}

func (s *Service) openExisting(ctx context.Context, id string) (*managedSession, error) {
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
	managed, err := s.adoptRuntime(runtime)
	if err != nil {
		return nil, err
	}
	managedID, _, _ := managed.identity()
	if managedID != id {
		_ = managed.dispose(context.Background())
		return nil, fmt.Errorf("opened session id %q does not match %q", managedID, id)
	}
	return managed, nil
}

func (s *Service) findSession(id string) (session.SessionInfo, bool, error) {
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

func (s *Service) Dispatch(ctx context.Context, id string, command Command) (CommandResult, error) {
	ctx = normalizeContext(ctx)
	managed, err := s.open(ctx, id)
	if err != nil {
		return nil, err
	}
	managed.touch()
	result, dispatchErr := managed.session.Dispatch(ctx, command)
	identityErr := s.reconcileIdentity(managed)
	if dispatchErr != nil {
		return nil, dispatchErr
	}
	if identityErr != nil {
		return nil, identityErr
	}
	s.publishCommandEffect(managed, command, result)
	return result, nil
}

func (s *Service) publishCommandEffect(managed *managedSession, command Command, result CommandResult) {
	id, _, _ := managed.identity()
	switch value := result.(type) {
	case ForkResult:
		if !value.Cancelled && value.SessionID != nil {
			s.events.publish(Event{SessionID: *value.SessionID, Value: SessionCatalogEvent{Change: SessionCreated}})
		}
	case SetSessionNameResult:
		if command.Type() == CommandSetSessionName {
			s.events.publish(Event{SessionID: id, Value: SessionCatalogEvent{Change: SessionUpdated}})
		}
	}
}

func (s *Service) reconcileIdentity(managed *managedSession) error {
	if managed == nil || managed.session == nil {
		return errors.New("managed session is unavailable")
	}
	state, err := managed.session.State()
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

func (s *Service) LiveState(id string) (State, bool, error) {
	managed, ok := s.active(id)
	if !ok {
		return State{}, false, nil
	}
	state, err := managed.session.State()
	return state, true, err
}

// CurrentRevision returns the last sequence assigned in the process-wide event
// stream shared by every application surface.
func (s *Service) CurrentRevision() uint64 {
	if s == nil {
		return 0
	}
	return s.events.currentRevision()
}

// SubscribeEvents returns every application event after the supplied revision,
// then continues with live events in the same total order.
func (s *Service) SubscribeEvents(after uint64) (*EventSubscription, error) {
	if s == nil {
		return nil, errors.New("application service is unavailable")
	}
	return s.events.subscribe(after)
}

func (s *Service) RunningIDs() []string {
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

func (s *Service) activeSessions() []*managedSession {
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

func (s *Service) reapIdleSessions() {
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

func (s *Service) reapOnce(now time.Time) {
	type candidate struct {
		id      string
		managed *managedSession
	}
	candidates := make([]candidate, 0)
	s.mu.Lock()
	for id, managed := range s.sessions {
		last := time.Unix(0, managed.lastAccess.Load())
		if now.Sub(last) < s.idle || managed.closing.Load() {
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
			now.Sub(last) >= s.idle &&
			candidate.managed.closing.CompareAndSwap(false, true) {
			delete(s.sessions, candidate.id)
			expired = append(expired, candidate.managed)
		}
		s.mu.Unlock()
	}
	for _, managed := range expired {
		_ = managed.dispose(context.Background())
	}
}

func (s *Service) Close(ctx context.Context) error {
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
		if err := managed.dispose(ctx); err != nil {
			closeErr = errors.Join(closeErr, err)
		}
	}
	s.events.close()
	return closeErr
}
