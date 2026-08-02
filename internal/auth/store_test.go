package auth

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(Options{Path: filepath.Join(t.TempDir(), "nested", "auth.json")})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestStoreRoundTripPreservesUnknownProviderAndPermissions(t *testing.T) {
	store := newTestStore(t)
	if err := os.MkdirAll(filepath.Dir(store.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	original := "{\n  \"future\": { \"type\": \"future\", \"secret\": [1, {\"x\": true}] },\n  \"openai\": {\"type\":\"api_key\",\"key\":\"old\"}\n}\n"
	if err := os.WriteFile(store.Path(), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAPIKey(context.Background(), "openai", "new-key", map[string]string{"REGION": "test"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"future"`) || !strings.Contains(string(data), `"secret"`) {
		t.Fatalf("unknown raw provider changed: %s", data)
	}
	credential, exists, err := store.Read(context.Background(), "openai")
	if err != nil || !exists || credential.Key != "new-key" || credential.Env["REGION"] != "test" {
		t.Fatalf("Read() = %#v, %t, %v", credential, exists, err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(store.Path())
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %#o, want 0600", info.Mode().Perm())
		}
	}
}

func TestStoreMalformedAndUnsafeFilesAreNeverOverwritten(t *testing.T) {
	store := newTestStore(t)
	if err := os.MkdirAll(filepath.Dir(store.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"{not-json", `{"openai":{"type":"api_key","key":"a","key":"b"}}`, `[]`, string([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'})} {
		t.Run(content[:1], func(t *testing.T) {
			if err := os.WriteFile(store.Path(), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			err := store.SetAPIKey(context.Background(), "anthropic", "new-key", nil)
			if !IsKind(err, KindMalformed) {
				t.Fatalf("SetAPIKey() error = %v", err)
			}
			got, readErr := os.ReadFile(store.Path())
			if readErr != nil || string(got) != content {
				t.Fatalf("file = %q, %v", got, readErr)
			}
		})
	}
	if runtime.GOOS != "windows" {
		if err := os.WriteFile(store.Path(), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(store.Path(), 0o644); err != nil {
			t.Fatal(err)
		}
		err := store.SetAPIKey(context.Background(), "openai", "new-key", nil)
		if !IsKind(err, KindPermission) {
			t.Fatalf("unsafe mode error = %v", err)
		}
		data, _ := os.ReadFile(store.Path())
		if string(data) != `{}` {
			t.Fatalf("unsafe file changed: %q", data)
		}
	}
}

func TestStoreFailureBeforeRenamePreservesOriginalAndCleansTemporary(t *testing.T) {
	store := newTestStore(t)
	if err := os.MkdirAll(filepath.Dir(store.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path(), []byte(`{"openai":{"type":"api_key","key":"old"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store.beforeRename = func() error { return errors.New("injected replacement failure") }
	err := store.SetAPIKey(context.Background(), "openai", "new-key", nil)
	if !IsKind(err, KindIO) {
		t.Fatalf("SetAPIKey() error = %v", err)
	}
	data, _ := os.ReadFile(store.Path())
	if strings.Contains(string(data), "new-key") {
		t.Fatalf("replacement changed original: %q", data)
	}
	entries, err := os.ReadDir(filepath.Dir(store.Path()))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".auth.json-") {
			t.Fatalf("orphan temp file %q", entry.Name())
		}
	}
}

func TestStoreSerializesConcurrentStoresAndLockContentionIsCancellable(t *testing.T) {
	directory := t.TempDir()
	first, err := NewStore(Options{Path: filepath.Join(directory, "auth.json")})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewStore(Options{Path: filepath.Join(directory, "auth.json")})
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	for _, input := range []struct{ provider, key string }{{"openai", "one"}, {"anthropic", "two"}} {
		input := input
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := first.SetAPIKey(context.Background(), input.provider, input.key, nil); err != nil {
				t.Errorf("write %s: %v", input.provider, err)
			}
		}()
	}
	wait.Wait()
	for _, provider := range []string{"openai", "anthropic"} {
		if _, exists, err := second.Read(context.Background(), provider); err != nil || !exists {
			t.Fatalf("%s = exists %t, err %v", provider, exists, err)
		}
	}
	if err := os.Mkdir(first.Path()+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(first.Path() + ".lock")
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	err = second.SetAPIKey(ctx, "google", "three", nil)
	if !IsKind(err, KindCancelled) {
		t.Fatalf("contention error = %v", err)
	}
	command := exec.Command(os.Args[0], "-test.run=TestAuthProcessHelper")
	command.Env = append(os.Environ(), "PI_GO_AUTH_HELPER=1", "PI_GO_AUTH_PATH="+first.Path())
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("separate-process lock behavior: %v: %s", err, output)
	}
}

func TestAuthProcessHelper(t *testing.T) {
	if os.Getenv("PI_GO_AUTH_HELPER") != "1" {
		return
	}
	store, err := NewStore(Options{Path: os.Getenv("PI_GO_AUTH_PATH")})
	if err != nil {
		os.Exit(11)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if !IsKind(store.SetAPIKey(ctx, "helper", "key", nil), KindCancelled) {
		os.Exit(12)
	}
	os.Exit(0)
}

func TestRuntimeOverlayDoesNotPersistAndDeleteClearsBoth(t *testing.T) {
	store := newTestStore(t)
	if err := store.SetAPIKey(context.Background(), "openai", "stored", nil); err != nil {
		t.Fatal(err)
	}
	runtime := NewRuntime(store)
	if err := runtime.SetAPIKey("openai", "runtime"); err != nil {
		t.Fatal(err)
	}
	credential, _, err := runtime.Read(context.Background(), "openai")
	if err != nil || credential.Key != "runtime" {
		t.Fatalf("runtime = %#v, %v", credential, err)
	}
	stored, _, err := store.Read(context.Background(), "openai")
	if err != nil || stored.Key != "stored" {
		t.Fatalf("stored = %#v, %v", stored, err)
	}
	if err := runtime.Delete(context.Background(), "openai"); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := runtime.Read(context.Background(), "openai"); err != nil || exists {
		t.Fatalf("post delete = %t, %v", exists, err)
	}
}
