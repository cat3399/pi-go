package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"unicode/utf8"
)

func (s *Store) Read(ctx context.Context, provider string) (Credential, bool, error) {
	if !validProviderID(provider) {
		return Credential{}, false, failure(KindInvalid, "read credential", provider, nil)
	}
	if cause := context.Cause(ctx); cause != nil {
		return Credential{}, false, failure(KindCancelled, "read credential", provider, cause)
	}
	release, err := s.acquireLocal(ctx, "read credential", provider)
	if err != nil {
		return Credential{}, false, err
	}
	defer release()
	root, exists, err := s.readRoot()
	if err != nil || !exists {
		return Credential{}, false, err
	}
	raw, ok := root[provider]
	if !ok {
		return Credential{}, false, nil
	}
	credential, err := parseCredential(raw, provider)
	return credential, err == nil, err
}

func (s *Store) List(ctx context.Context) ([]Info, error) {
	if cause := context.Cause(ctx); cause != nil {
		return nil, failure(KindCancelled, "list credentials", "", cause)
	}
	release, err := s.acquireLocal(ctx, "list credentials", "")
	if err != nil {
		return nil, err
	}
	defer release()
	root, exists, err := s.readRoot()
	if err != nil || !exists {
		return nil, err
	}
	result := make([]Info, 0, len(root))
	for provider, raw := range root {
		credential, err := parseCredential(raw, provider)
		if err != nil {
			return nil, err
		}
		result = append(result, Info{ProviderID: provider, Type: credential.Type})
	}
	sort.Slice(result, func(left, right int) bool { return result[left].ProviderID < result[right].ProviderID })
	return result, nil
}

func (s *Store) SetAPIKey(ctx context.Context, provider, key string, environment map[string]string) error {
	if !validProviderID(provider) || !validAPIKey(key) {
		return failure(KindInvalid, "set credential", provider, nil)
	}
	for name, value := range environment {
		if !validEnvironmentName(name) || !utf8Valid(value) {
			return failure(KindInvalid, "set credential", provider, nil)
		}
	}
	encoded, err := json.Marshal(struct {
		Type string            `json:"type"`
		Key  string            `json:"key"`
		Env  map[string]string `json:"env,omitempty"`
	}{"api_key", key, cloneEnv(environment)})
	if err != nil {
		return failure(KindInvalid, "set credential", provider, err)
	}
	return s.mutate(ctx, "set credential", provider, func(root map[string]json.RawMessage) { root[provider] = encoded })
}

func (s *Store) Delete(ctx context.Context, provider string) error {
	if !validProviderID(provider) {
		return failure(KindInvalid, "delete credential", provider, nil)
	}
	return s.mutate(ctx, "delete credential", provider, func(root map[string]json.RawMessage) { delete(root, provider) })
}

func (s *Store) mutate(ctx context.Context, operation, provider string, change func(map[string]json.RawMessage)) error {
	if cause := context.Cause(ctx); cause != nil {
		return failure(KindCancelled, operation, provider, cause)
	}
	if runtime.GOOS == "windows" {
		return failure(KindUnsupported, operation, provider, ErrPersistentAuthUnavailable)
	}
	releaseLocal, err := s.acquireLocal(ctx, operation, provider)
	if err != nil {
		return err
	}
	defer releaseLocal()
	if err := os.MkdirAll(lockPathParent(s.path), 0o700); err != nil {
		return failure(KindIO, operation, provider, err)
	}
	release, err := s.acquireFileLock(ctx)
	if err != nil {
		return err
	}
	defer release()
	root, exists, err := s.readRoot()
	if err != nil {
		return err
	}
	if !exists {
		root = make(map[string]json.RawMessage)
	}
	change(root)
	encoded, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return failure(KindIO, operation, provider, err)
	}
	encoded = append(encoded, '\n')
	return s.atomicWrite(encoded, operation, provider)
}

func (s *Store) readRoot() (map[string]json.RawMessage, bool, error) {
	file, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, failure(KindIO, "read auth file", "", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, false, failure(KindIO, "inspect auth file", "", err)
	}
	if !info.Mode().IsRegular() {
		return nil, false, failure(KindInvalid, "read auth file", "", nil)
	}
	if runtime.GOOS == "windows" {
		return nil, false, failure(KindPermission, "read auth file", "", ErrPrivateAdmissionUnavailable)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, false, failure(KindPermission, "read auth file", "", nil)
	}
	if info.Size() > s.maxFileBytes {
		return nil, false, failure(KindInvalid, "read auth file", "", nil)
	}
	data, err := io.ReadAll(io.LimitReader(file, s.maxFileBytes+1))
	if err != nil {
		return nil, false, failure(KindIO, "read auth file", "", err)
	}
	if int64(len(data)) > s.maxFileBytes {
		return nil, false, failure(KindInvalid, "read auth file", "", nil)
	}
	if !utf8.Valid(data) {
		return nil, false, failure(KindMalformed, "parse auth file", "", nil)
	}
	root, err := decodeObject(data)
	if err != nil {
		return nil, false, failure(KindMalformed, "parse auth file", "", err)
	}
	return root, true, nil
}

func (s *Store) atomicWrite(data []byte, operation, provider string) error {
	directory := filepath.Dir(s.path)
	temporary, err := os.CreateTemp(directory, ".auth.json-*")
	if err != nil {
		return failure(KindIO, operation, provider, err)
	}
	temporaryName := temporary.Name()
	completed := false
	defer func() {
		if !completed {
			_ = os.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return failure(KindPermission, operation, provider, err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return failure(KindIO, operation, provider, err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return failure(KindIO, operation, provider, err)
	}
	if err := temporary.Close(); err != nil {
		return failure(KindIO, operation, provider, err)
	}
	if s.beforeRename != nil {
		if err := s.beforeRename(); err != nil {
			return failure(KindIO, operation, provider, err)
		}
	}
	if err := os.Rename(temporaryName, s.path); err != nil {
		return failure(KindIO, operation, provider, err)
	}
	completed = true
	// Persistent Windows auth is fail-closed before this path because v0.1 has
	// no DACL admission/creation implementation. On supported platforms the
	// directory sync makes the rename as durable as the filesystem allows.
	if directoryFile, err := os.Open(directory); err == nil {
		_ = directoryFile.Sync()
		_ = directoryFile.Close()
	}
	return nil
}
