package tool

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGrepPendingIOIsCancelledAndWatcherIsJoined(t *testing.T) {
	suite := newTestSuite(t)
	writeTestFile(t, suite.WorkingDir(), "slow.txt", "placeholder")
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	opened := make(chan struct{})
	suite.openSearchFile = func(string) (*os.File, int64, error) {
		close(opened)
		return reader, math.MaxInt64, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	completed := make(chan error, 1)
	go func() {
		_, err := suite.Grep(ctx, GrepInput{Pattern: "needle", Path: textPointer("slow.txt")})
		completed <- err
	}()
	<-opened
	cancel()
	select {
	case err := <-completed:
		if !errors.Is(err, ErrOperationCancelled) || !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled grep error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending grep did not return after cancellation")
	}
	// Grep cannot return until its close-on-cancel watcher has exited.
}

func TestGrepStreamsLargeRegularFileIndependentlyOfReadStringLimit(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "large.txt")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	const size = int64(8 << 20)
	chunk := []byte(strings.Repeat(" ", 32*1024))
	for written := int64(0); written < size-int64(len("TOKEN\n")); {
		remaining := size - int64(len("TOKEN\n")) - written
		if remaining < int64(len(chunk)) {
			chunk = chunk[:int(remaining)]
		}
		count, err := file.Write(chunk)
		if err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		written += int64(count)
	}
	if _, err := file.Write([]byte("TOKEN\n")); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	suite, err := NewFilesystemSuite(FilesystemOptions{WorkingDir: root, MaxReadBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	result, err := suite.Grep(context.Background(), GrepInput{Pattern: "TOKEN", Path: textPointer("large.txt")})
	if err != nil || !strings.Contains(result.Text, "large.txt:1:") || result.Details["linesTruncated"] != true {
		t.Fatalf("large streamed grep = %q, %v", result.Text, err)
	}
}

func TestReadRejectsTextBeyondConfiguredDecodedUTF16Bound(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte(strings.Repeat("x", 1025)), 0o644); err != nil {
		t.Fatal(err)
	}
	suite, err := NewFilesystemSuite(FilesystemOptions{WorkingDir: root, MaxReadBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := suite.Read(context.Background(), ReadInput{Path: "large.txt"}); !errors.Is(err, ErrFilesystemReadTooLarge) {
		t.Fatalf("read error = %v, want ErrFilesystemReadTooLarge", err)
	}
}

func TestReadTextLimitCountsDecodedUTF16UnitsInsteadOfSourceBytes(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "wide.txt", "界")   // three UTF-8 bytes, one UTF-16 unit
	writeTestFile(t, root, "astral.txt", "😀") // four UTF-8 bytes, two UTF-16 units
	oneUnit, err := NewFilesystemSuite(FilesystemOptions{WorkingDir: root, MaxTextUnits: 1})
	if err != nil {
		t.Fatal(err)
	}
	result, err := oneUnit.Read(context.Background(), ReadInput{Path: "wide.txt"})
	if err != nil || result.Text != "界" {
		t.Fatalf("one-unit wide text = %#v, %v", result, err)
	}
	if _, err := oneUnit.Read(context.Background(), ReadInput{Path: "astral.txt"}); !errors.Is(err, ErrFilesystemReadTooLarge) {
		t.Fatalf("two-unit astral text at one-unit limit = %v", err)
	}
	twoUnits, err := NewFilesystemSuite(FilesystemOptions{WorkingDir: root, MaxTextUnits: 2})
	if err != nil {
		t.Fatal(err)
	}
	result, err = twoUnits.Read(context.Background(), ReadInput{Path: "astral.txt"})
	if err != nil || result.Text != "😀" {
		t.Fatalf("two-unit astral text = %#v, %v", result, err)
	}
}

func TestReadPendingIOIsCancelledAndWatcherIsJoined(t *testing.T) {
	suite := newTestSuite(t)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	opened := make(chan struct{})
	suite.openReadFile = func(string) (*os.File, error) {
		close(opened)
		return reader, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	completed := make(chan error, 1)
	go func() {
		_, err := suite.Read(ctx, ReadInput{Path: "slow.txt"})
		completed <- err
	}()
	<-opened
	cancel()
	select {
	case err := <-completed:
		if !errors.Is(err, ErrOperationCancelled) || !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled read error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending read did not return after cancellation")
	}
	// Read cannot return until watchReadCancellation has observed cancellation
	// and exited, so this boundary also guards against a leftover watcher.
}

func TestFilesystemOptionsRejectNULWorkingDirectory(t *testing.T) {
	if _, err := NewFilesystemSuite(FilesystemOptions{WorkingDir: "bad\x00cwd"}); !errors.Is(err, ErrInvalidFilesystemOptions) {
		t.Fatalf("working directory error = %v", err)
	}
}

func TestFilesystemTextArgumentsRejectNULButPayloadsAllowIt(t *testing.T) {
	suite := newTestSuite(t)
	path := "payload.bin"
	if _, err := suite.Write(context.Background(), WriteInput{Path: path, Content: "a\x00b"}); err != nil {
		t.Fatalf("write NUL payload: %v", err)
	}
	if _, err := suite.Edit(context.Background(), EditInput{
		Path: path, Edits: []Edit{{OldText: "\x00", NewText: "\x00x"}},
	}); err != nil {
		t.Fatalf("edit NUL payload: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(suite.WorkingDir(), path))
	if err != nil || string(data) != "a\x00xb" {
		t.Fatalf("payload = %q, %v", data, err)
	}

	bad := "bad\x00value"
	cases := []struct {
		name string
		call func() error
	}{
		{name: "read path", call: func() error { _, err := suite.Read(context.Background(), ReadInput{Path: bad}); return err }},
		{name: "write path", call: func() error {
			_, err := suite.Write(context.Background(), WriteInput{Path: bad, Content: "ok"})
			return err
		}},
		{name: "edit path", call: func() error {
			_, err := suite.Edit(context.Background(), EditInput{Path: bad, Edits: []Edit{{OldText: "a", NewText: "b"}}})
			return err
		}},
		{name: "grep pattern", call: func() error { _, err := suite.Grep(context.Background(), GrepInput{Pattern: bad}); return err }},
		{name: "grep path", call: func() error {
			_, err := suite.Grep(context.Background(), GrepInput{Pattern: "x", Path: &bad})
			return err
		}},
		{name: "grep glob", call: func() error {
			_, err := suite.Grep(context.Background(), GrepInput{Pattern: "x", Glob: &bad})
			return err
		}},
		{name: "find pattern", call: func() error { _, err := suite.Find(context.Background(), FindInput{Pattern: bad}); return err }},
		{name: "find path", call: func() error {
			_, err := suite.Find(context.Background(), FindInput{Pattern: "*", Path: &bad})
			return err
		}},
		{name: "ls path", call: func() error { _, err := suite.Ls(context.Background(), LsInput{Path: &bad}); return err }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); !errors.Is(err, ErrInvalidFilesystemInput) {
				t.Fatalf("error = %v, want ErrInvalidFilesystemInput", err)
			}
		})
	}
}

func TestGrepGlobUsesRipgrepNegationAndBraceAlternatives(t *testing.T) {
	suite := newTestSuite(t)
	for name := range map[string]struct{}{
		"main.go": {}, "web.ts": {}, "web.test.ts": {}, "notes.md": {}, "!literal.txt": {},
	} {
		writeTestFile(t, suite.WorkingDir(), name, "TOKEN\n")
	}
	brace := "{*.go,*.ts}"
	result, err := suite.Grep(context.Background(), GrepInput{Pattern: "TOKEN", Glob: &brace})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"main.go:1:", "web.ts:1:", "web.test.ts:1:"} {
		if !strings.Contains(result.Text, expected) {
			t.Fatalf("brace glob omitted %q: %q", expected, result.Text)
		}
	}
	if strings.Contains(result.Text, "notes.md:1:") {
		t.Fatalf("brace glob included notes.md: %q", result.Text)
	}

	exclude := "!*.test.ts"
	result, err = suite.Grep(context.Background(), GrepInput{Pattern: "TOKEN", Glob: &exclude})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Text, "web.test.ts:1:") || !strings.Contains(result.Text, "web.ts:1:") {
		t.Fatalf("negative glob result = %q", result.Text)
	}

	literalBang := `\!*.txt`
	result, err = suite.Grep(context.Background(), GrepInput{Pattern: "TOKEN", Glob: &literalBang})
	if err != nil || !strings.Contains(result.Text, "!literal.txt:1:") {
		t.Fatalf("escaped bang glob = %q, %v", result.Text, err)
	}

	// Find uses fd semantics: ! is an ordinary leading pattern character.
	result, err = suite.Find(context.Background(), FindInput{Pattern: "!*.txt"})
	if err != nil || result.Text != "!literal.txt" {
		t.Fatalf("find leading bang = %q, %v", result.Text, err)
	}
	result, err = suite.Find(context.Background(), FindInput{Pattern: "{*.go,*.ts}"})
	if err != nil || result.Text != "main.go\nweb.test.ts\nweb.ts" {
		t.Fatalf("find brace alternatives = %q, %v", result.Text, err)
	}
}
