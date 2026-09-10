package terminal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	pty "github.com/aymanbagabas/go-pty"
)

type Service struct {
	mu        sync.Mutex
	options   Options
	terminals map[string]*terminal
	closed    bool
	starting  int
	startup   sync.WaitGroup
}

// New is lazy: it allocates neither a shell nor an artifact until Open.
func New(options Options) *Service {
	options.Environment = append([]string(nil), options.Environment...)
	return &Service{options: options, terminals: make(map[string]*terminal)}
}

type span struct {
	start, end int64
	actor      Actor
}

type terminal struct {
	mu          sync.Mutex
	info        Info
	pty         pty.Pty
	cmd         *pty.Cmd
	log         *os.File
	buffer      []byte
	base, end   int64
	spans       []span
	actor       Actor
	agentCursor int64
	changed     chan struct{}
	done        chan struct{}
	readDone    chan struct{}
	inputGate   chan struct{}
	agentGate   chan struct{}
	stopOnce    sync.Once
	closeOnce   sync.Once
	closeDone   chan struct{}
}

// Do projects the stream for a model. View projects the same stream as raw
// bytes for a surface. The actor is selected by the caller, never by tool input.
func (s *Service) Do(ctx context.Context, req Request, actor Actor) (Result, error) {
	return s.execute(ctx, req, actor, false)
}

func (s *Service) View(ctx context.Context, req Request) (Result, error) {
	return s.execute(ctx, req, User, true)
}

func (s *Service) execute(ctx context.Context, req Request, actor Actor, raw bool) (Result, error) {
	if s == nil {
		return Result{}, ErrClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, MaxWait)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := validate(req); err != nil {
		return Result{}, err
	}
	if req.Action == List {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			return Result{}, ErrClosed
		}
		items := make([]Info, 0, len(s.terminals))
		for _, t := range s.terminals {
			items = append(items, t.snapshot())
		}
		sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.Before(items[j].CreatedAt) })
		return Result{Terminals: items}, nil
	}
	var t *terminal
	var err error
	if req.Action == Open {
		t, err = s.open(ctx, req, actor)
	} else {
		s.mu.Lock()
		if s.closed {
			err = ErrClosed
		} else {
			t = s.terminals[req.ID]
			if t == nil {
				err = ErrNotFound
			}
		}
		s.mu.Unlock()
	}
	if err != nil {
		return Result{}, err
	}
	// Serialize the model's implicit cursor. Explicit surface cursors never
	// acquire this gate and remain live while the Agent waits for output.
	if !raw {
		select {
		case t.agentGate <- struct{}{}:
			defer func() { <-t.agentGate }()
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	if req.Action == Close {
		t.stop("")
		select {
		case <-t.done:
		case <-time.After(settleTimeout):
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
		info := t.snapshot()
		return Result{Terminal: &info}, nil
	}
	if req.Action == Resize {
		if err := t.pty.Resize(req.Cols, req.Rows); err != nil {
			return Result{}, err
		}
		t.mu.Lock()
		t.info.Cols, t.info.Rows = req.Cols, req.Rows
		t.mu.Unlock()
		info := t.snapshot()
		return Result{Terminal: &info}, nil
	}
	t.mu.Lock()
	after := t.agentCursor
	if raw {
		after = 0
	}
	if req.After != nil {
		after = *req.After
	}
	if req.Action == Run || req.Action == Write || req.Action == Interrupt {
		after = t.end
	}
	t.mu.Unlock()
	input := ""
	switch req.Action {
	case Run:
		input = req.Command + "\n"
	case Write:
		input = req.Input
	case Interrupt:
		input = "\x03"
	}
	if input != "" {
		if err := t.write(ctx, input, actor); err != nil {
			return Result{}, err
		}
	}
	result, err := t.read(ctx, req, after, raw)
	if !raw && err == nil {
		t.mu.Lock()
		t.agentCursor = result.Cursor
		t.mu.Unlock()
	}
	return result, err
}

func validate(req Request) error {
	switch req.Action {
	case Open, Run, Read, Write, Resize, Interrupt, Close, List:
	default:
		return fmt.Errorf("unknown terminal action %q", req.Action)
	}
	if req.Action != List && req.Action != Open && strings.TrimSpace(req.ID) == "" {
		return errors.New("terminal id is required")
	}
	if req.Action == Run && strings.TrimSpace(req.Command) == "" {
		return errors.New("command is required")
	}
	if len(req.Command)+1 > MaxInputBytes || len(req.Input) > MaxInputBytes {
		return fmt.Errorf("terminal input exceeds %d bytes", MaxInputBytes)
	}
	if len(req.Title) > 120 {
		return errors.New("terminal title exceeds 120 bytes")
	}
	if req.Wait != nil && (*req.Wait < 0 || *req.Wait > MaxWait) {
		return fmt.Errorf("wait must be between 0 and %s", MaxWait)
	}
	if req.MaxBytes < 0 || req.MaxBytes > MaxOutputBytes || req.MaxLines < 0 || req.MaxLines > MaxOutputLines {
		return fmt.Errorf("output limits exceed %d bytes / %d lines", MaxOutputBytes, MaxOutputLines)
	}
	if req.After != nil && *req.After < 0 {
		return errors.New("cursor must be nonnegative")
	}
	if req.Mode != "" && req.Mode != "since" && req.Mode != "tail" && req.Mode != "follow" {
		return errors.New("mode must be since, tail, or follow")
	}
	if req.Cols < 0 || req.Cols > 500 || req.Rows < 0 || req.Rows > 300 {
		return errors.New("terminal size exceeds 500 columns / 300 rows")
	}
	if req.Action == Resize && (req.Cols < 2 || req.Rows < 2) {
		return errors.New("terminal size must be at least 2 columns / rows")
	}
	return nil
}

func (s *Service) open(ctx context.Context, req Request, actor Actor) (*terminal, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	active := s.starting
	for _, item := range s.terminals {
		if item.snapshot().State == "running" {
			active++
		}
	}
	if active >= maxTerminals {
		s.mu.Unlock()
		return nil, fmt.Errorf("session terminal limit (%d) reached", maxTerminals)
	}
	s.starting++
	s.startup.Add(1)
	s.mu.Unlock()
	type opened struct {
		terminal *terminal
		err      error
	}
	ready := make(chan opened)
	go func() {
		defer s.startup.Done()
		t, err := s.start(req, actor)
		s.mu.Lock()
		s.starting--
		closed := s.closed
		if err == nil && !closed && ctx.Err() == nil {
			s.pruneLocked()
			s.terminals[t.info.ID] = t
		}
		s.mu.Unlock()
		if closed && err == nil {
			err = ErrClosed
		}
		cleanup := err != nil
		select {
		case ready <- opened{t, err}:
		case <-ctx.Done():
			cleanup = true
		}
		if cleanup && t != nil {
			t.stop("")
			<-t.done
			<-t.readDone
			<-t.closeDone
		}
	}()
	select {
	case result := <-ready:
		return result.terminal, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Retain a bounded set of exited terminals for reopening. Their log files
// remain available at the paths already returned to the caller.
func (s *Service) pruneLocked() {
	for len(s.terminals) >= maxHistory {
		var oldest *terminal
		for _, item := range s.terminals {
			info := item.snapshot()
			if info.State == "exited" && (oldest == nil || info.CreatedAt.Before(oldest.info.CreatedAt)) {
				oldest = item
			}
		}
		if oldest == nil {
			return
		}
		delete(s.terminals, oldest.info.ID)
	}
}

// No partially initialized terminal is published. A canceled startup always
// tears down its handles, even if the OS returns after the caller's deadline.
func (s *Service) start(req Request, actor Actor) (*terminal, error) {
	var token [8]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	id := "term_" + hex.EncodeToString(token[:])
	t := &terminal{info: Info{ID: id, Title: req.Title, CreatedAt: time.Now(), State: "running"}, actor: actor,
		changed: make(chan struct{}), done: make(chan struct{}), readDone: make(chan struct{}), closeDone: make(chan struct{}),
		inputGate: make(chan struct{}, 1), agentGate: make(chan struct{}, 1)}
	defaults := Defaults{CWD: s.options.CWD, Shell: s.options.Shell, Environment: s.options.Environment}
	if s.options.Resolve != nil {
		var err error
		defaults, err = s.options.Resolve()
		if err != nil {
			return nil, err
		}
	}
	if req.CWD != "" {
		if filepath.IsAbs(req.CWD) {
			defaults.CWD = req.CWD
		} else {
			defaults.CWD = filepath.Join(defaults.CWD, req.CWD)
		}
	}
	shell, args, err := shellCommand(defaults.Shell)
	if err != nil {
		return nil, err
	}
	termPTY, err := pty.New()
	if err != nil {
		return nil, fmt.Errorf("open PTY: %w", err)
	}
	prepared, err := preparePTY(termPTY)
	if err != nil {
		_ = termPTY.Close()
		return nil, err
	}
	termPTY = prepared
	cols, rows := req.Cols, req.Rows
	if cols == 0 {
		cols = 100
	}
	if rows == 0 {
		rows = 30
	}
	if err = termPTY.Resize(cols, rows); err != nil {
		_ = termPTY.Close()
		return nil, err
	}
	dir, err := os.MkdirTemp("", "pi-terminal-")
	if err != nil {
		_ = termPTY.Close()
		return nil, err
	}
	log, err := os.OpenFile(filepath.Join(dir, id+".log"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		_ = termPTY.Close()
		return nil, err
	}
	cmd := termPTY.Command(shell, args...)
	cmd.Dir = defaults.CWD
	cmd.Env = append([]string(nil), defaults.Environment...)
	if defaults.Environment == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, "TERM=xterm-256color", "COLORTERM=truecolor")
	if err = cmd.Start(); err != nil {
		_ = termPTY.Close()
		_ = log.Close()
		return nil, fmt.Errorf("start terminal: %w", err)
	}
	if unixPTY, ok := termPTY.(pty.UnixPty); ok {
		_ = unixPTY.Slave().Close()
	}
	t.pty, t.cmd, t.log = termPTY, cmd, log
	t.info.CWD, t.info.Shell, t.info.Cols, t.info.Rows, t.info.LogPath = defaults.CWD, shell, cols, rows, log.Name()
	if t.info.Title == "" {
		t.info.Title = filepath.Base(shell)
	}
	go t.pump()
	go t.wait()
	return t, nil
}

func shellCommand(configured string) (string, []string, error) {
	shell := configured
	if shell == "" {
		if runtime.GOOS == "windows" {
			shell = os.Getenv("COMSPEC")
			if shell == "" {
				shell = "cmd.exe"
			}
		} else {
			shell = os.Getenv("SHELL")
			if shell == "" {
				shell = "/bin/sh"
			}
		}
	}
	path, err := exec.LookPath(shell)
	if err != nil {
		return "", nil, err
	}
	name := strings.ToLower(strings.TrimSuffix(filepath.Base(path), ".exe"))
	args := []string{"-i"}
	if name == "cmd" {
		args = []string{"/Q"}
	} else if name == "pwsh" || name == "powershell" {
		args = []string{"-NoLogo"}
	}
	return path, args, nil
}

func (t *terminal) snapshot() Info {
	t.mu.Lock()
	defer t.mu.Unlock()
	info := t.info
	if info.ExitCode != nil {
		code := *info.ExitCode
		info.ExitCode = &code
	}
	return info
}

func (t *terminal) notifyLocked() { close(t.changed); t.changed = make(chan struct{}) }

func (t *terminal) pump() {
	defer close(t.readDone)
	defer t.log.Close()
	buf := make([]byte, 16*1024)
	for {
		n, err := t.pty.Read(buf)
		if n > 0 {
			t.mu.Lock()
			remaining := int64(logBytes) - t.end
			actor := t.actor
			t.mu.Unlock()
			count := min(n, int(remaining))
			written, logErr := t.log.Write(buf[:count])
			if logErr == nil && written != count {
				logErr = io.ErrShortWrite
			}
			t.mu.Lock()
			if written > 0 {
				start := t.end
				if t.buffer == nil {
					t.buffer = make([]byte, bufferBytes)
				}
				position := int(t.end % bufferBytes)
				copied := copy(t.buffer[position:], buf[:written])
				copy(t.buffer, buf[copied:written])
				t.end += int64(written)
				t.base = max(t.base, t.end-bufferBytes)
				if len(t.spans) > 0 && t.spans[len(t.spans)-1].actor == actor {
					t.spans[len(t.spans)-1].end = t.end
				} else {
					t.spans = append(t.spans, span{start, t.end, actor})
				}
				for len(t.spans) > 0 && t.spans[0].end <= t.base {
					t.spans = t.spans[1:]
				}
				if len(t.spans) > maxSpans {
					t.spans = t.spans[len(t.spans)-maxSpans:]
					t.base = t.spans[0].start
				}
				t.notifyLocked()
			}
			t.mu.Unlock()
			if logErr != nil {
				t.stop("terminal log: " + logErr.Error())
				return
			}
			if count < n || t.end >= logBytes {
				t.stop("terminal stopped: 32 MiB output log limit reached")
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) && !errors.Is(err, syscall.EIO) {
				t.stop("terminal output: " + err.Error())
			}
			return
		}
	}
}

func (t *terminal) wait() {
	err := t.cmd.Wait()
	// Unix drains to EOF after closing our slave handle. ConPTY may need Close
	// to flush; run it concurrently with the reader, as required by Windows.
	if runtime.GOOS == "windows" {
		t.closePTY()
	}
	select {
	case <-t.readDone:
	case <-time.After(settleTimeout):
		t.stop("terminal output drain exceeded its deadline")
		<-t.readDone
	}
	t.closePTY()
	t.mu.Lock()
	t.info.State = "exited"
	if t.cmd.ProcessState != nil {
		code := t.cmd.ProcessState.ExitCode()
		t.info.ExitCode = &code
	}
	if err != nil && t.info.ExitCode == nil && t.info.Error == "" {
		t.info.Error = err.Error()
	}
	t.notifyLocked()
	t.mu.Unlock()
	close(t.done)
}

func (t *terminal) closePTY() {
	t.closeOnce.Do(func() { go func() { _ = t.pty.Close(); close(t.closeDone) }() })
}

func (t *terminal) stop(reason string) {
	t.stopOnce.Do(func() {
		if reason != "" {
			t.mu.Lock()
			t.info.Error = reason
			t.notifyLocked()
			t.mu.Unlock()
		}
		terminate(t.pty, t.cmd.Process)
		t.closePTY()
	})
}

func (t *terminal) write(ctx context.Context, input string, actor Actor) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	select {
	case t.inputGate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	t.mu.Lock()
	if t.info.State == "exited" {
		t.mu.Unlock()
		<-t.inputGate
		return ErrExited
	}
	t.actor = actor
	t.mu.Unlock()
	done := make(chan error, 1)
	go func() { _, err := io.WriteString(t.pty, input); <-t.inputGate; done <- err }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		// A blocked partial write must not arrive later after the caller has
		// retried. Close this terminal instead of leaving a queued command.
		t.stop("terminal stopped: input write timed out")
		return ctx.Err()
	}
}

func (s *Service) HasLive() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.terminals {
		if t.snapshot().State == "running" {
			return true
		}
	}
	return false
}

func (s *Service) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, settleTimeout)
	defer cancel()
	s.mu.Lock()
	s.closed = true
	items := make([]*terminal, 0, len(s.terminals))
	for _, t := range s.terminals {
		items = append(items, t)
	}
	s.mu.Unlock()
	for _, t := range items {
		t.stop("")
	}
	started := make(chan struct{})
	go func() { s.startup.Wait(); close(started) }()
	select {
	case <-started:
	case <-ctx.Done():
		return ctx.Err()
	}
	for _, t := range items {
		for _, done := range []<-chan struct{}{t.done, t.readDone, t.closeDone} {
			select {
			case <-done:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return nil
}
