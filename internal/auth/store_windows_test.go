//go:build windows

package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// This suite runs only on a real Windows Go test runner. Windows uses the same
// auth.json read/write path as every other platform.
func TestWindowsPersistentAuthRoundTripAndExistingFileAdmission(t *testing.T) {
	store := newTestStore(t)
	if _, exists, err := store.Read(context.Background(), "openai"); err != nil || exists {
		t.Fatalf("missing Windows auth file = exists %t, error %v", exists, err)
	}
	if err := os.MkdirAll(filepath.Dir(store.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAPIKey(context.Background(), "openai", "persistent-secret", nil); err != nil {
		t.Fatalf("Windows persistent set error = %v", err)
	}
	credential, exists, err := store.Read(context.Background(), "openai")
	if err != nil || !exists || credential.Key != "persistent-secret" {
		t.Fatalf("Windows persistent read = %#v, %t, %v", credential, exists, err)
	}
	// Replacing an existing auth.json must work, not only creating it.
	if err := store.SetAPIKey(context.Background(), "openai", "replaced-secret", nil); err != nil {
		t.Fatalf("Windows persistent replace error = %v", err)
	}
	configured := "configured-key"
	value, err := ResolveOpenAIKey(context.Background(), NewRuntime(store), nil, &configured, map[string]string{"OPENAI_API_KEY": "ambient-key"})
	if err != nil || value != "replaced-secret" {
		t.Fatalf("Windows stored source = %q, %v", value, err)
	}
	if err := store.Delete(context.Background(), "openai"); err != nil {
		t.Fatalf("Windows persistent delete error = %v", err)
	}
	if _, exists, err := store.Read(context.Background(), "openai"); err != nil || exists {
		t.Fatalf("deleted Windows credential = exists %t, error %v", exists, err)
	}

	original := `{"openai":{"type":"api_key","key":"stored-secret"}}`
	if err := os.WriteFile(store.Path(), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	credential, exists, err = store.Read(context.Background(), "openai")
	if err != nil || !exists || credential.Key != "stored-secret" {
		t.Fatalf("existing Windows auth read = %#v, %t, %v", credential, exists, err)
	}
	if infos, err := store.List(context.Background()); err != nil || len(infos) != 1 {
		t.Fatalf("existing Windows auth list = %#v, %v", infos, err)
	}
}
