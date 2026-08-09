package tool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type repeatedByteReader struct {
	value     byte
	remaining int64
}

func (r *repeatedByteReader) Read(buffer []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	count := len(buffer)
	if int64(count) > r.remaining {
		count = int(r.remaining)
	}
	for index := 0; index < count; index++ {
		buffer[index] = r.value
	}
	r.remaining -= int64(count)
	return count, nil
}

func TestIgnoreControlParsingStreamsHugeCommentsLongLinesAndManyRules(t *testing.T) {
	t.Run("huge comment", func(t *testing.T) {
		input := io.MultiReader(
			strings.NewReader("#"),
			&repeatedByteReader{value: 'x', remaining: 32 << 20},
			strings.NewReader("\r\n"),
		)
		var stats ignoreControlStreamStats
		if err := streamIgnoreControlLinesMeasured(context.Background(), input, func(int, string) error {
			t.Fatal("comment was emitted as an ignore rule")
			return nil
		}, &stats); err != nil {
			t.Fatal(err)
		}
		if stats.maxRetainedLineBytes != 0 || stats.yieldedLines != 0 {
			t.Fatalf("huge comment retained bytes=%d yielded=%d", stats.maxRetainedLineBytes, stats.yieldedLines)
		}
	})

	t.Run("line beyond bufio scanner ceiling", func(t *testing.T) {
		longRule := strings.Repeat("*", 70<<10) + "target.txt"
		base := t.TempDir()
		frame := ignoreRuleFrame{base: base}
		var stats ignoreControlStreamStats
		err := streamIgnoreControlLinesMeasured(
			context.Background(), strings.NewReader(longRule+"\r\n"),
			func(lineNumber int, line string) error {
				return frame.addRule(context.Background(), "fixture.ignore", lineNumber, line, false)
			},
			&stats,
		)
		if err != nil {
			t.Fatal(err)
		}
		matcher := &ignoreMatcher{frames: []ignoreRuleFrame{frame}}
		ignored, err := matcher.ignored(filepath.Join(base, "target.txt"), false)
		if err != nil || frame.ruleCount != 1 || !ignored {
			t.Fatalf("long ignore rule was not preserved: rules=%d ignored=%v err=%v", frame.ruleCount, ignored, err)
		}
		if len(frame.rules.suffixBasename.allEntries) != 1 || len(frame.rules.basenameGlobs.compiled) != 0 {
			t.Fatalf("long suffix rule was not compactly indexed: suffix=%d compiled=%d", len(frame.rules.suffixBasename.allEntries), len(frame.rules.basenameGlobs.compiled))
		}
		if stats.maxRetainedLineBytes != len(longRule) || stats.yieldedLines != 1 {
			t.Fatalf("long rule retained bytes=%d yielded=%d", stats.maxRetainedLineBytes, stats.yieldedLines)
		}
	})

	t.Run("many rules", func(t *testing.T) {
		const ruleCount = 10_000
		var input strings.Builder
		for index := 0; index < ruleCount; index++ {
			input.WriteString(fmt.Sprintf("rule-%05d.tmp\n", index))
		}
		base := t.TempDir()
		frame := ignoreRuleFrame{base: base}
		var stats ignoreControlStreamStats
		err := streamIgnoreControlLinesMeasured(
			context.Background(), strings.NewReader(input.String()),
			func(lineNumber int, line string) error {
				return frame.addRule(context.Background(), "fixture.ignore", lineNumber, line, false)
			},
			&stats,
		)
		if err != nil {
			t.Fatal(err)
		}
		if frame.ruleCount != ruleCount || len(frame.rules.exactBasename.allEntries) != ruleCount || stats.yieldedLines != ruleCount || stats.maxRetainedLineBytes != len("rule-00000.tmp") {
			t.Fatalf("many rules indexed=%d exact=%d yielded=%d max retained=%d", frame.ruleCount, len(frame.rules.exactBasename.allEntries), stats.yieldedLines, stats.maxRetainedLineBytes)
		}
		matcher := &ignoreMatcher{frames: []ignoreRuleFrame{frame}}
		ignored, err := matcher.ignored(filepath.Join(base, "rule-09999.tmp"), false)
		if err != nil || !ignored {
			t.Fatalf("last of many streamed rules did not retain matching semantics: ignored=%v err=%v", ignored, err)
		}
	})
}

func TestIgnoreMatcherHundredThousandRulesIsIndexedScopedAndBounded(t *testing.T) {
	const ruleCount = 100_000
	root := t.TempDir()
	var rules strings.Builder
	rules.Grow(ruleCount * len("prefix-00000-*\n"))
	for index := 0; index < 25_000; index++ {
		fmt.Fprintf(&rules, "noise-%05d.tmp\n", index)
	}
	for index := 0; index < 25_000; index++ {
		fmt.Fprintf(&rules, "prefix-%05d-*\n", index)
	}
	for index := 0; index < 25_000; index++ {
		fmt.Fprintf(&rules, "*-suffix-%05d.txt\n", index)
	}
	for index := 0; index < 24_997; index++ {
		fmt.Fprintf(&rules, "glob-%05d-?.txt\n", index)
	}
	// The final three rules exercise the tail lookup and last-match negation;
	// !negated.txt is exactly rule 100,000.
	rules.WriteString("last-rule.txt\nnegated.txt\n!negated.txt\n")
	writeTestFile(t, root, "a/.gitignore", rules.String())
	for index := 0; index < 93; index++ {
		writeTestFile(t, root, fmt.Sprintf("a/kept-%03d.txt", index), "kept\n")
	}
	writeTestFile(t, root, "a/prefix-24999-value.txt", "ignored by the prefix index\n")
	writeTestFile(t, root, "a/value-suffix-24999.txt", "ignored by the suffix index\n")
	writeTestFile(t, root, "a/glob-24996-a.txt", "ignored by the complex-glob index\n")
	writeTestFile(t, root, "a/last-rule.txt", "ignored by the 99,998th rule\n")
	writeTestFile(t, root, "a/negated.txt", "kept by the 100,000th rule\n")
	writeTestFile(t, root, "b/last-rule.txt", "must survive a's exact-rule frame\n")
	writeTestFile(t, root, "b/prefix-24999-value.txt", "must survive a's prefix-rule frame\n")

	// Measure the retained production matcher, not a synthetic regexp helper.
	// These intentionally broad bounds distinguish the former ~426 MiB regexp
	// representation while leaving ample room for allocator/runtime variance.
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	matcher, err := newIgnoreMatcher(context.Background(), root, []string{".gitignore", ".ignore", ".fdignore"}, vcsIgnoreGlobal)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := matcher.checkpoint()
	if err := matcher.pushDirectory(context.Background(), filepath.Join(root, "a"), false); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	retained := uint64(0)
	if after.HeapAlloc > before.HeapAlloc {
		retained = after.HeapAlloc - before.HeapAlloc
	}
	if allocated > 160<<20 || retained > 96<<20 {
		t.Fatalf("100k-rule matcher allocated=%d MiB retained=%d MiB", allocated>>20, retained>>20)
	}
	t.Logf("100k-rule matcher allocated=%d MiB retained=%d MiB", allocated>>20, retained>>20)
	if len(matcher.frames) != 1 || matcher.frames[0].ruleCount != ruleCount {
		t.Fatalf("active frames=%d rules=%d", len(matcher.frames), matcher.frames[0].ruleCount)
	}
	frame := &matcher.frames[0]
	if got := len(frame.versionControl.exactBasename.allEntries); got != 25_002 {
		t.Fatalf("deduplicated exact index entries=%d, want 25002", got)
	}
	if len(frame.versionControl.prefixBasename.allEntries) != 25_000 || len(frame.versionControl.suffixBasename.allEntries) != 25_000 || len(frame.versionControl.basenameGlobs.rules) != 24_997 || len(frame.versionControl.basenameGlobs.compiled) != 0 {
		t.Fatalf("classified indexes exact=%d prefix=%d suffix=%d glob=%d compiled=%d",
			len(frame.versionControl.exactBasename.allEntries), len(frame.versionControl.prefixBasename.allEntries),
			len(frame.versionControl.suffixBasename.allEntries), len(frame.versionControl.basenameGlobs.rules),
			len(frame.versionControl.basenameGlobs.compiled))
	}
	for path, want := range map[string]bool{
		"a/last-rule.txt":          true,
		"a/negated.txt":            false,
		"a/kept-000.txt":           false,
		"a/prefix-24999-value.txt": true,
		"a/value-suffix-24999.txt": true,
		"a/glob-24996-a.txt":       true,
		"a/glob-24995-ab.txt":      false,
		"a/value-suffix-24998.tmp": false,
	} {
		got, matchErr := matcher.ignored(filepath.Join(root, path), false)
		if matchErr != nil || got != want {
			t.Fatalf("ignored(%q)=%v, %v; want %v", path, got, matchErr, want)
		}
	}
	if len(frame.versionControl.basenameGlobs.compiled) != 2 {
		t.Fatalf("selected complex glob cache=%d, want 2 of 24997", len(frame.versionControl.basenameGlobs.compiled))
	}
	frame = nil
	matcher.restore(checkpoint)
	if len(matcher.frames) != 0 {
		t.Fatalf("child frame count after pop=%d, want 0", len(matcher.frames))
	}
	for _, path := range []string{"b/last-rule.txt", "b/prefix-24999-value.txt"} {
		ignored, matchErr := matcher.ignored(filepath.Join(root, filepath.FromSlash(path)), false)
		if matchErr != nil || ignored {
			t.Fatalf("popped child rule leaked into %q: ignored=%v err=%v", path, ignored, matchErr)
		}
	}
	runtime.GC()
	var afterPop runtime.MemStats
	runtime.ReadMemStats(&afterPop)
	if afterPop.HeapAlloc > before.HeapAlloc+8<<20 {
		t.Fatalf("popped child frame retained %d MiB", (afterPop.HeapAlloc-before.HeapAlloc)>>20)
	}
	runtime.KeepAlive(matcher)

	// The real Find operation reparses the control file, walks 100 candidates,
	// observes rule 100,000, and proves a child frame is popped before b/.
	suite, err := NewFilesystemSuite(FilesystemOptions{WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := time.Now()
	result, err := suite.Find(ctx, FindInput{Pattern: "*.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed >= 10*time.Second {
		t.Fatalf("100k rules and 100 files took %s", elapsed)
	} else {
		t.Logf("100k rules and 100 files completed in %s", elapsed)
	}
	lines := splitNonemptyLines(result.Text)
	if len(lines) != 96 {
		t.Fatalf("visible txt files=%d, want 96: %q", len(lines), result.Text)
	}
	if containsOutputLine(result.Text, "a/last-rule.txt") || containsOutputLine(result.Text, "a/prefix-24999-value.txt") ||
		containsOutputLine(result.Text, "a/value-suffix-24999.txt") || containsOutputLine(result.Text, "a/glob-24996-a.txt") {
		t.Fatalf("tail or child-scoped ignore was lost: %q", result.Text)
	}
	if !containsOutputLine(result.Text, "a/negated.txt") || !containsOutputLine(result.Text, "b/last-rule.txt") || !containsOutputLine(result.Text, "b/prefix-24999-value.txt") {
		t.Fatalf("tail negation or sibling frame pop was lost: %q", result.Text)
	}
}

func TestCompactIgnoreMatcherPreservesOrderedPatternClasses(t *testing.T) {
	base := t.TempDir()
	frame := ignoreRuleFrame{base: base}
	if err := frame.addRule(context.Background(), "fixture.ignore", 1, "[z-a]", false); !errors.Is(err, ErrInvalidFilesystemInput) {
		t.Fatalf("invalid character range was not rejected before traversal: %v", err)
	}
	patterns := []string{
		"literal.txt",
		"prefix-*",
		"*.suffix",
		"glob-?-x.txt",
		"/anchored.txt",
		"cache/",
		"ordered.txt",
		"!ordered.txt",
	}
	for index, pattern := range patterns {
		if err := frame.addRule(context.Background(), "fixture.ignore", index+2, pattern, false); err != nil {
			t.Fatal(err)
		}
	}
	matcher := &ignoreMatcher{frames: []ignoreRuleFrame{frame}}
	assertIgnored := func(path string, directory, want bool) {
		t.Helper()
		got, err := matcher.ignored(filepath.Join(base, filepath.FromSlash(path)), directory)
		if err != nil || got != want {
			t.Fatalf("ignored(%q, dir=%v)=%v, %v; want %v", path, directory, got, err, want)
		}
	}
	assertIgnored("deep/literal.txt", false, true)
	assertIgnored("prefix-value", false, true)
	assertIgnored("value.suffix", false, true)
	assertIgnored("deep/anchored.txt", false, false)
	assertIgnored("anchored.txt", false, true)
	assertIgnored("cache", false, false)
	assertIgnored("cache", true, true)
	assertIgnored("ordered.txt", false, false)
	if len(matcher.frames[0].rules.basenameGlobs.compiled) != 0 {
		t.Fatal("complex glob compiled before an indexed candidate was tested")
	}
	assertIgnored("unrelated.txt", false, false)
	if len(matcher.frames[0].rules.basenameGlobs.compiled) != 0 {
		t.Fatal("unrelated candidate compiled an indexed complex glob")
	}
	assertIgnored("glob-a-x.txt", false, true)
	if len(matcher.frames[0].rules.basenameGlobs.compiled) != 1 {
		t.Fatalf("complex matcher cache size=%d, want 1", len(matcher.frames[0].rules.basenameGlobs.compiled))
	}
}

func TestFindAndGrepStopDirectoryStreamingAtFirstLimit(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "000.txt", "TOKEN\n")
	for index := 0; index < 2_000; index++ {
		writeTestFile(t, root, filepath.Join("zzz-tail", "deep", strings.Repeat("x", index%8), fmt.Sprintf("%04d.txt", index)), "TOKEN\n")
	}
	suite, err := NewFilesystemSuite(FilesystemOptions{WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	realReadDir := os.ReadDir
	var calls atomic.Int64
	var enteredTail atomic.Bool
	suite.readSearchDir = func(path string) ([]os.DirEntry, error) {
		calls.Add(1)
		if strings.HasPrefix(path, filepath.Join(root, "zzz-tail")) {
			enteredTail.Store(true)
		}
		return realReadDir(path)
	}

	limit := 1
	result, err := suite.Find(context.Background(), FindInput{Pattern: "*.txt", Limit: &limit})
	if err != nil || !strings.HasPrefix(result.Text, "000.txt\n\n[1 results limit reached") {
		t.Fatalf("streamed find = %q, %v", result.Text, err)
	}
	if calls.Load() != 1 || enteredTail.Load() {
		t.Fatalf("find ReadDir calls=%d entered tail=%v", calls.Load(), enteredTail.Load())
	}

	calls.Store(0)
	enteredTail.Store(false)
	result, err = suite.Grep(context.Background(), GrepInput{Pattern: "TOKEN", Limit: &limit})
	if err != nil || !strings.HasPrefix(result.Text, "000.txt:1: TOKEN") {
		t.Fatalf("streamed grep = %q, %v", result.Text, err)
	}
	if calls.Load() != 1 || enteredTail.Load() {
		t.Fatalf("grep ReadDir calls=%d entered tail=%v", calls.Load(), enteredTail.Load())
	}

	allocations := testing.AllocsPerRun(20, func() {
		if _, findErr := suite.Find(context.Background(), FindInput{Pattern: "*.txt", Limit: &limit}); findErr != nil {
			panic(findErr)
		}
	})
	if allocations > 400 {
		t.Fatalf("first-result Find allocated %.1f objects; tail tree should never be materialized", allocations)
	}
}

func TestStreamingWalkUsesDeterministicPerDirectoryOrder(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "b.txt", "b")
	writeTestFile(t, root, "a/child.txt", "child")
	writeTestFile(t, root, "a.txt", "a")
	suite, err := NewFilesystemSuite(FilesystemOptions{WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	result, err := suite.Find(context.Background(), FindInput{Pattern: "**"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "a/\na/child.txt\na.txt\nb.txt" {
		t.Fatalf("per-directory order = %q", result.Text)
	}
}
