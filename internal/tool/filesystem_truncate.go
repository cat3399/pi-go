package tool

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// FilesystemTruncation describes a text-safe prefix. Output never splits a
// UTF-8 sequence or a line; callers can use NextOffset for continuation.
type FilesystemTruncation struct {
	Content        string
	Truncated      bool
	TruncatedBy    string
	TotalLines     int
	TotalBytes     int
	OutputLines    int
	OutputBytes    int
	FirstLineLarge bool
}

func truncateFilesystemHead(content string, maxLines, maxBytes int) FilesystemTruncation {
	lines := splitLogicalLines(content)
	totalBytes := len(content)
	result := FilesystemTruncation{TotalLines: len(lines), TotalBytes: totalBytes}
	if len(lines) <= maxLines && totalBytes <= maxBytes {
		result.Content = content
		result.OutputLines = len(lines)
		result.OutputBytes = totalBytes
		return result
	}
	if len(lines) > 0 && len(lines[0]) > maxBytes {
		result.Truncated = true
		result.TruncatedBy = "bytes"
		result.FirstLineLarge = true
		return result
	}
	kept := make([]string, 0, minInt(maxLines, len(lines)))
	bytes := 0
	for index, line := range lines {
		if index >= maxLines {
			result.TruncatedBy = "lines"
			break
		}
		lineBytes := len(line)
		if index > 0 {
			lineBytes++
		}
		if bytes+lineBytes > maxBytes {
			result.TruncatedBy = "bytes"
			break
		}
		kept = append(kept, line)
		bytes += lineBytes
	}
	result.Content = strings.Join(kept, "\n")
	result.OutputLines = len(kept)
	result.OutputBytes = len(result.Content)
	result.Truncated = true
	if result.TruncatedBy == "" {
		result.TruncatedBy = "lines"
	}
	return result
}

func splitLogicalLines(content string) []string {
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	if strings.HasSuffix(content, "\n") {
		return lines[:len(lines)-1]
	}
	return lines
}

func truncateGrepLine(line string, maximum int) (string, bool) {
	if utf8.RuneCountInString(line) <= maximum {
		return line, false
	}
	return string([]rune(line)[:maximum]) + "... [truncated]", true
}

func formatSize(bytes int) string {
	if bytes < 1024 {
		return fmt.Sprintf("%dB", bytes)
	}
	if bytes < 1024*1024 {
		return fmt.Sprintf("%.1fKB", float64(bytes)/1024)
	}
	return fmt.Sprintf("%.1fMB", float64(bytes)/(1024*1024))
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
