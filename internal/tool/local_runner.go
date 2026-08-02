package tool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const postExitIdleGrace = 100 * time.Millisecond

type LocalRunnerOptions struct {
	ShellPath string
}

// LocalRunner executes the built-in Bash command directly on the host. Each
// command owns a fresh process group/tree and two capture pipes.
type LocalRunner struct {
	shellPath string
}

func NewLocalRunner(options LocalRunnerOptions) *LocalRunner {
	return &LocalRunner{shellPath: options.ShellPath}
}

type pipeEventKind uint8

const (
	pipeData pipeEventKind = iota + 1
	pipeEOF
	pipeFailure
)

type pipeEvent struct {
	stream int
	kind   pipeEventKind
	data   []byte
	err    error
}

type processWaitResult struct {
	status ExitStatus
	err    error
}

// localProcessTree is the platform-owned lifetime handle for a started shell
// and every descendant that remains in its controllable process tree. On a
// normal successful return release must leave background descendants alone;
// terminate is used only for cancellation or execution failure.
type localProcessTree interface {
	terminate() error
	settleTermination() error
	release() error
}

func (r *LocalRunner) Run(ctx context.Context, request RunRequest, sink OutputSink) (ExitStatus, error) {
	if ctx == nil {
		return ExitStatus{}, newRunnerFailure(RunnerFailureSetup, errors.New("context is nil"))
	}
	if sink == nil {
		return ExitStatus{}, newRunnerFailure(RunnerFailureSetup, errors.New("output sink is nil"))
	}
	if cause := context.Cause(ctx); cause != nil {
		return ExitStatus{}, newRunInterruptedError(cause)
	}

	shell, err := resolveShell(r.shellPath, request)
	if err != nil {
		return ExitStatus{}, newRunnerFailure(RunnerFailureSetup, err)
	}
	arguments := append([]string(nil), shell.arguments...)
	if !shell.commandOnStdin {
		arguments = append(arguments, request.command)
	}
	command := exec.Command(shell.path, arguments...)
	command.Dir = request.workingDir
	command.Env = append([]string(nil), request.environment...)
	if shell.commandOnStdin {
		command.Stdin = strings.NewReader(request.command)
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		return ExitStatus{}, newRunnerFailure(RunnerFailureCapture, fmt.Errorf("create stdout pipe: %w", err))
	}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
		return ExitStatus{}, newRunnerFailure(RunnerFailureCapture, fmt.Errorf("create stderr pipe: %w", err))
	}
	command.Stdout = stdoutWriter
	command.Stderr = stderrWriter

	if cause := context.Cause(ctx); cause != nil {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
		_ = stderrReader.Close()
		_ = stderrWriter.Close()
		return ExitStatus{}, newRunInterruptedError(cause)
	}
	processTree, err := startLocalProcess(command)
	if err != nil {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
		_ = stderrReader.Close()
		_ = stderrWriter.Close()
		var runnerFailure *RunnerFailure
		if errors.As(err, &runnerFailure) {
			return ExitStatus{}, err
		}
		return ExitStatus{}, newRunnerFailure(RunnerFailureSpawn, err)
	}
	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()

	events := make(chan pipeEvent, 16)
	readerStop := make(chan struct{})
	var stopReaders sync.Once
	var readers sync.WaitGroup
	var lastActivity atomic.Int64
	activityEpoch := time.Now()
	markActivity(&lastActivity, time.Since(activityEpoch))
	readers.Add(2)
	go readProcessPipe(1, stdoutReader, events, readerStop, &readers, &lastActivity, activityEpoch)
	go readProcessPipe(2, stderrReader, events, readerStop, &readers, &lastActivity, activityEpoch)

	waited := make(chan processWaitResult, 1)
	go func() {
		waited <- interpretProcessWait(command.Wait(), command.ProcessState)
	}()

	closeReaders := func() {
		stopReaders.Do(func() {
			close(readerStop)
			_ = stdoutReader.Close()
			_ = stderrReader.Close()
		})
		readers.Wait()
	}

	var (
		status               ExitStatus
		exited               bool
		stdoutEnded          bool
		stderrEnded          bool
		interrupted          error
		captureFailure       error
		waitFailure          error
		cleanupFailure       error
		terminationErr       error
		terminationRequested bool
		idleTimer            *time.Timer
		idleTimerC           <-chan time.Time
		contextDone          = ctx.Done()
	)

	stopIdleTimer := func() {
		if idleTimer == nil {
			idleTimerC = nil
			return
		}
		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
		idleTimerC = nil
	}
	armIdleTimer := func(delay time.Duration) {
		if delay <= 0 {
			delay = time.Nanosecond
		}
		if idleTimer == nil {
			idleTimer = time.NewTimer(delay)
		} else {
			stopIdleTimer()
			idleTimer.Reset(delay)
		}
		idleTimerC = idleTimer.C
	}
	terminate := func() {
		terminationRequested = true
		if err := processTree.terminate(); err != nil {
			terminationErr = errors.Join(terminationErr, err)
		}
	}
	observeInterruption := func() {
		cause := context.Cause(ctx)
		if cause == nil || interrupted != nil || captureFailure != nil {
			return
		}
		interrupted = cause
		contextDone = nil
		terminate()
	}
	finish := func() (ExitStatus, error) {
		stopIdleTimer()
		// Establish cancellation-before-settlement ordering even when Wait,
		// pipe EOF, the idle timer, and ctx.Done become ready together.
		observeInterruption()
		closeReaders()
		observeInterruption()
		if terminationRequested {
			if err := processTree.settleTermination(); err != nil {
				terminationErr = errors.Join(terminationErr, err)
			}
		}
		if err := processTree.release(); err != nil {
			cleanupFailure = err
		}
		withCleanup := func(primary error) error {
			return errors.Join(primary, terminationErr, cleanupFailure)
		}
		switch {
		case captureFailure != nil:
			return status, withCleanup(newRunnerFailure(RunnerFailureCapture, captureFailure))
		case interrupted != nil:
			return status, withCleanup(newRunInterruptedError(interrupted))
		case waitFailure != nil:
			return status, withCleanup(newRunnerFailure(RunnerFailureWait, waitFailure))
		case cleanupFailure != nil:
			return status, errors.Join(newRunnerFailure(RunnerFailureCleanup, cleanupFailure), terminationErr)
		default:
			return status, withCleanup(nil)
		}
	}
	handlePipeEvent := func(event pipeEvent) {
		switch event.kind {
		case pipeData:
			if captureFailure == nil {
				if err := callOutputSink(sink, event.data); err != nil {
					captureFailure = err
					terminate()
				}
			}
			if exited && interrupted == nil && captureFailure == nil && waitFailure == nil {
				markActivity(&lastActivity, time.Since(activityEpoch))
				armIdleTimer(postExitIdleGrace)
			}
		case pipeEOF:
			if event.stream == 1 {
				stdoutEnded = true
			} else {
				stderrEnded = true
			}
		case pipeFailure:
			if captureFailure == nil {
				captureFailure = event.err
				terminate()
			}
		}
	}

	for {
		if exited && (stdoutEnded && stderrEnded ||
			interrupted != nil ||
			captureFailure != nil ||
			waitFailure != nil) {
			return finish()
		}

		select {
		case event := <-events:
			handlePipeEvent(event)

		case waitResult := <-waited:
			exited = true
			status = waitResult.status
			if interrupted != nil || captureFailure != nil {
				// Sweep the process group again after the direct child is reaped.
				// A shell can race the first group signal by forking a child at
				// the same instant; after Wait it can no longer create descendants.
				terminate()
			}
			if waitResult.err != nil && captureFailure == nil && interrupted == nil {
				waitFailure = waitResult.err
			}
			if interrupted == nil && captureFailure == nil && waitFailure == nil && !(stdoutEnded && stderrEnded) {
				markActivity(&lastActivity, time.Since(activityEpoch))
				armIdleTimer(postExitIdleGrace)
			}

		case <-contextDone:
			observeInterruption()

		case <-idleTimerC:
			draining := true
			for draining {
				select {
				case event := <-events:
					handlePipeEvent(event)
				default:
					draining = false
				}
			}
			if stdoutEnded && stderrEnded || interrupted != nil || captureFailure != nil || waitFailure != nil {
				return finish()
			}
			quietFor := time.Since(activityEpoch) - time.Duration(lastActivity.Load())
			if quietFor < postExitIdleGrace {
				armIdleTimer(postExitIdleGrace - quietFor)
				continue
			}
			return finish()
		}
	}
}

func readProcessPipe(
	stream int,
	reader *os.File,
	events chan<- pipeEvent,
	stop <-chan struct{},
	waitGroup *sync.WaitGroup,
	lastActivity *atomic.Int64,
	activityEpoch time.Time,
) {
	defer waitGroup.Done()
	buffer := make([]byte, 32*1024)
	for {
		count, err := reader.Read(buffer)
		if count > 0 {
			markActivity(lastActivity, time.Since(activityEpoch))
			event := pipeEvent{stream: stream, kind: pipeData, data: append([]byte(nil), buffer[:count]...)}
			select {
			case events <- event:
			case <-stop:
				return
			}
		}
		if err == nil {
			continue
		}
		event := pipeEvent{stream: stream, kind: pipeEOF}
		if !errors.Is(err, io.EOF) {
			select {
			case <-stop:
				return
			default:
				event.kind = pipeFailure
				event.err = err
			}
		}
		select {
		case events <- event:
		case <-stop:
		}
		return
	}
}

func markActivity(target *atomic.Int64, elapsed time.Duration) {
	value := int64(elapsed)
	for {
		current := target.Load()
		if current >= value || target.CompareAndSwap(current, value) {
			return
		}
	}
}

func interpretProcessWait(waitErr error, state *os.ProcessState) processWaitResult {
	status := UnknownExitStatus()
	if state != nil {
		code := state.ExitCode()
		if code >= 0 {
			status, _ = NewExitStatus(code)
		}
	}
	if waitErr == nil {
		return processWaitResult{status: status}
	}
	var exitError *exec.ExitError
	if errors.As(waitErr, &exitError) {
		return processWaitResult{status: status}
	}
	return processWaitResult{status: status, err: waitErr}
}

func callOutputSink(sink OutputSink, data []byte) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("output sink panicked with %T: %v", recovered, recovered)
		}
	}()
	return sink(data)
}
