package terminal

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

func (t *terminal) read(ctx context.Context, req Request, after int64, raw bool) (Result, error) {
	wait := DefaultWait
	if req.Wait != nil {
		wait = *req.Wait
	}
	maxBytes, maxLines := req.MaxBytes, req.MaxLines
	if maxBytes == 0 {
		maxBytes = MaxOutputBytes
	}
	if maxLines == 0 {
		maxLines = MaxOutputLines
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	var idle *time.Timer
	var idleC <-chan time.Time
	defer func() {
		if idle != nil {
			idle.Stop()
		}
	}()
	lastEnd := after
	reason := "data"
	for {
		t.mu.Lock()
		end, state, changed := t.end, t.info.State, t.changed
		t.mu.Unlock()
		if wait == 0 || req.Mode == "tail" {
			break
		}
		if state == "exited" {
			reason = "exited"
			break
		}
		if end-after >= int64(maxBytes) {
			reason = "limit"
			break
		}
		if end > after {
			if raw || req.Mode == "follow" {
				break
			}
			if idle == nil {
				idle = time.NewTimer(100 * time.Millisecond)
				idleC = idle.C
				lastEnd = end
			} else if end != lastEnd {
				idle.Reset(100 * time.Millisecond)
				lastEnd = end
			}
		}
		select {
		case <-changed:
		case <-idleC:
			reason = "idle"
			goto ready
		case <-deadline.C:
			reason = "deadline"
			goto ready
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
ready:
	t.mu.Lock()
	defer t.mu.Unlock()
	info := t.info
	result := Result{Terminal: &info, Reason: reason}
	start := min(after, t.end)
	if req.Mode == "tail" {
		start = max(t.base, t.end-int64(maxBytes))
	}
	if start < t.base {
		result.Truncated = true
		start = t.base
	}
	end := t.end
	if raw {
		end = min(end, start+int64(maxBytes))
		result.Data = t.bytesLocked(start, end)
		result.HasMore = end < t.end
	} else {
		// Output attribution follows the most recent input. This intentionally
		// handles ordinary shared use without shell markers or command wrapping.
		var text strings.Builder
		for _, span := range t.spans {
			if span.end <= start || span.start >= end || span.actor == User && !req.IncludeUser {
				continue
			}
			lo, hi := max(start, span.start), min(end, span.end)
			text.Write(t.bytesLocked(lo, hi))
		}
		output := strings.ReplaceAll(ansi.Strip(text.String()), "\r\n", "\n")
		output = strings.ToValidUTF8(output, "�")
		// Tail truncation follows bash: preserve the most recent output and
		// leave the complete raw stream in the private temporary log.
		if len(output) > maxBytes {
			output = output[len(output)-maxBytes:]
			for len(output) > 0 && !utf8.RuneStart(output[0]) {
				output = output[1:]
			}
			result.Truncated = true
		}
		lines := strings.Split(output, "\n")
		if len(lines) > maxLines {
			output = strings.Join(lines[len(lines)-maxLines:], "\n")
			result.Truncated = true
		}
		result.Text = output
	}
	result.Start, result.Cursor = start, end
	return result, nil
}

// Absolute stream positions remain stable when the bounded ring wraps.
func (t *terminal) bytesLocked(start, end int64) []byte {
	result := make([]byte, end-start)
	if len(result) == 0 {
		return result
	}
	position := int(start % bufferBytes)
	copied := copy(result, t.buffer[position:])
	copy(result[copied:], t.buffer[:len(result)-copied])
	return result
}
