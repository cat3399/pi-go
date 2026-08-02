package auth

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveValueTemplatesAndCommandRejectionAreSecretSafe(t *testing.T) {
	value, err := ResolveValue(context.Background(), "prefix-${SCOPED}-$$-$!-$AMBIENT", "test key", map[string]string{"SCOPED": "left"}, map[string]string{"SCOPED": "wrong", "AMBIENT": "right"})
	if err != nil || value != "prefix-left-$-!-right" {
		t.Fatalf("ResolveValue() = %q, %v", value, err)
	}
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	_, err = ResolveValue(context.Background(), "!sh -c 'touch "+marker+"; printf super-secret'", "test key", nil, nil)
	if !IsKind(err, KindUnsupported) || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("command error = %v", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("rejected command ran: %v", statErr)
	}
	_, err = ResolveValue(context.Background(), "${MISSING}", "test key", nil, nil)
	if !IsKind(err, KindNotConfigured) {
		t.Fatalf("missing variable error = %v", err)
	}
}

func TestResolveOpenAIKeyPrecedenceAndStoredOwnership(t *testing.T) {
	requirePersistentAuth(t)
	directory := t.TempDir()
	store, err := NewStore(Options{Path: filepath.Join(directory, "auth.json")})
	if err != nil {
		t.Fatal(err)
	}
	runtime := NewRuntime(store)
	if err := store.SetAPIKey(context.Background(), "openai", "stored-${KEY}", map[string]string{"KEY": "scoped"}); err != nil {
		t.Fatal(err)
	}
	explicit := "cli"
	configured := "configured"
	value, err := ResolveOpenAIKey(context.Background(), runtime, &explicit, &configured, map[string]string{"OPENAI_API_KEY": "ambient"})
	if err != nil || value != "cli" {
		t.Fatalf("CLI = %q, %v", value, err)
	}
	if err := runtime.SetAPIKey("openai", "runtime-key"); err != nil {
		t.Fatal(err)
	}
	value, err = ResolveOpenAIKey(context.Background(), runtime, nil, &configured, map[string]string{"OPENAI_API_KEY": "ambient"})
	if err != nil || value != "runtime-key" {
		t.Fatalf("runtime = %q, %v", value, err)
	}
	runtime.RemoveAPIKey("openai")
	value, err = ResolveOpenAIKey(context.Background(), runtime, nil, &configured, map[string]string{"OPENAI_API_KEY": "ambient"})
	if err != nil || value != "stored-scoped" {
		t.Fatalf("stored = %q, %v", value, err)
	}
	if err := os.WriteFile(store.Path(), []byte(`{"openai":{"type":"oauth","access":"secret"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = ResolveOpenAIKey(context.Background(), runtime, nil, &configured, map[string]string{"OPENAI_API_KEY": "ambient"})
	if !IsKind(err, KindUnsupported) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("OAuth ownership error = %v", err)
	}
}

func FuzzResolveValueNeverLeaksInput(f *testing.F) {
	f.Add("${MISSING}")
	f.Add("!printf secret")
	f.Fuzz(func(t *testing.T, raw string) {
		_, err := ResolveValue(context.Background(), raw, "fuzz value", map[string]string{"KNOWN": "safe"}, nil)
		if err != nil && strings.Contains(raw, "never-leak-token") && strings.Contains(err.Error(), "never-leak-token") {
			t.Fatalf("error leaked marked input: %v", err)
		}
	})
}
