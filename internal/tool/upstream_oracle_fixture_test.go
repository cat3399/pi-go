package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestFilesystemToolFixtures(t *testing.T) {
	var oracle struct {
		ReadRuntimeBoundary struct {
			BufferMaxStringLength int64  `json:"bufferMaxStringLength"`
			Unit                  string `json:"unit"`
		} `json:"readRuntimeBoundary"`
		GrepGlob struct {
			Brace    []string `json:"brace"`
			Negative []string `json:"negative"`
		} `json:"grepGlob"`
		FindGlob struct {
			Output string
		} `json:"findGlob"`
		IgnoreSemantics struct {
			Find, Grep []string
		} `json:"ignoreSemantics"`
		AncestorIgnoreSemantics struct {
			RepositoryFind, RepositoryGrep               []string
			OutsideRepositoryFind, OutsideRepositoryGrep []string
		} `json:"ancestorIgnoreSemantics"`
	}
	data, err := os.ReadFile("testdata/upstream_oracle.json")
	if err != nil {
		t.Fatalf("read tool fixture: %v", err)
	}
	if err := json.Unmarshal(data, &oracle); err != nil {
		t.Fatalf("decode tool fixture: %v", err)
	}
	if oracle.ReadRuntimeBoundary.BufferMaxStringLength != DefaultFilesystemMaxTextUnits || oracle.ReadRuntimeBoundary.Unit != "decoded UTF-16 code units" {
		t.Fatalf("text limit = %d; fixture specifies %q, %d", DefaultFilesystemMaxTextUnits, oracle.ReadRuntimeBoundary.Unit, oracle.ReadRuntimeBoundary.BufferMaxStringLength)
	}
	var corpus struct {
		Files  map[string]string
		Grep   struct{ Pattern, BraceGlob, NegativeGlob string }
		Find   struct{ Pattern string }
		Ignore struct {
			Pattern, GrepPattern string
			ControlFiles, Files  map[string]string
		} `json:"ignore"`
		AncestorIgnore struct {
			FindPattern, GrepPattern                  string
			RepositorySearch, OutsideRepositorySearch string
			ControlFiles, Files                       map[string]string
		} `json:"ancestorIgnore"`
	}
	corpusData, err := os.ReadFile("testdata/upstream_oracle_corpus.json")
	if err != nil {
		t.Fatalf("read frozen tool corpus: %v", err)
	}
	if err := json.Unmarshal(corpusData, &corpus); err != nil {
		t.Fatalf("decode frozen tool corpus: %v", err)
	}
	suite := newTestSuite(t)
	for name, content := range corpus.Files {
		writeTestFile(t, suite.WorkingDir(), name, content)
	}
	assertGrep := func(pattern string, want []string) {
		t.Helper()
		result, err := suite.Grep(context.Background(), GrepInput{Pattern: corpus.Grep.Pattern, Glob: &pattern})
		if err != nil {
			t.Fatal(err)
		}
		got := splitNonemptyLines(result.Text)
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("grep glob %q = %#v, want %#v", pattern, got, want)
		}
	}
	assertGrep(corpus.Grep.BraceGlob, oracle.GrepGlob.Brace)
	assertGrep(corpus.Grep.NegativeGlob, oracle.GrepGlob.Negative)
	result, err := suite.Find(context.Background(), FindInput{Pattern: corpus.Find.Pattern})
	if err != nil || result.Text != oracle.FindGlob.Output {
		t.Fatalf("find glob = %q, %v; want %q", result.Text, err, oracle.FindGlob.Output)
	}

	ignoreSuite := newTestSuite(t)
	for name, content := range corpus.Ignore.ControlFiles {
		writeTestFile(t, ignoreSuite.WorkingDir(), name, content)
	}
	for name, content := range corpus.Ignore.Files {
		writeTestFile(t, ignoreSuite.WorkingDir(), name, content)
	}
	result, err = ignoreSuite.Find(context.Background(), FindInput{Pattern: corpus.Ignore.Pattern})
	if err != nil {
		t.Fatal(err)
	}
	gotFind := splitNonemptyLines(result.Text)
	sort.Strings(gotFind)
	if !reflect.DeepEqual(gotFind, oracle.IgnoreSemantics.Find) {
		t.Fatalf("find ignore semantics = %#v, want %#v", gotFind, oracle.IgnoreSemantics.Find)
	}
	result, err = ignoreSuite.Grep(context.Background(), GrepInput{Pattern: corpus.Ignore.GrepPattern})
	if err != nil {
		t.Fatal(err)
	}
	gotGrep := splitNonemptyLines(result.Text)
	sort.Strings(gotGrep)
	if !reflect.DeepEqual(gotGrep, oracle.IgnoreSemantics.Grep) {
		t.Fatalf("grep ignore semantics = %#v, want %#v", gotGrep, oracle.IgnoreSemantics.Grep)
	}

	ancestorRoot := t.TempDir()
	for name, content := range corpus.AncestorIgnore.ControlFiles {
		writeTestFile(t, ancestorRoot, name, content)
	}
	for name, content := range corpus.AncestorIgnore.Files {
		writeTestFile(t, ancestorRoot, name, content)
	}
	assertAncestorFind := func(search string, want []string) {
		t.Helper()
		suite, suiteErr := NewFilesystemSuite(FilesystemOptions{WorkingDir: filepath.Join(ancestorRoot, filepath.FromSlash(search))})
		if suiteErr != nil {
			t.Fatal(suiteErr)
		}
		findResult, findErr := suite.Find(context.Background(), FindInput{Pattern: corpus.AncestorIgnore.FindPattern})
		if findErr != nil {
			t.Fatal(findErr)
		}
		got := splitNonemptyLines(findResult.Text)
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Find ancestor scope at %q = %#v, want %#v", search, got, want)
		}
	}
	assertAncestorGrep := func(search string, want []string) {
		t.Helper()
		suite, suiteErr := NewFilesystemSuite(FilesystemOptions{WorkingDir: filepath.Join(ancestorRoot, filepath.FromSlash(search))})
		if suiteErr != nil {
			t.Fatal(suiteErr)
		}
		grepResult, grepErr := suite.Grep(context.Background(), GrepInput{Pattern: corpus.AncestorIgnore.GrepPattern})
		if grepErr != nil {
			t.Fatal(grepErr)
		}
		got := splitNonemptyLines(grepResult.Text)
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Grep ancestor scope at %q = %#v, want %#v", search, got, want)
		}
	}
	assertAncestorFind(corpus.AncestorIgnore.RepositorySearch, oracle.AncestorIgnoreSemantics.RepositoryFind)
	assertAncestorGrep(corpus.AncestorIgnore.RepositorySearch, oracle.AncestorIgnoreSemantics.RepositoryGrep)
	assertAncestorFind(corpus.AncestorIgnore.OutsideRepositorySearch, oracle.AncestorIgnoreSemantics.OutsideRepositoryFind)
	assertAncestorGrep(corpus.AncestorIgnore.OutsideRepositorySearch, oracle.AncestorIgnoreSemantics.OutsideRepositoryGrep)
}

func splitNonemptyLines(value string) []string {
	var lines []string
	for _, line := range strings.Split(value, "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
