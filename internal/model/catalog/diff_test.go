package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestReportKeepsEveryChangeWhenDisplayIsTruncated(t *testing.T) {
	dir := t.TempDir()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, dir)
	}
	var diff Diff
	for i := range 10 {
		diff.Added = append(diff.Added, fmt.Sprintf("model-%02d", i))
	}
	short, err := diff.Report()
	if err != nil || short.FullOutputPath != "" || !strings.Contains(short.Summary, "  + model-09\n") {
		t.Fatalf("short report = %+v, %v", short, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("short report created files: %v, %v", entries, err)
	}
	diff.Added = append(diff.Added, "model-10", "model-11")
	diff.Removed, diff.Changed, diff.Defaults, diff.Providers = diff.Added, diff.Added, diff.Added, diff.Added
	result, err := diff.Report()
	if err != nil || filepath.Dir(result.FullOutputPath) != dir || !strings.Contains(result.String(), "[Output truncated. Full output: "+result.FullOutputPath+"]") {
		t.Fatalf("missing truncation notice or log: %+v, %v", result, err)
	}
	raw, err := os.ReadFile(result.FullOutputPath)
	if err != nil {
		t.Fatal(err)
	}
	full := string(raw)
	if strings.Contains(full, "... 2 more") || strings.Contains(result.Summary, "model-10") {
		t.Fatal("full log and preview were not rendered independently")
	}
	for _, group := range []string{"+", "-", "~", "default", "provider"} {
		if !strings.Contains(result.Summary, "  "+group+" ... 2 more\n") {
			t.Fatalf("missing preview count for %s", group)
		}
		for _, value := range diff.Added {
			if !strings.Contains(full, "  "+group+" "+value+"\n") {
				t.Fatalf("log omitted %s %s", group, value)
			}
		}
	}
	if stat, err := os.Stat(result.FullOutputPath); err != nil || runtime.GOOS != "windows" && stat.Mode().Perm()&0077 != 0 {
		t.Fatalf("log not private: %v, %v", stat, err)
	}
}

func TestReportBoundsEvenOneLongModelID(t *testing.T) {
	dir := t.TempDir()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, dir)
	}
	id := strings.Repeat("模型", maxSummaryBytes/4) + "-end"
	result, err := (Diff{Changed: []string{id}}).Report()
	if err != nil || len(result.Summary) > maxSummaryBytes || !utf8.ValidString(result.Summary) {
		t.Fatalf("invalid preview: %d bytes, %v", len(result.Summary), err)
	}
	if raw, err := os.ReadFile(result.FullOutputPath); err != nil || !strings.Contains(string(raw), "  ~ "+id+"\n") {
		t.Fatalf("incomplete long-ID log: %v", err)
	}
}
