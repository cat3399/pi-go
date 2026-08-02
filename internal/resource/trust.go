package resource

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"
	"unicode/utf8"
)

// TrustStore persists explicit trust decisions. Missing is intentionally not
// equivalent to true. Unknown JSON members survive a mutation byte-for-byte.
type TrustStore struct {
	path         string
	max          int64
	gate         chan struct{}
	poll         time.Duration
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
	Label   string
	Trusted bool
	Updates []TrustUpdate
}
type TrustUpdate struct {
	Path     string
	Decision *bool
}

func NewTrustStore(agentDir string) (*TrustStore, error) {
	if agentDir == "" || !utf8.ValidString(agentDir) {
		return nil, fmt.Errorf("%w: invalid trust directory", ErrTrustStore)
	}
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	return &TrustStore{path: filepath.Join(agentDir, "trust.json"), max: DefaultMaxFileBytes, poll: 20 * time.Millisecond, gate: gate, syncDir: syncDirectory}, nil
}
func (s *TrustStore) Path() string { return s.path }

// normalize is deliberately lexical: admission must not inspect an untrusted
// cwd. A different symlink spelling therefore never inherits a decision until
// its own lexical ancestor is explicitly trusted.
func normalize(path string) (string, error) {
	if path == "" || !utf8.ValidString(path) {
		return "", fmt.Errorf("%w: invalid path", ErrTrustStore)
	}
	value, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%w: normalize: %w", ErrTrustStore, err)
	}
	return filepath.Clean(value), nil
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

func (s *TrustStore) Get(ctx context.Context, cwd string) (bool, bool, error) {
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
	root, _, err := s.read()
	if err != nil {
		return false, false, err
	}
	current, err := normalize(cwd)
	if err != nil {
		return false, false, err
	}
	for {
		if raw, ok := root[current]; ok {
			if value, known := boolDecision(raw); known {
				return value, true, nil
			}
			if !nullDecision(raw) {
				return false, false, nil
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false, false, nil
		}
		current = parent
	}
}

// decision is the Reload-only form of Get.  Root identifies the closest
// explicit decision, which bounds ancestor instruction discovery: an exact
// trust decision must not accidentally authorize arbitrary files above it.
func (s *TrustStore) decision(ctx context.Context, cwd string) (trustDecision, error) {
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
	return s.decisionUnlocked(cwd)
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

func (s *TrustStore) confirmDecision(ctx context.Context, cwd string, want trustDecision, publish func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	release, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	got, err := s.decisionUnlocked(cwd)
	if err != nil {
		return err
	}
	if got != want {
		return ErrStaleReload
	}
	return publish()
}

func (s *TrustStore) Set(ctx context.Context, cwd string, trusted bool) error {
	return s.SetMany(ctx, []TrustUpdate{{Path: cwd, Decision: &trusted}})
}
func (s *TrustStore) SetMany(ctx context.Context, changes []TrustUpdate) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return fmt.Errorf("%w: persistent private trust is unavailable on Windows", ErrTrustStore)
	}
	releaseMemory, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer releaseMemory()
	if err := contextErr(ctx); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("%w: create trust directory", ErrTrustStore)
	}
	release, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer release()
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
		fmt.Fprintf(&out, "  %q: %s", key, root[key])
		if i+1 < len(keys) {
			out.WriteByte(',')
		}
		out.WriteByte('\n')
	}
	out.WriteString("}\n")
	if int64(out.Len()) > s.max {
		return fmt.Errorf("%w: %w", ErrTrustStore, ErrTooLarge)
	}
	return s.atomic(out.Bytes())
}
func (s *TrustStore) Options(cwd string) ([]TrustOption, error) {
	path, err := normalize(cwd)
	if err != nil {
		return nil, err
	}
	parent := filepath.Dir(path)
	yes := true
	no := false
	values := []TrustOption{{Label: "Trust", Trusted: true, Updates: []TrustUpdate{{Path: path, Decision: &yes}}}, {Label: "Do not trust", Trusted: false, Updates: []TrustUpdate{{Path: path, Decision: &no}}}}
	if parent != path {
		values = append([]TrustOption{{Label: "Trust parent folder (" + parent + ")", Trusted: true, Updates: []TrustUpdate{{Path: parent, Decision: &yes}, {Path: path, Decision: nil}}}}, values...)
	}
	return values, nil
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
	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: read trust store", ErrTrustStore)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%w: unsafe trust store", ErrTrustStore)
	}
	f, err := os.Open(s.path)
	if err != nil {
		return nil, false, fmt.Errorf("%w: read trust store", ErrTrustStore)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, false, fmt.Errorf("%w: unsafe trust store", ErrTrustStore)
	}
	if runtime.GOOS == "windows" {
		return nil, false, fmt.Errorf("%w: private admission unavailable", ErrTrustStore)
	}
	if opened.Mode().Perm()&0o077 != 0 {
		return nil, false, fmt.Errorf("%w: trust store permissions are unsafe", ErrTrustStore)
	}
	if opened.Size() > s.max {
		return nil, false, fmt.Errorf("%w: trust store too large", ErrTrustStore)
	}
	data, err := io.ReadAll(io.LimitReader(f, s.max+1))
	if err != nil || int64(len(data)) > s.max || !utf8.Valid(data) {
		return nil, false, fmt.Errorf("%w: malformed trust store", ErrTrustStore)
	}
	root, err := decodeStrictObject(data)
	if err != nil {
		return nil, false, fmt.Errorf("%w: malformed trust store", ErrTrustStore)
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
	// null and future values are intentionally not booleans. Callers distinguish
	// null (inherit) from a future value (an unknown stop point).
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
func (s *TrustStore) lock(ctx context.Context) (func(), error) {
	path := s.path + ".lock"
	for {
		if err := os.Mkdir(path, 0o700); err == nil {
			return func() { _ = os.Remove(path) }, nil
		} else if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("%w: acquire lock", ErrTrustStore)
		}
		if err := contextErr(ctx); err != nil {
			return nil, err
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
func (s *TrustStore) atomic(data []byte) error {
	dir := filepath.Dir(s.path)
	temp, err := os.CreateTemp(dir, ".trust.json-*")
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
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("%w: private temporary", ErrTrustStore)
	}
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

func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
