//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package terminal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func duration(value time.Duration) *time.Duration { return &value }

func testService(t *testing.T) *Service {
	t.Helper()
	s := New(Options{CWD: t.TempDir(), Shell: "/bin/sh", Environment: []string{"PATH=/usr/bin:/bin", "PS1=", "PS2="}})
	t.Cleanup(func() {
		if err := s.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	return s
}

func testOpen(t *testing.T, s *Service) string {
	t.Helper()
	r, err := s.Do(context.Background(), Request{Action: Open, Wait: duration(0)}, Agent)
	if err != nil {
		t.Fatal(err)
	}
	r = untilText(t, s, Request{Action: Run, ID: r.Terminal.ID, Command: "stty -echo; printf 'READY\\n'"}, "READY\n")
	return r.Terminal.ID
}

func untilText(t *testing.T, s *Service, req Request, expected string) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	r, err := s.Do(ctx, req, Agent)
	if err != nil {
		t.Fatal(err)
	}
	text := r.Text
	for !strings.Contains(text, expected) {
		r, err = s.Do(ctx, Request{Action: Read, ID: r.Terminal.ID, Wait: duration(250 * time.Millisecond)}, Agent)
		if err != nil {
			t.Fatalf("waiting for %q in %q: %v", expected, text, err)
		}
		text += r.Text
	}
	r.Text = text
	return r
}

func TestPersistentShellAndIsolation(t *testing.T) {
	s := testService(t)
	id := testOpen(t, s)
	dir := t.TempDir()
	untilText(t, s, Request{Action: Run, ID: id, Command: "export PI_TERMINAL_VALUE=retained; cd '" + dir + "'; printf 'SET\\n'"}, "SET\n")
	r := untilText(t, s, Request{Action: Run, ID: id, Command: "printf '%s\\n' \"$PI_TERMINAL_VALUE\"; pwd"}, dir+"\n")
	if !strings.Contains(r.Text, "retained\n") || strings.Contains(r.Text, "SET\n") {
		t.Fatalf("state/incremental output: %q", r.Text)
	}
	id2 := testOpen(t, s)
	r = untilText(t, s, Request{Action: Run, ID: id2, Command: "printf 'value=%s\\n' \"$PI_TERMINAL_VALUE\""}, "value=\n")
	if id == id2 || strings.Contains(r.Text, "retained") {
		t.Fatalf("terminals were not isolated: %+v", r)
	}
	if !s.HasLive() {
		t.Fatal("shell should remain live after a read")
	}
}

func TestAsyncBoundedReadAndIndependentSurfaceCursors(t *testing.T) {
	s := testService(t)
	id := testOpen(t, s)
	start := time.Now()
	r, err := s.Do(context.Background(), Request{Action: Run, ID: id, Command: "sleep 0.25; printf 'LATER\\n'", Wait: duration(0)}, Agent)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("async run waited for the process")
	}
	if r.Terminal.State != "running" {
		t.Fatal("a returned read must not claim command completion")
	}
	cursor := r.Cursor
	untilText(t, s, Request{Action: Read, ID: id, Mode: "follow"}, "LATER\n")
	var results [2]Result
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, readErr := s.View(context.Background(), Request{Action: Read, ID: id, After: &cursor, Wait: duration(0)})
			if readErr != nil {
				t.Error(readErr)
			}
			results[i] = result
		}()
	}
	wg.Wait()
	if !bytes.Equal(results[0].Data, results[1].Data) || !bytes.Contains(results[0].Data, []byte("LATER")) {
		t.Fatalf("surface readers drained each other: %+v", results)
	}
	r, err = s.Do(context.Background(), Request{Action: Read, ID: id, Wait: duration(35 * time.Millisecond)}, Agent)
	if err != nil || r.Text != "" || r.Reason != "deadline" {
		t.Fatalf("repeat/silent wait = %+v, %v", r, err)
	}
}

func TestSharedInputAndInteractiveInterrupt(t *testing.T) {
	s := testService(t)
	id := testOpen(t, s)
	_, err := s.View(context.Background(), Request{Action: Write, ID: id, Input: "printf 'FROM_USER\\n'\n", Wait: duration(200 * time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	// An independent surface may disconnect here; that does not own the shell.
	time.Sleep(50 * time.Millisecond)
	r, err := s.Do(context.Background(), Request{Action: Read, ID: id, Wait: duration(0)}, Agent)
	if err != nil || strings.Contains(r.Text, "FROM_USER") {
		t.Fatalf("default user projection: %q, %v", r.Text, err)
	}
	r, err = s.Do(context.Background(), Request{Action: Read, ID: id, Mode: "tail", IncludeUser: true}, Agent)
	if err != nil || !strings.Contains(r.Text, "FROM_USER\n") {
		t.Fatalf("shared projection: %q, %v", r.Text, err)
	}
	_, err = s.Do(context.Background(), Request{Action: Run, ID: id, Command: "read reply; printf 'reply=%s\\n' \"$reply\"", Wait: duration(25 * time.Millisecond)}, Agent)
	if err != nil {
		t.Fatal(err)
	}
	untilText(t, s, Request{Action: Write, ID: id, Input: "interactive\n"}, "reply=interactive\n")
	_, err = s.Do(context.Background(), Request{Action: Run, ID: id, Command: "sleep 30", Wait: duration(30 * time.Millisecond)}, Agent)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Do(context.Background(), Request{Action: Interrupt, ID: id, Wait: duration(150 * time.Millisecond)}, Agent)
	if err != nil {
		t.Fatal(err)
	}
	untilText(t, s, Request{Action: Run, ID: id, Command: "printf 'ALIVE\\n'"}, "ALIVE\n")
}

func TestTruncationRetainsPrivateLogAndShutdown(t *testing.T) {
	s := testService(t)
	id := testOpen(t, s)
	r := untilText(t, s, Request{Action: Run, ID: id, Command: "i=0; while [ $i -lt 1000 ]; do printf '0123456789\\n'; i=$((i+1)); done; printf 'END\\n'", MaxBytes: 100, MaxLines: 5}, "END\n")
	// Read a bounded tail after generation, irrespective of where streaming
	// split the original run result.
	r, err := s.Do(context.Background(), Request{Action: Read, ID: id, Mode: "tail", MaxBytes: 100, MaxLines: 5}, Agent)
	if err != nil || len(r.Text) > 100 || strings.Count(r.Text, "\n") > 5 || !r.Truncated {
		t.Fatalf("truncation: %+v, %v", r, err)
	}
	path := r.Terminal.LogPath
	content, err := os.ReadFile(path)
	if err != nil || bytes.Count(content, []byte("0123456789")) < 1000 {
		t.Fatalf("complete log: %d, %v", len(content), err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("log permissions: %v, %v", info, err)
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil || parent.Mode().Perm() != 0700 {
		t.Fatalf("log directory permissions: %v, %v", parent, err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.HasLive() {
		t.Fatal("shutdown left a live terminal")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("shutdown removed output artifact", err)
	}
	if _, err := s.View(context.Background(), Request{Action: Open}); !errors.Is(err, ErrClosed) {
		t.Fatalf("open after shutdown: %v", err)
	}
}

func TestCanceledReadDoesNotAbortTerminalAndStartupIsBounded(t *testing.T) {
	s := testService(t)
	id := testOpen(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	_, err := s.Do(ctx, Request{Action: Read, ID: id, Wait: duration(time.Second)}, Agent)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read cancellation: %v", err)
	}
	untilText(t, s, Request{Action: Run, ID: id, Command: "printf 'STILL_HERE\\n'"}, "STILL_HERE\n")
	blocked, release := make(chan struct{}), make(chan struct{})
	other := New(Options{Resolve: func() (Defaults, error) { close(blocked); <-release; return Defaults{Shell: "/bin/sh"}, nil }})
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel2()
	started := time.Now()
	_, err = other.View(ctx2, Request{Action: Open})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("startup wasn't bounded: %v", err)
	}
	<-blocked
	close(release)
	if err := other.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if other.HasLive() {
		t.Fatal("canceled startup was published")
	}
}

func TestCloseStopsBackgroundJobs(t *testing.T) {
	s := testService(t)
	id := testOpen(t, s)
	r := untilText(t, s, Request{Action: Run, ID: id, Command: "sleep 30 & printf 'BG=%s\\n' $! "}, "BG=")
	var pid int
	for _, line := range strings.Split(r.Text, "\n") {
		if strings.HasPrefix(line, "BG=") {
			pid, _ = strconv.Atoi(strings.TrimPrefix(line, "BG="))
		}
	}
	if pid <= 0 {
		t.Fatalf("background PID missing: %q", r.Text)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if _, err := s.Do(context.Background(), Request{Action: Close, ID: id}, Agent); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("closing a terminal left its background job alive")
}

func TestFloodAndLogQuotaAreBounded(t *testing.T) {
	s := testService(t)
	id := testOpen(t, s)
	start := time.Now()
	r, err := s.Do(context.Background(), Request{Action: Run, ID: id, Command: "dd if=/dev/zero bs=65536 count=513 2>/dev/null", Wait: duration(time.Second)}, Agent)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second || len(r.Text) > MaxOutputBytes {
		t.Fatalf("flood escaped bounds: %d bytes in %s", len(r.Text), time.Since(start))
	}
	// Total log throughput depends on the host PTY and concurrent filesystem
	// tests. The per-operation wait/output assertions above remain strict.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for r.Terminal.State != "exited" {
		r, err = s.Do(ctx, Request{Action: Read, ID: id, Wait: duration(50 * time.Millisecond)}, Agent)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(r.Terminal.Error, "log limit") {
		t.Fatalf("missing quota failure: %+v", r.Terminal)
	}
	stat, err := os.Stat(r.Terminal.LogPath)
	if err != nil || stat.Size() != logBytes {
		t.Fatalf("log quota: %v, %v", stat, err)
	}
}

func TestReplayAcrossRingWrap(t *testing.T) {
	s := testService(t)
	id := testOpen(t, s)
	untilText(t, s, Request{Action: Run, ID: id, Command: "dd if=/dev/zero bs=65536 count=18 2>/dev/null | tr '\\000' x; printf 'RING_END\\n'"}, "RING_END\n")
	cursor := int64(0)
	var replay []byte
	for {
		r, err := s.View(context.Background(), Request{Action: Read, ID: id, After: &cursor, Wait: duration(0)})
		if err != nil {
			t.Fatal(err)
		}
		if cursor == 0 && (!r.Truncated || r.Start == 0) {
			t.Fatal("lost history was not reported")
		}
		replay = append(replay, r.Data...)
		cursor = r.Cursor
		if !r.HasMore {
			break
		}
	}
	want := append(bytes.Repeat([]byte("x"), bufferBytes-len("RING_END\r\n")), []byte("RING_END\r\n")...)
	if !bytes.Equal(replay, want) {
		t.Fatalf("replay changed byte order across wrap: %d bytes, expected %d", len(replay), len(want))
	}
}

func TestClosedTerminalsReleaseActiveSlots(t *testing.T) {
	s := testService(t)
	for i := 0; i <= maxTerminals; i++ {
		r, err := s.Do(context.Background(), Request{Action: Open, Wait: duration(0)}, Agent)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if _, err := s.Do(context.Background(), Request{Action: Close, ID: r.Terminal.ID}, Agent); err != nil {
			t.Fatal(err)
		}
	}
}
