package tool

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestStandaloneBashStreamsSanitizedOutputAndReturnsExitStatusAsData(t *testing.T) {
	t.Parallel()
	workingDir := t.TempDir()
	bash, err := NewBash(BashOptions{
		WorkingDir: workingDir, Environment: []string{},
		Runner: runnerFunc(func(_ context.Context, request RunRequest, sink OutputSink) (ExitStatus, error) {
			if request.Command() != "failing command" || request.WorkingDir() != workingDir {
				t.Fatalf("request = %#v", request)
			}
			for _, chunk := range [][]byte{
				[]byte("\x1b[31mhello "),
				{0xe4, 0xbd},
				{0xa0, '\x1b', '[', '0', 'm', '\r', '\n', 0x00},
			} {
				if err := sink(chunk); err != nil {
					return ExitStatus{}, err
				}
			}
			return testExitStatus(t, 7), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	result, err := bash.ExecuteStandalone(context.Background(), "failing command", func(delta string) {
		deltas = append(deltas, delta)
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "hello 你\n" || result.ExitCode == nil || *result.ExitCode != 7 || result.Cancelled || result.Truncated {
		t.Fatalf("result = %#v", result)
	}
	if want := []string{"hello ", "", "你\n"}; !reflect.DeepEqual(deltas, want) {
		t.Fatalf("deltas = %#v, want %#v", deltas, want)
	}
}

func TestStandaloneBashCancellationReturnsPartialResult(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	bash, err := NewBash(BashOptions{
		WorkingDir: t.TempDir(), Environment: []string{},
		Runner: runnerFunc(func(ctx context.Context, _ RunRequest, sink OutputSink) (ExitStatus, error) {
			if err := sink([]byte("partial")); err != nil {
				return ExitStatus{}, err
			}
			close(started)
			<-ctx.Done()
			return UnknownExitStatus(), NewRunInterruptedError(context.Cause(ctx))
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan struct {
		result StandaloneBashResult
		err    error
	}, 1)
	go func() {
		result, executeErr := bash.ExecuteStandalone(ctx, "wait", nil)
		done <- struct {
			result StandaloneBashResult
			err    error
		}{result: result, err: executeErr}
	}()
	<-started
	cancel(context.Canceled)
	outcome := <-done
	if outcome.err != nil || outcome.result.Output != "partial" || !outcome.result.Cancelled || outcome.result.ExitCode != nil {
		t.Fatalf("cancelled result = (%#v, %v)", outcome.result, outcome.err)
	}
}

func TestStandaloneBashTruncationStoresSanitizedFullOutput(t *testing.T) {
	t.Parallel()
	artifactDir := filepath.Join(t.TempDir(), "private-artifacts")
	bash, err := NewBash(BashOptions{
		WorkingDir: t.TempDir(), Environment: []string{}, ArtifactDirectory: artifactDir,
		MaxOutputLines: 20, MaxOutputBytes: 5,
		Runner: runnerFunc(func(_ context.Context, _ RunRequest, sink OutputSink) (ExitStatus, error) {
			if err := sink([]byte("\x1b[32mabcdef\x1b[0m")); err != nil {
				return ExitStatus{}, err
			}
			return testExitStatus(t, 0), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := bash.ExecuteStandalone(context.Background(), "large", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || result.FullOutputPath == "" || !strings.HasSuffix(result.Output, "bcdef") {
		t.Fatalf("truncated result = %#v", result)
	}
	full, err := os.ReadFile(result.FullOutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(full) != "abcdef" {
		t.Fatalf("full output = %q", full)
	}
}

func TestStandaloneBashPreservesSanitizedArtifactWhenRawStreamCrossesThreshold(t *testing.T) {
	t.Parallel()
	artifactDir := filepath.Join(t.TempDir(), "private-artifacts")
	bash, err := NewBash(BashOptions{
		WorkingDir: t.TempDir(), Environment: []string{}, ArtifactDirectory: artifactDir,
		MaxOutputLines: 20, MaxOutputBytes: 5,
		Runner: runnerFunc(func(_ context.Context, _ RunRequest, sink OutputSink) (ExitStatus, error) {
			if err := sink([]byte("\x1b[31mx\x1b[0m")); err != nil {
				return ExitStatus{}, err
			}
			return testExitStatus(t, 0), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := bash.ExecuteStandalone(context.Background(), "colored", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "x" || result.Truncated || result.FullOutputPath == "" {
		t.Fatalf("raw-threshold result = %#v", result)
	}
	full, err := os.ReadFile(result.FullOutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(full) != "x" {
		t.Fatalf("raw-threshold full output = %q", full)
	}
}
