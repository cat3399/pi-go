package tool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestMutationQueueCancelledNodesSettleBehindPredecessorWithoutResidualBarriers(t *testing.T) {
	const cancelledCount = 32

	queue := newMutationQueue()
	firstStarted := make(chan struct{})
	thirdStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var cancelledStarted atomic.Int32
	var orderMu sync.Mutex
	var order []string
	first := make(chan error, 1)
	go func() {
		first <- queue.with(context.Background(), "target", func() error {
			orderMu.Lock()
			order = append(order, "A:start")
			orderMu.Unlock()
			close(firstStarted)
			<-releaseFirst
			orderMu.Lock()
			order = append(order, "A:end")
			orderMu.Unlock()
			return nil
		})
	}()
	<-firstStarted
	waitForQueueState(t, queue, 1, 0)

	cancelled := make([]chan error, 0, cancelledCount)
	for index := 0; index < cancelledCount; index++ {
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		cancelled = append(cancelled, result)
		go func() {
			result <- queue.with(ctx, "target", func() error {
				cancelledStarted.Add(1)
				return nil
			})
		}()
		waitForQueueState(t, queue, index+2, index)
		cancel()
		waitForQueueState(t, queue, index+2, index+1)
	}

	third := make(chan error, 1)
	go func() {
		third <- queue.with(context.Background(), "target", func() error {
			orderMu.Lock()
			order = append(order, "C:start")
			orderMu.Unlock()
			close(thirdStarted)
			return nil
		})
	}()
	waitForQueueState(t, queue, cancelledCount+2, cancelledCount)
	for index, result := range cancelled {
		select {
		case err := <-result:
			t.Fatalf("cancelled node %d settled before predecessor: %v", index, err)
		default:
		}
	}
	select {
	case <-thirdStarted:
		t.Fatal("C crossed cancelled nodes while A was still running")
	default:
	}
	close(releaseFirst)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	for index, result := range cancelled {
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled node %d error = %v", index, err)
		}
	}
	select {
	case <-thirdStarted:
	case <-time.After(time.Second):
		t.Fatal("C did not start after A completed")
	}
	if err := <-third; err != nil {
		t.Fatal(err)
	}
	if got := cancelledStarted.Load(); got != 0 {
		t.Fatalf("%d cancelled operations started", got)
	}
	if nodes, keys, settling := queue.pendingState(); nodes != 0 || keys != 0 || settling != 0 {
		t.Fatalf("queue retained nodes=%d keys=%d settling=%d", nodes, keys, settling)
	}
	orderMu.Lock()
	defer orderMu.Unlock()
	if got := strings.Join(order, ","); got != "A:start,A:end,C:start" {
		t.Fatalf("order = %s", got)
	}
}

func waitForQueueState(t *testing.T, queue *mutationQueue, expectedNodes, expectedSettling int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		nodes, _, settling := queue.pendingState()
		if nodes == expectedNodes && settling == expectedSettling {
			return
		}
		time.Sleep(time.Millisecond)
	}
	nodes, keys, settling := queue.pendingState()
	t.Fatalf("queue nodes = %d, keys = %d, settling = %d; want nodes = %d, settling = %d", nodes, keys, settling, expectedNodes, expectedSettling)
}

func TestWriteFollowsSymlinkPreservesModeAndRejectsReadonly(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("symlink creation needs platform privilege on Windows")
	}
	suite := newTestSuite(t)
	target := writeTestFile(t, suite.WorkingDir(), "target.txt", "old")
	if err := os.Chmod(target, 0o640); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(suite.WorkingDir(), "alias.txt")
	if err := os.Symlink("target.txt", alias); err != nil {
		t.Fatal(err)
	}
	if _, err := suite.Write(context.Background(), WriteInput{Path: "alias.txt", Content: "new"}); err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := os.Lstat(alias)
	if err != nil {
		t.Fatal(err)
	}
	if aliasInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatal("write replaced the symlink itself")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "new" {
		t.Fatalf("target = %q, %v", data, err)
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if targetInfo.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o", targetInfo.Mode().Perm())
	}
	aliasKey, err := mutationKey(alias)
	if err != nil {
		t.Fatal(err)
	}
	targetKey, err := mutationKey(target)
	if err != nil {
		t.Fatal(err)
	}
	if aliasKey != targetKey {
		t.Fatalf("alias key %q != target key %q", aliasKey, targetKey)
	}

	if err := os.Chmod(target, 0o444); err != nil {
		t.Fatal(err)
	}
	_, err = suite.Write(context.Background(), WriteInput{Path: "alias.txt", Content: "forbidden"})
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("readonly write error = %v", err)
	}
	data, _ = os.ReadFile(target)
	if string(data) != "new" {
		t.Fatalf("readonly target changed to %q", data)
	}
}

func TestEditThroughSymlinkUpdatesTarget(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("symlink creation needs platform privilege on Windows")
	}
	suite := newTestSuite(t)
	target := writeTestFile(t, suite.WorkingDir(), "edit-target.txt", "alpha\nbeta\n")
	if err := os.Symlink("edit-target.txt", filepath.Join(suite.WorkingDir(), "edit-alias.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := suite.Edit(context.Background(), EditInput{Path: "edit-alias.txt", Edits: []Edit{{OldText: "beta", NewText: "BETA"}}}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(target)
	if string(data) != "alpha\nBETA\n" {
		t.Fatalf("target = %q", data)
	}
}

func TestGeneratedUnifiedPatchAppliesSingleAndDistantEdits(t *testing.T) {
	lines := make([]string, 80)
	for index := range lines {
		lines[index] = fmt.Sprintf("line-%03d", index+1)
	}
	before := strings.Join(lines, "\n") + "\n"
	afterLines := append([]string(nil), lines...)
	afterLines[1], afterLines[39], afterLines[77] = "LINE-002", "LINE-040", "LINE-078"
	after := strings.Join(afterLines, "\n") + "\n"
	_, patch, firstChanged := makeEditDiff("sample.txt", before, after)
	if firstChanged != 2 {
		t.Fatalf("first changed line = %d", firstChanged)
	}
	if got := strings.Count(patch, "@@ -"); got != 3 {
		t.Fatalf("hunk count = %d\n%s", got, patch)
	}
	applied, err := applyUnifiedPatchForTest(before, patch)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, patch)
	}
	if applied != after {
		t.Fatalf("applied patch mismatch")
	}

	_, singlePatch, _ := makeEditDiff("single.txt", "one\ntwo", "one\nTWO")
	applied, err = applyUnifiedPatchForTest("one\ntwo", singlePatch)
	if err != nil {
		t.Fatal(err)
	}
	if applied != "one\nTWO" {
		t.Fatalf("single applied = %q", applied)
	}
}

func applyUnifiedPatchForTest(original, patch string) (string, error) {
	originalLines := splitDiffLines(original)
	patchLines := strings.Split(patch, "\n")
	if len(patchLines) < 2 {
		return "", errors.New("missing file headers")
	}
	oldCursor := 0
	var output []string
	for index := 2; index < len(patchLines); {
		if patchLines[index] == "" {
			index++
			continue
		}
		var oldStart, oldCount, newStart, newCount int
		if _, err := fmt.Sscanf(patchLines[index], "@@ -%d,%d +%d,%d @@", &oldStart, &oldCount, &newStart, &newCount); err != nil {
			return "", fmt.Errorf("bad hunk header %q: %w", patchLines[index], err)
		}
		index++
		copyUntil := oldStart - 1
		if oldCount == 0 {
			copyUntil = oldStart
		}
		if copyUntil < oldCursor || copyUntil > len(originalLines) {
			return "", errors.New("invalid old start")
		}
		output = append(output, originalLines[oldCursor:copyUntil]...)
		oldCursor = copyUntil
		seenOld, seenNew := 0, 0
		for index < len(patchLines) && !strings.HasPrefix(patchLines[index], "@@ ") && patchLines[index] != "" {
			line := patchLines[index]
			if len(line) == 0 {
				return "", errors.New("empty patch operation")
			}
			operation, text := line[0], line[1:]+"\n"
			if index+1 < len(patchLines) && patchLines[index+1] == `\ No newline at end of file` {
				text = strings.TrimSuffix(text, "\n")
				index++
			}
			switch operation {
			case ' ', '-':
				if oldCursor >= len(originalLines) || originalLines[oldCursor] != text {
					return "", fmt.Errorf("old line mismatch at %d", oldCursor+1)
				}
				if operation == ' ' {
					output = append(output, text)
					seenNew++
				}
				oldCursor++
				seenOld++
			case '+':
				output = append(output, text)
				seenNew++
			default:
				return "", fmt.Errorf("unknown patch operation %q", operation)
			}
			index++
		}
		if seenOld != oldCount || seenNew != newCount {
			return "", fmt.Errorf("hunk counts old=%d/%d new=%d/%d", seenOld, oldCount, seenNew, newCount)
		}
	}
	output = append(output, originalLines[oldCursor:]...)
	return strings.Join(output, ""), nil
}

func TestIgnoreRulesParentNestedMalformedAndIOFailure(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, root, ".gitignore", "nested/parent-ignored.txt\n")
	writeTestFile(t, root, "nested/.gitignore", "local-ignored.txt\n")
	writeTestFile(t, root, "nested/parent-ignored.txt", "x")
	writeTestFile(t, root, "nested/local-ignored.txt", "x")
	writeTestFile(t, root, "nested/kept.txt", "x")
	suite, err := NewFilesystemSuite(FilesystemOptions{WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	result, err := suite.Find(context.Background(), FindInput{Pattern: "*.txt", Path: textPointer("nested")})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "kept.txt" {
		t.Fatalf("nested search = %q", result.Text)
	}

	writeTestFile(t, root, "nested/.gitignore", "[")
	_, err = suite.Find(context.Background(), FindInput{Pattern: "**", Path: textPointer("nested")})
	if !errors.Is(err, ErrInvalidFilesystemInput) {
		t.Fatalf("malformed ignore error = %v", err)
	}
	writeTestFile(t, root, "nested/.gitignore", `escaped\ pattern`)
	result, err = suite.Find(context.Background(), FindInput{Pattern: "**", Path: textPointer("nested")})
	if err != nil || strings.Contains(result.Text, "escaped pattern") {
		t.Fatalf("escaped ignore syntax result = %#v, %v", result, err)
	}

	other := t.TempDir()
	if err := os.Mkdir(filepath.Join(other, ".gitignore"), 0o755); err != nil {
		t.Fatal(err)
	}
	otherSuite, err := NewFilesystemSuite(FilesystemOptions{WorkingDir: other})
	if err != nil {
		t.Fatal(err)
	}
	_, err = otherSuite.Find(context.Background(), FindInput{Pattern: "**"})
	if err == nil || !strings.Contains(err.Error(), "read ignore rules") {
		t.Fatalf("ignore I/O error = %v", err)
	}
}

func TestGrepLargeContextUsesByteLimitOnly(t *testing.T) {
	suite := newTestSuite(t)
	lines := make([]string, 2101)
	lines[1050] = "MATCH"
	writeTestFile(t, suite.WorkingDir(), "f", strings.Join(lines, "\n"))
	result, err := suite.Grep(context.Background(), GrepInput{Pattern: "MATCH", Path: textPointer("f"), Context: intPointer(1050)})
	if err != nil {
		t.Fatal(err)
	}
	if result.Details != nil {
		t.Fatalf("unexpected truncation details: %#v", result.Details)
	}
	if got := strings.Count(result.Text, "\n") + 1; got != 2101 {
		t.Fatalf("output lines = %d", got)
	}
	if len(result.Text) >= DefaultFilesystemMaxBytes {
		t.Fatalf("fixture unexpectedly exceeds byte limit: %d", len(result.Text))
	}
}

func TestReadNFDAndLowercaseScreenshotAMPMFallbacks(t *testing.T) {
	suite := newTestSuite(t)
	writeTestFile(t, suite.WorkingDir(), "cafe\u0301.txt", "nfd")
	result, err := suite.Read(context.Background(), ReadInput{Path: "café.txt"})
	if err != nil || result.Text != "nfd" {
		t.Fatalf("NFD fallback = %#v, %v", result, err)
	}
	writeTestFile(t, suite.WorkingDir(), "Screenshot at 10.00.00\u202fam.png", "lower")
	result, err = suite.Read(context.Background(), ReadInput{Path: "Screenshot at 10.00.00 am.png"})
	if err != nil || result.Text != "lower" {
		t.Fatalf("lower AM fallback = %#v, %v", result, err)
	}
}

type countingCancelContext struct {
	checks      atomic.Int32
	cancelAfter int32
}

func (c *countingCancelContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *countingCancelContext) Done() <-chan struct{}       { return nil }
func (c *countingCancelContext) Err() error {
	if c.checks.Add(1) >= c.cancelAfter {
		return context.Canceled
	}
	return nil
}
func (c *countingCancelContext) Value(any) any { return nil }

func TestCancellationAtEmptyLsAndDuringTreeWalk(t *testing.T) {
	suite := newTestSuite(t)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := suite.Ls(cancelled, LsInput{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled empty ls = %v", err)
	}
	for index := 0; index < 200; index++ {
		writeTestFile(t, suite.WorkingDir(), filepath.Join("tree", strconv.Itoa(index), "file.txt"), "x")
	}
	stepped := &countingCancelContext{cancelAfter: 30}
	if _, err := suite.Find(stepped, FindInput{Pattern: "**"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-walk cancellation = %v", err)
	}
}

func FuzzUnifiedPatchRoundTrip(f *testing.F) {
	f.Add("one\ntwo\n", "one\nTWO\n")
	f.Add("", "created without newline")
	f.Add("deleted", "")
	f.Fuzz(func(t *testing.T, before, after string) {
		if !utf8.ValidString(before) || !utf8.ValidString(after) || len(before)+len(after) > 16*1024 {
			t.Skip()
		}
		_, patch, _ := makeEditDiff("fuzz.txt", before, after)
		applied, err := applyUnifiedPatchForTest(before, patch)
		if err != nil {
			t.Fatalf("apply failed: %v\n%s", err, patch)
		}
		if applied != after {
			t.Fatalf("round trip mismatch: got %q want %q\n%s", applied, after, patch)
		}
	})
}
