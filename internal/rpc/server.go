package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/cat3399/pi-go/internal/application"
)

const maxJSONLineBytes = 64 << 20

type dispatcher interface {
	Dispatch(context.Context, application.Command) (application.CommandResult, error)
	Subscribe(application.SessionObserver) func()
	Dispose(context.Context) error
}

type Server struct {
	backend dispatcher
	input   io.Reader
	output  *recordWriter
}

func NewServer(backend dispatcher, input io.Reader, output io.Writer) (*Server, error) {
	if backend == nil || input == nil || output == nil {
		return nil, errors.New("RPC backend, input, and output are required")
	}
	return &Server{backend: backend, input: input, output: newRecordWriter(output)}, nil
}

// Serve reads strict LF-delimited JSON commands. Command handlers run
// concurrently, matching pi's RPC mode and allowing abort commands to reach a
// long-running bash or compaction operation.
func (s *Server) Serve(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	unsubscribe := s.backend.Subscribe(func(_ context.Context, event application.Event) {
		record, err := encodeApplicationEvent(event)
		if err == nil {
			err = s.output.Write(record)
		}
		if err != nil {
			s.output.Fail(err)
			cancel()
		}
	})
	defer unsubscribe()

	lines := make(chan []byte)
	scanDone := make(chan error, 1)
	go scanJSONLines(serveCtx, s.input, lines, scanDone)

	var commands sync.WaitGroup
	reading := true
	inputEnded := false
	for reading {
		select {
		case line, ok := <-lines:
			if !ok {
				inputEnded = true
				reading = false
				continue
			}
			commands.Add(1)
			go func() {
				defer commands.Done()
				s.handleLine(serveCtx, line)
			}()
		case <-serveCtx.Done():
			reading = false
		case <-s.output.Failed():
			reading = false
		}
	}
	cancel()

	// Let every command already framed from stdin reach ApplicationSession before closing
	// it. Their request contexts are now cancelled, so long-running bash and
	// compaction calls return promptly, while state/config commands cannot lose
	// a race to ApplicationSession.Dispose merely because the writer closed stdin.
	commands.Wait()
	// Runtime disposal aborts and settles every admitted operation. The application
	// disposal barrier guarantees its final events reach this subscription.
	disposeErr := s.backend.Dispose(context.Background())
	var scanErr error
	if inputEnded {
		scanErr = <-scanDone
	}
	if outputErr := s.output.Err(); outputErr != nil {
		return outputErr
	}
	if disposeErr != nil {
		return fmt.Errorf("dispose RPC application session: %w", disposeErr)
	}
	if scanErr != nil {
		return scanErr
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return nil
}

func (s *Server) handleLine(ctx context.Context, line []byte) {
	decoded, err := decodeCommand(line)
	if err != nil {
		_ = s.output.Write(errorResponse(decoded.id, decoded.typ, err))
		return
	}

	result, err := s.backend.Dispatch(ctx, decoded.command)
	if err != nil {
		_ = s.output.Write(errorResponse(decoded.id, decoded.typ, err))
		return
	}
	data, err := encodeResult(result)
	if err != nil {
		_ = s.output.Write(errorResponse(decoded.id, decoded.typ, err))
		return
	}
	_ = s.output.Write(successResponse(decoded.id, decoded.typ, data))
}

func scanJSONLines(ctx context.Context, input io.Reader, lines chan<- []byte, done chan<- error) {
	defer close(lines)
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64<<10), maxJSONLineBytes)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		select {
		case lines <- line:
		case <-ctx.Done():
			done <- context.Cause(ctx)
			return
		}
	}
	if err := scanner.Err(); err != nil {
		done <- fmt.Errorf("read RPC input: %w", err)
		return
	}
	done <- nil
}

type recordWriter struct {
	mu     sync.Mutex
	writer *bufio.Writer
	err    error
	failed chan struct{}
	once   sync.Once
}

func newRecordWriter(output io.Writer) *recordWriter {
	return &recordWriter{writer: bufio.NewWriter(output), failed: make(chan struct{})}
}

func (w *recordWriter) Write(value any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	encoded, err := json.Marshal(value)
	if err == nil {
		_, err = w.writer.Write(encoded)
	}
	if err == nil {
		err = w.writer.WriteByte('\n')
	}
	if err == nil {
		err = w.writer.Flush()
	}
	if err != nil {
		w.err = err
		w.once.Do(func() { close(w.failed) })
	}
	return err
}

func (w *recordWriter) Fail(err error) {
	if err == nil {
		return
	}
	w.mu.Lock()
	if w.err == nil {
		w.err = err
		w.once.Do(func() { close(w.failed) })
	}
	w.mu.Unlock()
}

func (w *recordWriter) Failed() <-chan struct{} { return w.failed }

func (w *recordWriter) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}
