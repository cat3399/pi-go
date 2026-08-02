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
	"sync"
	"time"
	"unicode/utf8"
)

// TrustStore persists explicit trust decisions. Missing is intentionally not
// equivalent to true. Unknown JSON members survive a mutation byte-for-byte.
type TrustStore struct {
	path         string
	max          int64
	mu           sync.Mutex
	poll         time.Duration
	beforeRename func() error
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
	return &TrustStore{path: filepath.Join(agentDir, "trust.json"), max: DefaultMaxFileBytes, poll: 20 * time.Millisecond}, nil
}
func (s *TrustStore) Path() string { return s.path }
func normalize(path string) (string, error) {
	if path == "" || !utf8.ValidString(path) {
		return "", fmt.Errorf("%w: invalid path", ErrTrustStore)
	}
	value, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%w: normalize: %w", ErrTrustStore, err)
	}
	// A trust entry is an authority boundary, not merely a display string.  Use
	// the physical path when it already exists so a trusted lexical parent
	// cannot be used to admit a different directory through a symlink.
	if resolved, resolveErr := filepath.EvalSymlinks(value); resolveErr == nil {
		value = resolved
	} else if !errors.Is(resolveErr, os.ErrNotExist) {
		return "", fmt.Errorf("%w: normalize: %v", ErrTrustStore, resolveErr)
	}
	return filepath.Clean(value), nil
}

func (s *TrustStore) Get(ctx context.Context, cwd string) (bool, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := contextErr(ctx); err != nil {
		return false, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
			var value bool
			if err := json.Unmarshal(raw, &value); err == nil && (bytes.Equal(bytes.TrimSpace(raw), []byte("true")) || bytes.Equal(bytes.TrimSpace(raw), []byte("false"))) {
				return value, true, nil
			}
			return false, false, fmt.Errorf("%w: invalid decision", ErrTrustStore)
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
	s.mu.Lock()
	defer s.mu.Unlock()
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
			value := bytes.Equal(bytes.TrimSpace(raw), []byte("true"))
			if !value && !bytes.Equal(bytes.TrimSpace(raw), []byte("false")) {
				return trustDecision{}, fmt.Errorf("%w: invalid decision", ErrTrustStore)
			}
			return trustDecision{Trusted: value, Known: true, Root: current}, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return trustDecision{}, nil
		}
		current = parent
	}
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
	s.mu.Lock()
	defer s.mu.Unlock()
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
	for _, raw := range root {
		trimmed := bytes.TrimSpace(raw)
		if !bytes.Equal(trimmed, []byte("true")) && !bytes.Equal(trimmed, []byte("false")) {
			return nil, false, fmt.Errorf("%w: malformed trust decision", ErrTrustStore)
		}
	}
	return root, true, nil
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
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
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
	if f, err := os.Open(dir); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}
	return nil
}
