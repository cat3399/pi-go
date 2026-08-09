package auth

import (
	"bytes"
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

func requirePersistentAuth(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("v0.1 persistent auth is intentionally fail-closed on Windows")
	}
}

func TestStoreRoundTripPreservesUnknownProviderAndPermissions(t *testing.T) {
	requirePersistentAuth(t)
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

func TestStoreMalformedFilesAreNeverOverwrittenAndOrdinaryModesAreAccepted(t *testing.T) {
	requirePersistentAuth(t)
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
	if err := os.WriteFile(store.Path(), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(store.Path(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAPIKey(context.Background(), "openai", "new-key", nil); err != nil {
		t.Fatalf("ordinary auth.json mode: %v", err)
	}
	credential, exists, err := store.Read(context.Background(), "openai")
	if err != nil || !exists || credential.Key != "new-key" {
		t.Fatalf("credential after ordinary-mode update = %#v, %t, %v", credential, exists, err)
	}
}

func TestStoreFailureBeforeRenamePreservesOriginalAndCleansTemporary(t *testing.T) {
	requirePersistentAuth(t)
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
	requirePersistentAuth(t)
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
	for index, input := range []struct{ provider, key string }{{"openai", "one"}, {"anthropic", "two"}} {
		input := input
		writer := first
		if index == 1 {
			writer = second
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := writer.SetAPIKey(context.Background(), input.provider, input.key, nil); err != nil {
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

func TestStoreSameInstanceWaitIsContextAwareAndReleasesAfterCancellation(t *testing.T) {
	requirePersistentAuth(t)
	directory := t.TempDir()
	path := filepath.Join(directory, "auth.json")
	if err := os.Mkdir(path+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	enteredFileWait := make(chan struct{})
	var once sync.Once
	store, err := NewStore(Options{
		Path: path,
		LockPoll: func(ctx context.Context) error {
			once.Do(func() { close(enteredFileWait) })
			<-ctx.Done()
			return context.Cause(ctx)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() { firstDone <- store.SetAPIKey(firstCtx, "openai", "first", nil) }()
	select {
	case <-enteredFileWait:
	case <-time.After(time.Second):
		t.Fatal("first mutation did not reach the file lock")
	}

	secondCtx, cancelSecond := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancelSecond()
	started := time.Now()
	err = store.SetAPIKey(secondCtx, "anthropic", "second", nil)
	if !IsKind(err, KindCancelled) {
		t.Fatalf("same-Store wait error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("same-Store cancellation took %s", elapsed)
	}

	cancelFirst()
	if err := <-firstDone; !IsKind(err, KindCancelled) {
		t.Fatalf("first mutation error = %v", err)
	}
	if err := os.Remove(path + ".lock"); err != nil {
		t.Fatal(err)
	}
	store.lockPoll = nil
	if err := store.SetAPIKey(context.Background(), "google", "after-cancel", nil); err != nil {
		t.Fatalf("local/file locks were not released: %v", err)
	}
}

func TestSeparateProcessWritersContendMergeAndRelease(t *testing.T) {
	requirePersistentAuth(t)
	directory := t.TempDir()
	path := filepath.Join(directory, "auth.json")
	marker := filepath.Join(directory, "first-holds-lock")
	releaseMarker := filepath.Join(directory, "release-first")
	contendMarker := filepath.Join(directory, "second-observed-contention")
	first := authHelperCommand(path, "write", "openai", "first-key", marker)
	second := authHelperCommand(path, "write", "anthropic", "second-key", "")
	first.Env = append(first.Env, "PI_GO_AUTH_RELEASE_MARKER="+releaseMarker)
	second.Env = append(second.Env, "PI_GO_AUTH_CONTEND_MARKER="+contendMarker)
	var firstOutput bytes.Buffer
	var secondOutput bytes.Buffer
	first.Stdout, first.Stderr = &firstOutput, &firstOutput
	second.Stdout, second.Stderr = &secondOutput, &secondOutput
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	if err := waitForTestPath(marker, time.Second); err != nil {
		_ = first.Process.Kill()
		_ = first.Wait()
		t.Fatal(err)
	}
	if err := second.Start(); err != nil {
		_ = first.Process.Kill()
		_ = first.Wait()
		t.Fatal(err)
	}
	if err := waitForTestPath(contendMarker, time.Second); err != nil {
		_ = first.Process.Kill()
		_ = second.Process.Kill()
		_ = first.Wait()
		_ = second.Wait()
		t.Fatal(err)
	}
	if err := os.WriteFile(releaseMarker, []byte("release"), 0o600); err != nil {
		_ = first.Process.Kill()
		_ = second.Process.Kill()
		_ = first.Wait()
		_ = second.Wait()
		t.Fatal(err)
	}
	if err := first.Wait(); err != nil {
		t.Fatalf("first writer: %v: %s", err, firstOutput.String())
	}
	if err := second.Wait(); err != nil {
		t.Fatalf("second writer: %v: %s", err, secondOutput.String())
	}
	store, err := NewStore(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	for provider, key := range map[string]string{"openai": "first-key", "anthropic": "second-key"} {
		credential, exists, err := store.Read(context.Background(), provider)
		if err != nil || !exists || credential.Key != key {
			t.Fatalf("merged %s = %#v, %t, %v", provider, credential, exists, err)
		}
	}
	if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("successful child writers left lock behind: %v", err)
	}
}

func TestSeparateProcessFailureReleasesForHealthyWriter(t *testing.T) {
	requirePersistentAuth(t)
	path := filepath.Join(t.TempDir(), "auth.json")
	failed := authHelperCommand(path, "fail", "", "", "")
	if output, err := failed.CombinedOutput(); err != nil {
		t.Fatalf("expected child failure path: %v: %s", err, output)
	}
	if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("failed child left lock behind: %v", err)
	}
	healthy := authHelperCommand(path, "write", "openai", "healthy-key", "")
	if output, err := healthy.CombinedOutput(); err != nil {
		t.Fatalf("healthy writer after failure: %v: %s", err, output)
	}
	store, err := NewStore(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	credential, exists, err := store.Read(context.Background(), "openai")
	if err != nil || !exists || credential.Key != "healthy-key" {
		t.Fatalf("healthy post-failure credential = %#v, %t, %v", credential, exists, err)
	}
	if _, exists, err := store.Read(context.Background(), "failed-child"); err != nil || exists {
		t.Fatalf("failed child was committed = %t, %v", exists, err)
	}
}

func authHelperCommand(path, mode, provider, key, marker string) *exec.Cmd {
	command := exec.Command(os.Args[0], "-test.run=TestAuthProcessHelper")
	command.Env = append(
		os.Environ(),
		"PI_GO_AUTH_HELPER=1",
		"PI_GO_AUTH_PATH="+path,
		"PI_GO_AUTH_HELPER_MODE="+mode,
		"PI_GO_AUTH_PROVIDER="+provider,
		"PI_GO_AUTH_KEY="+key,
		"PI_GO_AUTH_HOLD_MARKER="+marker,
	)
	return command
}

func waitForTestPath(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for child lock marker")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAuthProcessHelper(t *testing.T) {
	if os.Getenv("PI_GO_AUTH_HELPER") != "1" {
		return
	}
	options := Options{Path: os.Getenv("PI_GO_AUTH_PATH")}
	if marker := os.Getenv("PI_GO_AUTH_CONTEND_MARKER"); marker != "" {
		options.LockPoll = func(ctx context.Context) error {
			if err := os.WriteFile(marker, []byte("contended"), 0o600); err != nil {
				return err
			}
			timer := time.NewTimer(10 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return context.Cause(ctx)
			case <-timer.C:
				return nil
			}
		}
	}
	store, err := NewStore(options)
	if err != nil {
		os.Exit(11)
	}
	switch os.Getenv("PI_GO_AUTH_HELPER_MODE") {
	case "write":
		if marker := os.Getenv("PI_GO_AUTH_HOLD_MARKER"); marker != "" {
			store.beforeRename = func() error {
				if err := os.WriteFile(marker, []byte("locked"), 0o600); err != nil {
					return err
				}
				if release := os.Getenv("PI_GO_AUTH_RELEASE_MARKER"); release != "" {
					return waitForTestPath(release, 2*time.Second)
				}
				return nil
			}
		}
		if err := store.SetAPIKey(
			context.Background(),
			os.Getenv("PI_GO_AUTH_PROVIDER"),
			os.Getenv("PI_GO_AUTH_KEY"),
			nil,
		); err != nil {
			os.Exit(13)
		}
	case "fail":
		store.beforeRename = func() error { return errors.New("injected child write failure") }
		if !IsKind(store.SetAPIKey(context.Background(), "failed-child", "failed-key", nil), KindIO) {
			os.Exit(14)
		}
	default:
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel()
		if !IsKind(store.SetAPIKey(ctx, "helper", "key", nil), KindCancelled) {
			os.Exit(12)
		}
	}
	os.Exit(0)
}

func TestRuntimeOverlayDoesNotPersistAndDeleteClearsBoth(t *testing.T) {
	requirePersistentAuth(t)
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
