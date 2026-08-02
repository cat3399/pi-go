package tool

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestOutputAccumulatorStreamingUTF8AndTrailingNewline(t *testing.T) {
	t.Parallel()
	accumulator := newTestAccumulator(t, 2000, 51200)
	euro := []byte("€\n")
	if err := accumulator.append(euro[:1]); err != nil {
		t.Fatal(err)
	}
	if err := accumulator.append(euro[1:]); err != nil {
		t.Fatal(err)
	}
	snapshot := finishTestAccumulator(t, accumulator)
	if snapshot.content != "€\n" {
		t.Fatalf("content = %q", snapshot.content)
	}
	if snapshot.truncation.totalLines != 1 || snapshot.truncation.totalBytes != 4 {
		t.Fatalf("truncation metadata = %#v", snapshot.truncation)
	}
	if snapshot.truncation.truncated {
		t.Fatal("short output was truncated")
	}
}

func TestOutputAccumulatorBOMAndInvalidUTF8(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("owner-only artifact ACL adapter intentionally fails closed on Windows")
	}
	t.Parallel()
	accumulator := newTestAccumulator(t, 10, 100)
	raw := append([]byte{0xef, 0xbb, 0xbf}, []byte("x")...)
	raw = append(raw, 0xff, 0xe2, 0x82)
	if err := accumulator.append(raw[:2]); err != nil {
		t.Fatal(err)
	}
	if err := accumulator.append(raw[2:5]); err != nil {
		t.Fatal(err)
	}
	if err := accumulator.append(raw[5:]); err != nil {
		t.Fatal(err)
	}
	snapshot := finishTestAccumulator(t, accumulator)
	if !utf8.ValidString(snapshot.content) {
		t.Fatalf("display content is not valid UTF-8: %x", snapshot.content)
	}
	if strings.HasPrefix(snapshot.content, "\ufeff") {
		t.Fatalf("display retained UTF-8 BOM: %q", snapshot.content)
	}
	if !strings.Contains(snapshot.content, "x��") {
		t.Fatalf("invalid UTF-8 replacement differs: %q", snapshot.content)
	}
	artifactAccumulator := newTestAccumulator(t, 10, 4)
	if err := artifactAccumulator.append(raw); err != nil {
		t.Fatal(err)
	}
	artifactSnapshot := finishTestAccumulator(t, artifactAccumulator)
	full, err := os.ReadFile(artifactSnapshot.fullOutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(full, raw) {
		t.Fatalf("artifact bytes = %x, want %x", full, raw)
	}
}

func TestOutputAccumulatorLineTailArtifactAndModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("owner-only artifact ACL adapter intentionally fails closed on Windows")
	}
	t.Parallel()
	parent := t.TempDir()
	root := filepath.Join(parent, "private")
	store, err := newArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	accumulator, err := newOutputAccumulator(2, 100, store)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("1\n2\n3\n")
	if err := accumulator.append(raw); err != nil {
		t.Fatal(err)
	}
	snapshot := finishTestAccumulator(t, accumulator)
	if snapshot.content != "2\n3" {
		t.Fatalf("content = %q, want tail", snapshot.content)
	}
	truncation := snapshot.truncation
	if !truncation.truncated ||
		truncation.truncatedBy != TruncatedByLines ||
		truncation.totalLines != 3 ||
		truncation.outputLines != 2 {
		t.Fatalf("truncation = %#v", truncation)
	}
	if !snapshot.artifactComplete || !filepath.IsAbs(snapshot.fullOutputPath) {
		t.Fatalf("artifact not complete/absolute: %#v", snapshot)
	}
	full, err := os.ReadFile(snapshot.fullOutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(full, raw) {
		t.Fatalf("artifact = %q, want %q", full, raw)
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	fileInfo, err := os.Stat(snapshot.fullOutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if rootInfo.Mode().Perm() != 0o700 || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("modes = dir %04o file %04o", rootInfo.Mode().Perm(), fileInfo.Mode().Perm())
	}
}

func TestOutputAccumulatorByteTailCases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		content     string
		maxBytes    int
		want        string
		wantPartial bool
	}{
		{name: "complete lines", content: "aa\nbb\ncc", maxBytes: 5, want: "bb\ncc"},
		{name: "long final line", content: "prefix\n€€€", maxBytes: 5, want: "€", wantPartial: true},
		{name: "long earlier line", content: "123456789\nok", maxBytes: 5, want: "ok"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			accumulator := newTestAccumulator(t, 100, test.maxBytes)
			if err := accumulator.append([]byte(test.content)); err != nil {
				if runtime.GOOS == "windows" && errors.Is(err, ErrArtifactSecurity) {
					t.Skip("Windows artifact creation fails closed")
				}
				t.Fatal(err)
			}
			snapshot := finishTestAccumulator(t, accumulator)
			if snapshot.content != test.want {
				t.Fatalf("content = %q, want %q", snapshot.content, test.want)
			}
			if snapshot.truncation.truncatedBy != TruncatedByBytes ||
				snapshot.truncation.lastLinePartial != test.wantPartial {
				t.Fatalf("truncation = %#v", snapshot.truncation)
			}
		})
	}
}

func TestOutputAccumulatorTrailingNewlineRetainsLastLineSize(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("owner-only artifact ACL adapter intentionally fails closed on Windows")
	}
	t.Parallel()
	accumulator := newTestAccumulator(t, 100, 4)
	if err := accumulator.append([]byte("abcdefgh\n")); err != nil {
		t.Fatal(err)
	}
	snapshot := finishTestAccumulator(t, accumulator)
	if !snapshot.truncation.lastLinePartial || snapshot.lastLineBytes != 8 {
		t.Fatalf("last-line metadata = partial:%v bytes:%d, want true/8", snapshot.truncation.lastLinePartial, snapshot.lastLineBytes)
	}
	footer := truncationFooter(snapshot, snapshot.fullOutputPath)
	if !strings.Contains(footer, "line is 8B") {
		t.Fatalf("footer = %q", footer)
	}
}

type faultArtifactFile struct {
	bytes.Buffer
	failAfter int
	writeErr  error
	closeErr  error
	closed    bool
}

func (file *faultArtifactFile) Write(data []byte) (int, error) {
	if file.failAfter >= 0 {
		remaining := file.failAfter - file.Len()
		if remaining <= 0 {
			return 0, file.writeErr
		}
		if remaining < len(data) {
			written, _ := file.Buffer.Write(data[:remaining])
			return written, file.writeErr
		}
	}
	return file.Buffer.Write(data)
}

func (file *faultArtifactFile) Close() error {
	file.closed = true
	return file.closeErr
}

type faultArtifactFactory struct {
	file    *faultArtifactFile
	path    string
	creates int
}

func (factory *faultArtifactFactory) create() (artifactFile, string, error) {
	factory.creates++
	return factory.file, factory.path, nil
}

func TestBashArtifactWriteAndCloseFailuresRemainTypedAndPrivate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		file *faultArtifactFile
	}{
		{
			name: "write",
			file: &faultArtifactFile{failAfter: 3, writeErr: errors.New("injected write failure")},
		},
		{
			name: "close",
			file: &faultArtifactFile{failAfter: -1, closeErr: errors.New("injected close failure")},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			factory := &faultArtifactFactory{file: test.file, path: "/private/incomplete-output"}
			bash := newFakeBash(t, runnerFunc(func(_ context.Context, _ RunRequest, sink OutputSink) (ExitStatus, error) {
				if err := sink([]byte("abcdef")); err != nil {
					return ExitStatus{}, err
				}
				return testExitStatus(t, 0), nil
			}))
			bash.maxBytes = 4
			bash.store = factory

			result, err := bash.Execute(context.Background(), testBashInput(t, "chatty", nil))
			var failure *BashFailure
			if !errors.As(err, &failure) || failure.Kind() != FailureArtifact || !errors.Is(err, ErrArtifactIO) {
				t.Fatalf("error = %v, want typed artifact I/O failure", err)
			}
			if _, exposed := result.FullOutputPath(); exposed || strings.Contains(result.Text(), factory.path) {
				t.Fatalf("incomplete artifact path was exposed: %#v", result)
			}
			if factory.creates != 1 || !test.file.closed {
				t.Fatalf("artifact lifecycle = creates:%d closed:%v", factory.creates, test.file.closed)
			}
			if test.name == "write" && test.file.Len() == 0 {
				t.Fatal("write fault did not exercise a partial artifact")
			}
		})
	}
}

func TestOutputAccumulatorExactlyAtLimitsDoesNotTruncate(t *testing.T) {
	t.Parallel()
	accumulator := newTestAccumulator(t, 2, 3)
	if err := accumulator.append([]byte("a\nb")); err != nil {
		t.Fatal(err)
	}
	snapshot := finishTestAccumulator(t, accumulator)
	if snapshot.truncation.truncated {
		t.Fatalf("exact-limit output was truncated: %#v", snapshot.truncation)
	}
	if snapshot.content != "a\nb" {
		t.Fatalf("content = %q", snapshot.content)
	}
}

func TestOutputAccumulatorRollingTailMatchesWholeOutputOracle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("owner-only artifact ACL adapter intentionally fails closed on Windows")
	}
	t.Parallel()
	full := strings.Repeat("0123456789", 40) + "\nend"
	accumulator := newTestAccumulator(t, 1000, 16)
	for offset := 0; offset < len(full); {
		end := min(offset+7, len(full))
		if err := accumulator.append([]byte(full[offset:end])); err != nil {
			t.Fatal(err)
		}
		offset = end
	}
	snapshot := finishTestAccumulator(t, accumulator)
	want := tailContent(full, 1000, 16)
	if snapshot.content != want {
		t.Fatalf("rolling tail = %q, whole-output oracle = %q", snapshot.content, want)
	}
	if len(accumulator.tail) > accumulator.maxRollingBytes*2 {
		t.Fatalf("rolling buffer remained unbounded: %d bytes", len(accumulator.tail))
	}
	raw, err := os.ReadFile(snapshot.fullOutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != full {
		t.Fatalf("artifact differs after rolling trim: got %d bytes, want %d", len(raw), len(full))
	}
}

func FuzzOutputAccumulatorChunking(f *testing.F) {
	f.Add([]byte("hello\nworld"), uint16(3))
	f.Add([]byte{0xef, 0xbb, 0xbf, 0xe2, 0x82, 0xac}, uint16(1))
	f.Add([]byte{0xff, 0xe2, 0x82}, uint16(2))
	root := f.TempDir()
	f.Fuzz(func(t *testing.T, raw []byte, split uint16) {
		maxBytes := len(raw)*3 + 4
		if maxBytes < 4 {
			maxBytes = 4
		}
		store, err := newArtifactStore(root)
		if err != nil {
			t.Fatal(err)
		}
		accumulator, err := newOutputAccumulator(len(raw)+1, maxBytes, store)
		if err != nil {
			t.Fatal(err)
		}
		index := 0
		if len(raw) > 0 {
			index = int(split) % (len(raw) + 1)
		}
		if err := accumulator.append(raw[:index]); err != nil {
			t.Fatal(err)
		}
		if err := accumulator.append(raw[index:]); err != nil {
			t.Fatal(err)
		}
		snapshot := finishTestAccumulator(t, accumulator)
		if !utf8.ValidString(snapshot.content) {
			t.Fatalf("invalid UTF-8 output: %x", snapshot.content)
		}
		if snapshot.truncation.truncated {
			t.Fatalf("oversized limits unexpectedly truncated input of %d bytes", len(raw))
		}
	})
}

func newTestAccumulator(t *testing.T, maxLines, maxBytes int) *outputAccumulator {
	t.Helper()
	store, err := newArtifactStore(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	accumulator, err := newOutputAccumulator(maxLines, maxBytes, store)
	if err != nil {
		t.Fatal(err)
	}
	return accumulator
}

func finishTestAccumulator(t *testing.T, accumulator *outputAccumulator) outputSnapshot {
	t.Helper()
	if err := accumulator.finish(); err != nil {
		if runtime.GOOS == "windows" && errors.Is(err, ErrArtifactSecurity) {
			t.Skip("Windows artifact creation fails closed")
		}
		t.Fatal(err)
	}
	if err := accumulator.close(); err != nil {
		t.Fatal(err)
	}
	return accumulator.snapshot()
}
