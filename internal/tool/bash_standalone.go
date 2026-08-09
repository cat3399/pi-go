package tool

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"unicode/utf8"
)

// StandaloneBashResult is the user-initiated shell result used by
// coding-agent's !/!! command. Unlike the model-facing BashResult, a non-zero
// or unavailable exit code is data rather than a tool failure.
type StandaloneBashResult struct {
	Output         string
	ExitCode       *int
	Cancelled      bool
	Truncated      bool
	FullOutputPath string
}

// ExecuteStandalone runs a user-initiated command through the same configured
// shell, cwd, environment, process-tree cancellation, truncation, and artifact
// store as the built-in bash tool. Its display/storage stream follows
// coding-agent's bash-executor path: ANSI and unsafe control characters are
// removed, carriage returns are normalized away, and each sanitized chunk is
// reported in arrival order.
func (b *Bash) ExecuteStandalone(ctx context.Context, command string, onChunk func(string)) (StandaloneBashResult, error) {
	if b == nil {
		return StandaloneBashResult{}, errors.New("bash tool is nil")
	}
	input, err := NewBashInput(command, nil)
	if err != nil {
		return StandaloneBashResult{}, err
	}
	if ctx == nil {
		return StandaloneBashResult{}, errors.New("context is nil")
	}
	if cause := context.Cause(ctx); cause != nil {
		return StandaloneBashResult{Cancelled: true}, nil
	}
	workingDir, environment, err := b.resolveExecutionContext(BashExecutionContext{})
	if err != nil {
		return StandaloneBashResult{}, err
	}
	if err := validateWorkingDirectory(workingDir); err != nil {
		return StandaloneBashResult{}, err
	}
	accumulator, err := newOutputAccumulator(b.maxLines, b.maxBytes, b.store)
	if err != nil {
		return StandaloneBashResult{}, err
	}

	runContext, cancelRun := context.WithCancelCause(ctx)
	state := &standaloneBashOutputState{
		accepting: true, accumulator: accumulator, artifactThreshold: b.maxBytes, cancel: cancelRun, onChunk: onChunk,
	}
	status, runErr := b.runner.Run(
		runContext,
		newRunRequest(input.Command(), workingDir, environment),
		state.append,
	)
	snapshot, outputErr := state.settle()
	cancelRun(context.Canceled)
	if outputErr != nil {
		return standaloneResultFromSnapshot(snapshot, false, ExitStatus{}), outputErr
	}
	if cause := context.Cause(ctx); cause != nil {
		return standaloneResultFromSnapshot(snapshot, true, ExitStatus{}), nil
	}
	if runErr != nil {
		if isInterruption(runErr) {
			return standaloneResultFromSnapshot(snapshot, true, ExitStatus{}), nil
		}
		return standaloneResultFromSnapshot(snapshot, false, status), runErr
	}
	return standaloneResultFromSnapshot(snapshot, false, status), nil
}

func standaloneResultFromSnapshot(snapshot outputSnapshot, cancelled bool, status ExitStatus) StandaloneBashResult {
	result := StandaloneBashResult{
		Output: snapshot.content, Cancelled: cancelled, Truncated: snapshot.truncation.truncated,
	}
	if code, ok := status.Code(); ok && !cancelled {
		result.ExitCode = &code
	}
	if snapshot.artifactComplete {
		result.FullOutputPath = snapshot.fullOutputPath
	}
	return result
}

type standaloneBashOutputState struct {
	mu          sync.Mutex
	accepting   bool
	accumulator *outputAccumulator
	// bash-executor starts preserving the sanitized full stream as soon as the
	// raw process stream crosses DEFAULT_MAX_BYTES, even when ANSI removal leaves
	// the final visible result below the truncation limit.
	artifactThreshold int
	rawBytes          uint64
	cancel            context.CancelCauseFunc
	onChunk           func(string)
	decoder           standaloneTextDecoder
	firstErr          error
}

func (s *standaloneBashOutputState) append(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.accepting {
		return nil
	}
	if s.firstErr != nil {
		return s.firstErr
	}
	s.rawBytes = saturatingAdd(s.rawBytes, uint64(len(data)))
	return s.appendDecoded(s.decoder.decode(data, false))
}

func (s *standaloneBashOutputState) appendDecoded(decoded string) error {
	text := sanitizeStandaloneOutput(decoded)
	if err := s.accumulator.append([]byte(text)); err != nil {
		s.firstErr = err
		s.cancel(err)
		return err
	}
	if s.rawBytes > uint64(s.artifactThreshold) && !s.accumulator.artifactComplete && s.accumulator.artifactPath == "" {
		if err := s.accumulator.ensureArtifact(); err != nil {
			s.firstErr = err
			s.cancel(err)
			return err
		}
	}
	if s.onChunk != nil {
		s.onChunk(text)
	}
	return nil
}

func (s *standaloneBashOutputState) settle() (outputSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accepting = false
	if trailing := s.decoder.decode(nil, true); trailing != "" && s.firstErr == nil {
		_ = s.appendDecoded(trailing)
	}
	if err := s.accumulator.finish(); err != nil && s.firstErr == nil {
		s.firstErr = err
	}
	if err := s.accumulator.close(); err != nil && s.firstErr == nil {
		s.firstErr = err
	}
	return s.accumulator.snapshot(), s.firstErr
}

// standaloneTextDecoder mirrors TextDecoder.decode(..., {stream:true}) closely
// enough to keep UTF-8 split across process chunks intact.
type standaloneTextDecoder struct {
	pending       []byte
	atStreamStart bool
	started       bool
}

func (d *standaloneTextDecoder) decode(data []byte, final bool) string {
	combined := make([]byte, 0, len(d.pending)+len(data))
	combined = append(combined, d.pending...)
	combined = append(combined, data...)
	d.pending = nil
	var decoded strings.Builder
	for len(combined) > 0 {
		if !utf8.FullRune(combined) {
			if final {
				decoded.WriteRune(utf8.RuneError)
			} else {
				d.pending = bytes.Clone(combined)
			}
			break
		}
		value, width := utf8.DecodeRune(combined)
		combined = combined[width:]
		if !d.started {
			d.started = true
			d.atStreamStart = true
		}
		if d.atStreamStart {
			d.atStreamStart = false
			if value == '\ufeff' {
				continue
			}
		}
		decoded.WriteRune(value)
	}
	return decoded.String()
}

func sanitizeStandaloneOutput(value string) string {
	value = stripStandaloneANSI(value)
	var clean strings.Builder
	clean.Grow(len(value))
	for _, character := range value {
		switch {
		case character == '\t' || character == '\n':
			clean.WriteRune(character)
		case character == '\r':
			// bash-executor normalizes carriage returns away.
		case character <= 0x1f:
		case character >= 0xfff9 && character <= 0xfffb:
		default:
			clean.WriteRune(character)
		}
	}
	return clean.String()
}

// stripStandaloneANSI covers the OSC and CSI/control forms accepted by pi's
// ansi-regex derivative. Incomplete sequences are retained for the subsequent
// control-character sanitizer, matching per-chunk stripAnsi behavior.
func stripStandaloneANSI(value string) string {
	var clean strings.Builder
	for index := 0; index < len(value); {
		start := index
		if value[index] == 0x1b {
			if index+1 >= len(value) {
				clean.WriteByte(value[index])
				index++
				continue
			}
			if value[index+1] == ']' {
				if end, ok := standaloneOSCEnd(value, index+2); ok {
					index = end
					continue
				}
				clean.WriteString(value[start:])
				break
			}
			index++
			if strings.ContainsRune("[]()#;?", rune(value[index])) {
				index++
			}
		} else if value[index] == 0xc2 && index+1 < len(value) && value[index+1] == 0x9b {
			index += 2
		} else {
			clean.WriteByte(value[index])
			index++
			continue
		}
		for index < len(value) {
			character := value[index]
			index++
			if character >= 0x40 && character <= 0x7e {
				start = -1
				break
			}
		}
		if start >= 0 {
			clean.WriteString(value[start:index])
		}
	}
	return clean.String()
}

func standaloneOSCEnd(value string, index int) (int, bool) {
	for index < len(value) {
		switch value[index] {
		case 0x07:
			return index + 1, true
		case 0x1b:
			if index+1 < len(value) && value[index+1] == '\\' {
				return index + 2, true
			}
		case 0xc2:
			if index+1 < len(value) && value[index+1] == 0x9c {
				return index + 2, true
			}
		}
		index++
	}
	return 0, false
}
