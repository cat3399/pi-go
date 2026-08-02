package tool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func newTestSuite(t *testing.T) *FilesystemSuite {
	t.Helper()
	suite, err := NewFilesystemSuite(FilesystemOptions{WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return suite
}
func writeTestFile(t *testing.T, root, name, contents string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
func textPointer(value string) *string { return &value }
func intPointer(value int) *int        { return &value }
func boolPointer(value bool) *bool     { return &value }

func TestFilesystemReadRangeTruncateAndBinary(t *testing.T) {
	suite := newTestSuite(t)
	writeTestFile(t, suite.WorkingDir(), "notes.txt", "one\ntwo\nthree\nfour\n")
	result, err := suite.Read(context.Background(), ReadInput{Path: "notes.txt", Offset: intPointer(2), Limit: intPointer(2)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "two\nthree") || !strings.Contains(result.Text, "2 more lines in file. Use offset=4") {
		t.Fatalf("range result = %q", result.Text)
	}
	_, err = suite.Read(context.Background(), ReadInput{Path: "notes.txt", Offset: intPointer(99)})
	if err == nil || !errors.Is(err, ErrFilesystemPath) {
		t.Fatalf("bad offset error = %v", err)
	}
	writeTestFile(t, suite.WorkingDir(), "binary.bin", "a\x00b")
	result, err = suite.Read(context.Background(), ReadInput{Path: "binary.bin"})
	if !errors.Is(err, ErrBinaryFile) || !strings.Contains(result.Text, "binary file") {
		t.Fatalf("binary result = %#v, %v", result, err)
	}
	small, err := NewFilesystemSuite(FilesystemOptions{WorkingDir: suite.WorkingDir(), MaxLines: 2, MaxBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	result, err = small.Read(context.Background(), ReadInput{Path: "notes.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "Showing lines 1-2 of 5. Use offset=3") {
		t.Fatalf("truncation = %q", result.Text)
	}
}

func TestFilesystemPathPolicy(t *testing.T) {
	suite := newTestSuite(t)
	writeTestFile(t, suite.WorkingDir(), "space name.txt", "ok")
	result, err := suite.Read(context.Background(), ReadInput{Path: "@space\u00a0name.txt"})
	if err != nil || result.Text != "ok" {
		t.Fatalf("ergonomic path = %#v, %v", result, err)
	}
	curly := "Capture d’écran.txt"
	writeTestFile(t, suite.WorkingDir(), curly, "image name")
	result, err = suite.Read(context.Background(), ReadInput{Path: "Capture d'écran.txt"})
	if err != nil || result.Text != "image name" {
		t.Fatalf("curly fallback = %#v, %v", result, err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("allowed"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err = suite.Read(context.Background(), ReadInput{Path: outside})
	if err != nil || result.Text != "allowed" {
		t.Fatalf("absolute path must retain upstream behavior: %#v %v", result, err)
	}
}

func TestFilesystemWriteIsAtomicAndCancellationIsSafe(t *testing.T) {
	suite := newTestSuite(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := suite.Write(ctx, WriteInput{Path: "new/file.txt", Content: "no"})
	if err == nil || !errors.Is(err, context.Canceled) || result.Text != "Filesystem operation cancelled" {
		t.Fatalf("cancel = %#v %v", result, err)
	}
	if _, statErr := os.Stat(filepath.Join(suite.WorkingDir(), "new/file.txt")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("cancel created file: %v", statErr)
	}
	result, err = suite.Write(context.Background(), WriteInput{Path: "new/file.txt", Content: "hello 世界"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "12 bytes") {
		t.Fatalf("write result = %q", result.Text)
	}
	data, err := os.ReadFile(filepath.Join(suite.WorkingDir(), "new/file.txt"))
	if err != nil || string(data) != "hello 世界" {
		t.Fatalf("write data = %q, %v", data, err)
	}
}

func TestMutationQueueSerializesAliasesAndHonorsQueuedCancellation(t *testing.T) {
	queue := newMutationQueue()
	firstStarted := make(chan struct{})
	release := make(chan struct{})
	secondStarted := make(chan struct{})
	var order []string
	var mu sync.Mutex
	first := make(chan error, 1)
	go func() {
		first <- queue.with(context.Background(), "same", func() error {
			mu.Lock()
			order = append(order, "first:start")
			mu.Unlock()
			close(firstStarted)
			<-release
			mu.Lock()
			order = append(order, "first:end")
			mu.Unlock()
			return nil
		})
	}()
	<-firstStarted
	ctx, cancel := context.WithCancel(context.Background())
	second := make(chan error, 1)
	go func() { second <- queue.with(ctx, "same", func() error { close(secondStarted); return nil }) }()
	cancel()
	if err := <-second; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued cancellation = %v", err)
	}
	select {
	case <-secondStarted:
		t.Fatal("cancelled queued operation started")
	default:
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(order, ",") != "first:start,first:end" {
		t.Fatalf("order = %#v", order)
	}
}

func TestFilesystemEditOriginalSnapshotCRLFBOMAndConflict(t *testing.T) {
	suite := newTestSuite(t)
	path := writeTestFile(t, suite.WorkingDir(), "edit.txt", "\ufeffalpha\r\nbeta\r\ngamma\r\n")
	result, err := suite.Edit(context.Background(), EditInput{Path: "edit.txt", Edits: []Edit{{OldText: "alpha\n", NewText: "ALPHA\n"}, {OldText: "gamma\n", NewText: "GAMMA\n"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "2 block") || result.Details["patch"] == "" {
		t.Fatalf("edit result = %#v", result)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "\ufeffALPHA\r\nbeta\r\nGAMMA\r\n" {
		t.Fatalf("edited = %q", data)
	}
	before := string(data)
	_, err = suite.Edit(context.Background(), EditInput{Path: "edit.txt", Edits: []Edit{{OldText: "ALPHA", NewText: "a"}, {OldText: "missing", NewText: "x"}}})
	if !errors.Is(err, ErrEditConflict) {
		t.Fatalf("partial conflict error = %v", err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != before {
		t.Fatalf("failed multi edit changed file = %q", data)
	}
	_, err = suite.Edit(context.Background(), EditInput{Path: "edit.txt", Edits: []Edit{{OldText: "ALPHA\r\nbeta", NewText: "x"}, {OldText: "beta\r\nGAMMA", NewText: "y"}}})
	if !errors.Is(err, ErrEditConflict) {
		t.Fatalf("overlap = %v", err)
	}
}

func TestFilesystemEditFuzzyAndUnique(t *testing.T) {
	suite := newTestSuite(t)
	path := writeTestFile(t, suite.WorkingDir(), "fuzzy.txt", "console.log(‘hello’);\nhello\u00a0world\n")
	_, err := suite.Edit(context.Background(), EditInput{Path: "fuzzy.txt", Edits: []Edit{{OldText: "console.log('hello');", NewText: "console.log('world');"}}})
	if err != nil {
		t.Fatalf("smart quote fuzzy = %v", err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "world") {
		t.Fatalf("fuzzy content = %q", data)
	}
	writeTestFile(t, suite.WorkingDir(), "duplicates.txt", "same\nsame\n")
	_, err = suite.Edit(context.Background(), EditInput{Path: "duplicates.txt", Edits: []Edit{{OldText: "same", NewText: "other"}}})
	if !errors.Is(err, ErrEditConflict) {
		t.Fatalf("duplicate = %v", err)
	}
	writeTestFile(t, suite.WorkingDir(), "compatibility.txt", "ＡＢＣ\n")
	_, err = suite.Edit(context.Background(), EditInput{Path: "compatibility.txt", Edits: []Edit{{OldText: "ABC", NewText: "XYZ"}}})
	if !errors.Is(err, ErrUnsupportedFilesystemFeature) {
		t.Fatalf("NFKC gap must fail explicitly: %v", err)
	}
}

func TestFilesystemFindGrepLsDeterministic(t *testing.T) {
	suite := newTestSuite(t)
	writeTestFile(t, suite.WorkingDir(), "z.txt", "zero\nNeedle value\nlast\n")
	writeTestFile(t, suite.WorkingDir(), "a.txt", "needle lower\n")
	writeTestFile(t, suite.WorkingDir(), ".hidden/secret.txt", "Needle hidden\n")
	writeTestFile(t, suite.WorkingDir(), "ignored.txt", "Needle ignored\n")
	writeTestFile(t, suite.WorkingDir(), ".gitignore", "ignored.txt\n")
	result, err := suite.Find(context.Background(), FindInput{Pattern: "**/*.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != ".hidden/secret.txt\na.txt\nz.txt" {
		t.Fatalf("find = %q", result.Text)
	}
	result, err = suite.Grep(context.Background(), GrepInput{Pattern: "needle", Path: textPointer("."), IgnoreCase: boolPointer(true), Context: intPointer(1), Limit: intPointer(2)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "a.txt:1: needle lower") || !strings.Contains(result.Text, ".hidden/secret.txt:1: Needle hidden") || strings.Contains(result.Text, "ignored") {
		t.Fatalf("grep = %q", result.Text)
	}
	result, err = suite.Ls(context.Background(), LsInput{})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(result.Text, "\n")
	if len(lines) < 3 || lines[0] != ".gitignore" || lines[1] != ".hidden/" {
		t.Fatalf("ls sort = %#v", lines)
	}
}

func TestFilesystemRegistryStrictDecodeAndDispatch(t *testing.T) {
	suite := newTestSuite(t)
	registry, err := NewFilesystemRegistry(suite)
	if err != nil {
		t.Fatal(err)
	}
	_, err = registry.ExecuteJSON(context.Background(), WriteToolName, []byte(`{"path":"a.txt","content":"x","content":"y"}`))
	if !errors.Is(err, ErrInvalidFilesystemInput) {
		t.Fatalf("duplicate decode = %v", err)
	}
	result, err := registry.ExecuteJSON(context.Background(), WriteToolName, []byte(`{"path":"a.txt","content":"x"}`))
	if err != nil || !strings.Contains(result.Text, "Successfully wrote") {
		t.Fatalf("registry write = %#v %v", result, err)
	}
	result, err = registry.ExecuteJSON(context.Background(), "unknown", []byte(`{}`))
	if !errors.Is(err, ErrFilesystemToolNotFound) || result.Text != "Tool unknown not found" {
		t.Fatalf("unknown = %#v %v", result, err)
	}
}

func TestFilesystemOperationCancellationDuringWalk(t *testing.T) {
	suite := newTestSuite(t)
	for index := 0; index < 100; index++ {
		writeTestFile(t, suite.WorkingDir(), filepath.Join("many", strings.Repeat("x", index%10), fmt.Sprintf("%03d.txt", index)), "data")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := suite.Find(ctx, FindInput{Pattern: "**"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("walk cancel = %v", err)
	}
}

func FuzzFilesystemJSONInputs(f *testing.F) {
	f.Add([]byte(`{"path":"a","content":"b"}`))
	f.Add([]byte(`{"path":1}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		suite := newTestSuite(t)
		_, _ = suite.ExecuteJSON(context.Background(), WriteToolName, raw)
		_, _ = suite.ExecuteJSON(context.Background(), EditToolName, raw)
		_, _ = suite.ExecuteJSON(context.Background(), GrepToolName, raw)
	})
}

func FuzzApplyEdits(f *testing.F) {
	f.Add("alpha\nbeta\n", "beta", "BETA")
	f.Fuzz(func(t *testing.T, content, oldText, newText string) {
		if !utf8.ValidString(content) || !utf8.ValidString(oldText) || !utf8.ValidString(newText) || oldText == "" {
			t.Skip()
		}
		result, _, _, err := applyEdits(content, []Edit{{OldText: oldText, NewText: newText}}, "fuzz")
		if err == nil && !utf8.ValidString(result) {
			t.Fatalf("invalid UTF-8 output")
		}
	})
}

func TestMutationQueueParallelKeys(t *testing.T) {
	queue := newMutationQueue()
	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	run := func(key string) chan error {
		done := make(chan error, 1)
		go func() {
			done <- queue.with(context.Background(), key, func() error { ready <- struct{}{}; <-release; return nil })
		}()
		return done
	}
	left, right := run("left"), run("right")
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("left did not start")
	}
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("right key blocked")
	}
	close(release)
	if err := <-left; err != nil {
		t.Fatal(err)
	}
	if err := <-right; err != nil {
		t.Fatal(err)
	}
}
