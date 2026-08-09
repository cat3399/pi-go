package resource

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// TrustStore persists explicit trust decisions. Missing is intentionally not
// equivalent to true; callers decide whether a resource-free project can be
// treated as trusted without writing a decision.
type TrustStore struct {
	path         string
	max          int64
	gate         chan struct{}
	poll         time.Duration
	leaseTTL     time.Duration
	heartbeat    time.Duration
	chtimes      func(string, time.Time, time.Time) error
	beforeRename func() error
	afterRename  func() error
	syncDir      func(string) error
}
type trustDecision struct {
	Trusted bool
	Known   bool
	Root    string
}
type TrustOption struct {
	Label     string
	Trusted   bool
	Updates   []TrustUpdate
	SavedPath string
}
type TrustUpdate struct {
	Path     string
	Decision *bool
}

func NewTrustStore(agentDir string) (*TrustStore, error) {
	if agentDir == "" || !utf8.ValidString(agentDir) || strings.IndexByte(agentDir, 0) >= 0 {
		return nil, fmt.Errorf("%w: invalid trust directory", ErrTrustStore)
	}
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	return &TrustStore{
		path: filepath.Join(agentDir, "trust.json"), max: DefaultMaxFileBytes,
		poll: 20 * time.Millisecond, leaseTTL: 10 * time.Second, heartbeat: time.Second,
		gate: gate, chtimes: os.Chtimes, syncDir: syncDirectory,
	}, nil
}
func (s *TrustStore) Path() string { return s.path }

func normalize(path string) (string, error) {
	if path == "" || !utf8.ValidString(path) || strings.IndexByte(path, 0) >= 0 {
		return "", fmt.Errorf("%w: invalid path", ErrTrustStore)
	}
	value, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%w: normalize: %w", ErrTrustStore, err)
	}
	value = filepath.Clean(value)
	if canonical, err := filepath.EvalSymlinks(value); err == nil {
		value = filepath.Clean(canonical)
	}
	return value, nil
}

func (s *TrustStore) acquire(ctx context.Context) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return nil, contextErr(ctx)
	case <-s.gate:
		return func() { s.gate <- struct{}{} }, nil
	}
}

func (s *TrustStore) Get(ctx context.Context, cwd string) (trusted bool, known bool, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := contextErr(ctx); err != nil {
		return false, false, err
	}
	release, err := s.acquire(ctx)
	if err != nil {
		return false, false, err
	}
	defer release()
	lease, err := s.acquirePersistentLock(ctx)
	if err != nil {
		return false, false, err
	}
	defer func() {
		if releaseErr := lease.release(); releaseErr != nil {
			trusted, known = false, false
			err = errors.Join(err, releaseErr)
		}
	}()
	root, _, err := s.read()
	if err != nil {
		return false, false, err
	}
	if err := lease.ensureOwned(); err != nil {
		return false, false, err
	}
	current, err := normalize(cwd)
	if err != nil {
		return false, false, err
	}
	trusted, known = false, false
	for {
		if raw, ok := root[current]; ok {
			if value, isKnown := boolDecision(raw); isKnown {
				trusted, known = value, true
				break
			}
			if !nullDecision(raw) {
				break
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	// Normalization may touch a slow filesystem. Recheck after all lookup work
	// so a process that lost its lease cannot return a stale decision.
	if err := lease.ensureOwned(); err != nil {
		return false, false, err
	}
	return trusted, known, nil
}

// decision is the Reload-only form of Get. Root identifies the closest saved
// entry so publication can reject a trust decision that changed mid-reload.
func (s *TrustStore) decision(ctx context.Context, cwd string) (decision trustDecision, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := contextErr(ctx); err != nil {
		return trustDecision{}, err
	}
	release, err := s.acquire(ctx)
	if err != nil {
		return trustDecision{}, err
	}
	defer release()
	lease, err := s.acquirePersistentLock(ctx)
	if err != nil {
		return trustDecision{}, err
	}
	defer func() {
		if releaseErr := lease.release(); releaseErr != nil {
			decision = trustDecision{}
			err = errors.Join(err, releaseErr)
		}
	}()
	decision, err = s.decisionUnlocked(cwd)
	if err != nil {
		return trustDecision{}, err
	}
	if err := lease.ensureOwned(); err != nil {
		return trustDecision{}, err
	}
	return decision, nil
}

func (s *TrustStore) decisionUnlocked(cwd string) (trustDecision, error) {
	root, _, err := s.read()
	if err != nil {
		return trustDecision{}, err
	}
	current, err := normalize(cwd)
	if err != nil {
		return trustDecision{}, err
	}
	for {
		if raw, ok := root[current]; ok {
			if value, known := boolDecision(raw); known {
				return trustDecision{Trusted: value, Known: true, Root: current}, nil
			}
			if !nullDecision(raw) {
				return trustDecision{}, nil
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			return trustDecision{}, nil
		}
		current = parent
	}
}

func (s *TrustStore) confirmDecision(ctx context.Context, cwd string, want trustDecision, publish func() error) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	release, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	lease, err := s.acquirePersistentLock(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lease.release()) }()
	got, err := s.decisionUnlocked(cwd)
	if err != nil {
		return err
	}
	if got != want {
		return ErrStaleReload
	}
	if err := lease.ensureOwned(); err != nil {
		return err
	}
	return publish()
}

func (s *TrustStore) Set(ctx context.Context, cwd string, trusted bool) error {
	return s.SetMany(ctx, []TrustUpdate{{Path: cwd, Decision: &trusted}})
}
func (s *TrustStore) SetMany(ctx context.Context, changes []TrustUpdate) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	releaseMemory, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer releaseMemory()
	if err := contextErr(ctx); err != nil {
		return err
	}
	lease, err := s.acquirePersistentLock(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lease.release()) }()
	root, exists, err := s.read()
	if err != nil {
		return err
	}
	if !exists {
		root = map[string]json.RawMessage{}
	}
	for _, change := range changes {
		key, err := normalize(change.Path)
		if err != nil {
			return err
		}
		if change.Decision == nil {
			delete(root, key)
		} else {
			raw := []byte("false")
			if *change.Decision {
				raw = []byte("true")
			}
			root[key] = raw
		}
	}
	keys := make([]string, 0, len(root))
	for key := range root {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out bytes.Buffer
	out.WriteString("{\n")
	for i, key := range keys {
		encodedKey, err := json.Marshal(key)
		if err != nil {
			return fmt.Errorf("%w: encode trust path", ErrTrustStore)
		}
		out.WriteString("  ")
		out.Write(encodedKey)
		out.WriteString(": ")
		out.Write(root[key])
		if i+1 < len(keys) {
			out.WriteByte(',')
		}
		out.WriteByte('\n')
	}
	out.WriteString("}\n")
	if int64(out.Len()) > s.max {
		return fmt.Errorf("%w: %w", ErrTrustStore, ErrTooLarge)
	}
	return s.atomic(out.Bytes(), lease)
}
func (s *TrustStore) Options(cwd string) ([]TrustOption, error) {
	return s.OptionsWithSession(cwd, false)
}

func (s *TrustStore) OptionsWithSession(cwd string, includeSessionOnly bool) ([]TrustOption, error) {
	path, err := normalize(cwd)
	if err != nil {
		return nil, err
	}
	parent := filepath.Dir(path)
	yes := true
	no := false
	values := []TrustOption{{Label: "Trust", Trusted: true, Updates: []TrustUpdate{{Path: path, Decision: &yes}}, SavedPath: path}}
	if parent != path {
		values = append(values, TrustOption{Label: "Trust parent folder (" + parent + ")", Trusted: true, Updates: []TrustUpdate{{Path: parent, Decision: &yes}, {Path: path, Decision: nil}}, SavedPath: parent})
	}
	if includeSessionOnly {
		values = append(values, TrustOption{Label: "Trust (this session only)", Trusted: true})
	}
	values = append(values, TrustOption{Label: "Do not trust", Trusted: false, Updates: []TrustUpdate{{Path: path, Decision: &no}}, SavedPath: path})
	if includeSessionOnly {
		values = append(values, TrustOption{Label: "Do not trust (this session only)", Trusted: false})
	}
	return values, nil
}

// HasTrustRequiringProjectResources mirrors coding-agent's trust admission
// probe. Context files are intentionally excluded: AGENTS.md/CLAUDE.md are
// loaded independently of project trust.
func HasTrustRequiringProjectResources(cwd string) bool {
	current, err := normalize(cwd)
	if err != nil {
		return false
	}
	for _, name := range []string{"settings.json", "extensions", "skills", "prompts", "themes", "SYSTEM.md", "APPEND_SYSTEM.md"} {
		if _, err := os.Stat(filepath.Join(current, ".pi", name)); err == nil {
			return true
		}
	}
	home, _ := os.UserHomeDir()
	userSkills := filepath.Join(canonicalTrustPath(home), ".agents", "skills")
	for directory := current; ; directory = filepath.Dir(directory) {
		candidate := filepath.Join(directory, ".agents", "skills")
		if candidate != userSkills {
			if _, err := os.Stat(candidate); err == nil {
				return true
			}
		}
		if parent := filepath.Dir(directory); parent == directory {
			break
		}
	}
	return false
}

func canonicalTrustPath(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(real)
	}
	if absolute, err := filepath.Abs(path); err == nil {
		return filepath.Clean(absolute)
	}
	return filepath.Clean(path)
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	if cause := context.Cause(ctx); cause != nil {
		return fmt.Errorf("%w: cancelled: %w", ErrTrustStore, cause)
	}
	return nil
}
func (s *TrustStore) read() (map[string]json.RawMessage, bool, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: read trust store", ErrTrustStore)
	}
	if int64(len(data)) > s.max {
		return nil, false, fmt.Errorf("%w: trust store too large", ErrTrustStore)
	}
	if !utf8.Valid(data) {
		return nil, false, fmt.Errorf("%w: malformed trust store", ErrTrustStore)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil || root == nil {
		return nil, false, fmt.Errorf("%w: malformed trust store", ErrTrustStore)
	}
	for key, raw := range root {
		if _, ok := boolDecision(raw); !ok && !nullDecision(raw) {
			return nil, false, fmt.Errorf("%w: value for %q must be true, false, or null", ErrTrustStore, key)
		}
	}
	return root, true, nil
}

func boolDecision(raw json.RawMessage) (bool, bool) {
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("true")) {
		return true, true
	}
	if bytes.Equal(trimmed, []byte("false")) {
		return false, true
	}
	// null is intentionally not a boolean and means inheritance.
	return false, false
}

func nullDecision(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
func decodeStrictObject(data []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("root")
	}
	out := map[string]json.RawMessage{}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, ok := key.(string)
		if !ok {
			return nil, errors.New("key")
		}
		if _, ok := out[name]; ok {
			return nil, errors.New("duplicate")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		if err := validateRawJSON(raw); err != nil {
			return nil, err
		}
		out[name] = append(json.RawMessage(nil), raw...)
	}
	token, err = dec.Token()
	if err != nil || token != json.Delim('}') {
		return nil, errors.New("close")
	}
	if _, err = dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing")
	}
	return out, nil
}

func validateRawJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := validateJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing value")
	}
	return nil
}

func validateJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key")
			}
			if _, duplicate := seen[key]; duplicate {
				return errors.New("duplicate object key")
			}
			seen[key] = struct{}{}
			if err := validateJSONValue(decoder); err != nil {
				return err
			}
		}
		closeToken, err := decoder.Token()
		if err != nil || closeToken != json.Delim('}') {
			return errors.New("object close")
		}
	case '[':
		for decoder.More() {
			if err := validateJSONValue(decoder); err != nil {
				return err
			}
		}
		closeToken, err := decoder.Token()
		if err != nil || closeToken != json.Delim(']') {
			return errors.New("array close")
		}
	default:
		return errors.New("unexpected delimiter")
	}
	return nil
}

const (
	trustLockOwnerFile     = "owner"
	trustLockHeartbeatFile = "heartbeat"
	// Kept only so a lock left by the earlier transition-based implementation
	// can be cleaned after it is atomically retired. New recovery never mutates
	// the active lock directory before the final rename.
	trustLockTransition = "transition"
)

var errTrustLeaseOwnershipLost = errors.New("trust lock lease ownership lost")

type trustLease struct {
	store   *TrustStore
	path    string
	token   string
	stop    chan struct{}
	done    chan struct{}
	stateMu sync.RWMutex

	lastRefresh time.Time
	compromised error
	releaseErr  error
	stopOnce    sync.Once
	endOnce     sync.Once
}

func (s *TrustStore) lock(ctx context.Context) (*trustLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	path := s.path + ".lock"
	for {
		if err := contextErr(ctx); err != nil {
			return nil, err
		}
		token, err := newTrustLeaseToken()
		if err != nil {
			return nil, fmt.Errorf("%w: create lock token", ErrTrustStore)
		}
		if err := platformCreatePrivateTrustDirectory(path); err == nil {
			lease := &trustLease{store: s, path: path, token: token, stop: make(chan struct{}), done: make(chan struct{})}
			if err := lease.initialize(); err != nil {
				cleanupErr := lease.cleanupFreshDirectory()
				return nil, errors.Join(fmt.Errorf("%w: initialize lock lease: %v", ErrTrustStore, err), cleanupErr)
			}
			lease.startHeartbeat()
			return lease, nil
		} else if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("%w: acquire lock: %v", ErrTrustStore, err)
		}
		recovered, err := s.recoverStaleLease(path, token)
		if err != nil {
			return nil, err
		}
		if recovered {
			continue
		}
		timer := time.NewTimer(s.poll)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, contextErr(ctx)
		case <-timer.C:
		}
	}
}

func (s *TrustStore) acquirePersistentLock(ctx context.Context) (*trustLease, error) {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return nil, fmt.Errorf("%w: create trust directory", ErrTrustStore)
	}
	return s.lock(ctx)
}

func newTrustLeaseToken() (string, error) {
	var value [24]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func (l *trustLease) initialize() error {
	if err := writeExclusivePrivateFile(filepath.Join(l.path, trustLockOwnerFile), []byte(l.token)); err != nil {
		return err
	}
	if err := writeExclusivePrivateFile(filepath.Join(l.path, trustLockHeartbeatFile), []byte(l.token)); err != nil {
		return err
	}
	l.stateMu.Lock()
	l.lastRefresh = time.Now()
	l.stateMu.Unlock()
	return nil
}

func writeExclusivePrivateFile(path string, data []byte) error {
	file, err := platformCreateExclusivePrivateTrustFile(path)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func (l *trustLease) startHeartbeat() {
	go func() {
		defer close(l.done)
		interval := l.heartbeatInterval()
		delay := interval
		for {
			timer := time.NewTimer(delay)
			select {
			case <-l.stop:
				if !timer.Stop() {
					<-timer.C
				}
				return
			case <-timer.C:
			}
			if err := l.refreshOnce(); err != nil {
				if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errTrustLeaseOwnershipLost) || l.refreshExpired() {
					l.markCompromised(err)
					return
				}
				// Match proper-lockfile's bounded recovery idea: transient stat/
				// Chtimes errors retry faster, but only until the lease TTL.
				delay = minDuration(interval, 100*time.Millisecond)
				continue
			}
			delay = interval
		}
	}()
}

func (l *trustLease) heartbeatInterval() time.Duration {
	interval := l.store.heartbeat
	if interval <= 0 {
		return time.Second
	}
	return interval
}

func (l *trustLease) leaseTTL() time.Duration {
	ttl := l.store.leaseTTL
	if ttl <= 0 {
		return 10 * time.Second
	}
	return ttl
}

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}

func (l *trustLease) refreshOnce() error {
	if err := l.verifyLeaseFiles(); err != nil {
		return err
	}
	now := time.Now()
	if err := l.store.chtimes(filepath.Join(l.path, trustLockHeartbeatFile), now, now); err != nil {
		return fmt.Errorf("refresh lock heartbeat: %w", err)
	}
	if err := l.verifyLeaseFiles(); err != nil {
		return err
	}
	l.stateMu.Lock()
	defer l.stateMu.Unlock()
	if l.compromised != nil {
		return l.compromised
	}
	l.lastRefresh = now
	return nil
}

func (l *trustLease) ensureOwned() error {
	if l == nil {
		return fmt.Errorf("%w: missing lock lease", ErrTrustStore)
	}
	if err := l.compromiseError(); err != nil {
		return err
	}
	if l.refreshExpired() {
		return l.markCompromised(errors.New("heartbeat expired before ownership check"))
	}
	if err := l.verifyLeaseFiles(); err != nil {
		return l.markCompromised(err)
	}
	if err := l.compromiseError(); err != nil {
		return err
	}
	return nil
}

func (l *trustLease) verifyLeaseFiles() error {
	owner, err := os.ReadFile(filepath.Join(l.path, trustLockOwnerFile))
	if err != nil {
		return fmt.Errorf("%w: read owner token: %w", errTrustLeaseOwnershipLost, err)
	}
	if string(owner) != l.token {
		return fmt.Errorf("%w: owner token changed", errTrustLeaseOwnershipLost)
	}
	heartbeat, err := os.ReadFile(filepath.Join(l.path, trustLockHeartbeatFile))
	if err != nil {
		return fmt.Errorf("%w: read heartbeat token: %w", errTrustLeaseOwnershipLost, err)
	}
	if string(heartbeat) != l.token {
		return fmt.Errorf("%w: heartbeat token changed", errTrustLeaseOwnershipLost)
	}
	return nil
}

func (l *trustLease) refreshExpired() bool {
	l.stateMu.RLock()
	last := l.lastRefresh
	l.stateMu.RUnlock()
	return last.IsZero() || time.Since(last) > l.leaseTTL()
}

func (l *trustLease) compromiseError() error {
	l.stateMu.RLock()
	defer l.stateMu.RUnlock()
	return l.compromised
}

func (l *trustLease) markCompromised(cause error) error {
	if cause == nil {
		cause = errTrustLeaseOwnershipLost
	}
	l.stateMu.Lock()
	if l.compromised == nil {
		l.compromised = fmt.Errorf("%w: lock lease compromised: %w", ErrTrustStore, cause)
	}
	err := l.compromised
	l.stateMu.Unlock()
	return err
}

func (l *trustLease) ownsToken() error {
	owner, err := os.ReadFile(filepath.Join(l.path, trustLockOwnerFile))
	if err != nil {
		return fmt.Errorf("%w: inspect lock owner during release: %w", ErrTrustStore, err)
	}
	if string(owner) != l.token {
		return fmt.Errorf("%w: release refused for replacement owner", ErrTrustStore)
	}
	return nil
}

func (l *trustLease) release() error {
	if l == nil {
		return nil
	}
	l.endOnce.Do(func() {
		l.stopOnce.Do(func() { close(l.stop) })
		<-l.done
		compromised := l.compromiseError()
		if verifyErr := l.verifyLeaseFiles(); verifyErr != nil {
			compromised = errors.Join(compromised, l.markCompromised(verifyErr))
		}
		if err := l.ownsToken(); err != nil {
			l.releaseErr = errors.Join(compromised, err)
			return
		}
		retired := l.path + ".released-" + l.token
		if err := os.Rename(l.path, retired); err != nil {
			l.releaseErr = errors.Join(compromised, fmt.Errorf("%w: retire lock during release: %v", ErrTrustStore, err))
			return
		}
		l.releaseErr = errors.Join(compromised, cleanupTrustLeaseDirectory(retired))
	})
	return l.releaseErr
}

func (l *trustLease) cleanupFreshDirectory() error {
	if l == nil {
		return nil
	}
	owner, err := os.ReadFile(filepath.Join(l.path, trustLockOwnerFile))
	if err == nil && string(owner) != l.token {
		return fmt.Errorf("%w: fresh lock owner changed during cleanup", ErrTrustStore)
	}
	return cleanupTrustLeaseDirectory(l.path)
}

func cleanupTrustLeaseDirectory(path string) error {
	var cleanupErr error
	for _, name := range []string{trustLockHeartbeatFile, trustLockOwnerFile, trustLockTransition} {
		if err := os.Remove(filepath.Join(path, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove %s: %w", name, err))
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove lock directory: %w", err))
	}
	if cleanupErr != nil {
		return fmt.Errorf("%w: clean retired lock %s: %w", ErrTrustStore, path, cleanupErr)
	}
	return nil
}

func (s *TrustStore) recoverStaleLease(path, recoveryToken string) (bool, error) {
	stale, observed, err := s.leaseIsStale(path)
	if err != nil || !stale {
		return false, err
	}
	// The second observation is deliberately read-only. In particular, no
	// claim is created inside path: empty/legacy locks use the directory mtime
	// as their heartbeat, so touching it would make them immortal.
	stillStale, current, err := s.leaseIsStale(path)
	if err != nil || !stillStale || current.owner != observed.owner || current.heartbeat != observed.heartbeat {
		return false, err
	}
	retired := path + ".stale-" + recoveryToken
	if err := os.Rename(path, retired); err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("%w: recover stale lock: %v", ErrTrustStore, err)
	}
	// Cleanup is best-effort after the atomic retirement. A claimant crash or
	// an unknown legacy entry can leave this unique sibling behind, but it can
	// never block acquisition of the active lock path.
	_ = cleanupTrustLeaseDirectory(retired)
	return true, nil
}

type trustLockObservation struct {
	owner     string
	heartbeat bool
}

func (s *TrustStore) leaseIsStale(path string) (bool, trustLockObservation, error) {
	var observed trustLockObservation
	ownerData, ownerErr := os.ReadFile(filepath.Join(path, trustLockOwnerFile))
	if ownerErr != nil && !errors.Is(ownerErr, os.ErrNotExist) {
		return false, observed, fmt.Errorf("%w: inspect lock owner: %v", ErrTrustStore, ownerErr)
	}
	observed.owner = string(ownerData)
	info, err := os.Stat(filepath.Join(path, trustLockHeartbeatFile))
	if errors.Is(err, os.ErrNotExist) {
		info, err = os.Stat(path)
	} else if err == nil {
		observed.heartbeat = true
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, observed, nil
		}
		return false, observed, fmt.Errorf("%w: inspect lock heartbeat: %v", ErrTrustStore, err)
	}
	ttl := s.leaseTTL
	if ttl <= 0 {
		ttl = 10 * time.Second
	}
	return time.Since(info.ModTime()) > ttl, observed, nil
}

func (s *TrustStore) atomic(data []byte, lease *trustLease) error {
	dir := filepath.Dir(s.path)
	temp, err := platformCreatePrivateTrustTempFile(dir, ".trust.json-", ".tmp")
	if err != nil {
		return fmt.Errorf("%w: create temporary", ErrTrustStore)
	}
	name := temp.Name()
	done := false
	defer func() {
		if !done {
			_ = os.Remove(name)
		}
	}()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("%w: write", ErrTrustStore)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("%w: sync", ErrTrustStore)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("%w: close", ErrTrustStore)
	}
	if s.beforeRename != nil {
		if err := s.beforeRename(); err != nil {
			return fmt.Errorf("%w: replace", ErrTrustStore)
		}
	}
	if err := lease.ensureOwned(); err != nil {
		return err
	}
	if err := os.Rename(name, s.path); err != nil {
		return fmt.Errorf("%w: replace", ErrTrustStore)
	}
	done = true
	if s.afterRename != nil {
		if err := s.afterRename(); err != nil {
			return fmt.Errorf("%w: %w", ErrCommitUnknown, err)
		}
	}
	if err := s.syncDir(dir); err != nil {
		return fmt.Errorf("%w: %w", ErrCommitUnknown, err)
	}
	return nil
}
