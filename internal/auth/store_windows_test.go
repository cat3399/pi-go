//go:build windows

package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This suite runs only on a real Windows Go test runner. It verifies v0.1's
// fail-closed policy; it does not pretend Unix mode bits establish a DACL.
func TestWindowsPersistentAuthFailClosedAndEphemeralSourcesRemainUsable(t *testing.T) {
	store := newTestStore(t)
	if _, exists, err := store.Read(context.Background(), "openai"); err != nil || exists {
		t.Fatalf("missing Windows auth file = exists %t, error %v", exists, err)
	}
	if infos, err := store.List(context.Background()); err != nil || len(infos) != 0 {
		t.Fatalf("list missing Windows auth file = %#v, %v", infos, err)
	}
	if err := store.SetAPIKey(context.Background(), "openai", "persistent-secret", nil); !errors.Is(err, ErrPersistentAuthUnavailable) || !IsKind(err, KindUnsupported) {
		t.Fatalf("Windows persistent set error = %v", err)
	}
	if err := store.Delete(context.Background(), "openai"); !errors.Is(err, ErrPersistentAuthUnavailable) || !IsKind(err, KindUnsupported) {
		t.Fatalf("Windows persistent delete error = %v", err)
	}
	if _, err := os.Stat(store.Path()); !os.IsNotExist(err) {
		t.Fatalf("Windows fail-closed mutation created a file: %v", err)
	}

	runtimeCredentials := NewRuntime(store)
	if err := runtimeCredentials.SetAPIKey("openai", "runtime-key"); err != nil {
		t.Fatal(err)
	}
	value, err := ResolveOpenAIKey(context.Background(), runtimeCredentials, nil, nil, map[string]string{"OPENAI_API_KEY": "ambient-key"})
	if err != nil || value != "runtime-key" {
		t.Fatalf("Windows runtime override = %q, %v", value, err)
	}
	runtimeCredentials.RemoveAPIKey("openai")
	configured := "configured-key"
	value, err = ResolveOpenAIKey(context.Background(), runtimeCredentials, nil, &configured, map[string]string{"OPENAI_API_KEY": "ambient-key"})
	if err != nil || value != "configured-key" {
		t.Fatalf("Windows configured source = %q, %v", value, err)
	}
	value, err = ResolveOpenAIKey(context.Background(), runtimeCredentials, nil, nil, map[string]string{"OPENAI_API_KEY": "ambient-key"})
	if err != nil || value != "ambient-key" {
		t.Fatalf("Windows ambient source = %q, %v", value, err)
	}

	if err := os.MkdirAll(filepath.Dir(store.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	original := `{"openai":{"type":"api_key","key":"stored-secret"}}`
	if err := os.WriteFile(store.Path(), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, readErr := store.Read(context.Background(), "openai")
	if !errors.Is(readErr, ErrPrivateAdmissionUnavailable) || !IsKind(readErr, KindPermission) {
		t.Fatalf("existing Windows auth read error = %v", readErr)
	}
	if _, err := store.List(context.Background()); !errors.Is(err, ErrPrivateAdmissionUnavailable) || !IsKind(err, KindPermission) {
		t.Fatalf("existing Windows auth list error = %v", err)
	}
	if _, err := ResolveOpenAIKey(context.Background(), runtimeCredentials, nil, &configured, map[string]string{"OPENAI_API_KEY": "ambient-key"}); !errors.Is(err, ErrPrivateAdmissionUnavailable) {
		t.Fatalf("existing Windows file fell through to lower source: %v", err)
	}
	data, err := os.ReadFile(store.Path())
	if err != nil || string(data) != original {
		t.Fatalf("Windows failed admission changed file: %q, %v", data, err)
	}
	if strings.Contains(readErr.Error(), "stored-secret") {
		t.Fatalf("typed Windows error leaked a secret: %v", readErr)
	}
}
