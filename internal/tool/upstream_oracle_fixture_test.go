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

func TestFrozenUpstreamToolOracle(t *testing.T) {
	var oracle struct {
		UpstreamCommit string `json:"upstreamCommit"`
		Generator      struct {
			NodeVersion, ToolPlatform, RGVersion, RGSHA256, FDVersion, FDSHA256, Corpus string
		} `json:"generator"`
		ReadRuntimeBoundary struct {
			BufferMaxStringLength int64  `json:"bufferMaxStringLength"`
			Unit                  string `json:"unit"`
		} `json:"readRuntimeBoundary"`
		GrepGlob struct {
			Brace    []string `json:"brace"`
			Negative []string `json:"negative"`
		} `json:"grepGlob"`
		FindGlob struct {
			Input, ForwardedPattern, Operation, Output string
		} `json:"findGlob"`
		IgnoreSemantics struct {
			Operation  string
			Find, Grep []string
		} `json:"ignoreSemantics"`
		AncestorIgnoreSemantics struct {
			Operation                                    string
			RepositoryFind, RepositoryGrep               []string
			OutsideRepositoryFind, OutsideRepositoryGrep []string
		} `json:"ancestorIgnoreSemantics"`
	}
	data, err := os.ReadFile("testdata/upstream_oracle.json")
	if err != nil {
		t.Fatalf("read frozen upstream oracle: %v", err)
	}
	if err := json.Unmarshal(data, &oracle); err != nil {
		t.Fatalf("decode frozen upstream oracle: %v", err)
	}
	if oracle.UpstreamCommit != "a116523434806910336b9de3e38a41aa5860030b" {
		t.Fatalf("unexpected upstream oracle commit %q", oracle.UpstreamCommit)
	}
	if oracle.ReadRuntimeBoundary.BufferMaxStringLength != DefaultFilesystemMaxTextUnits || oracle.ReadRuntimeBoundary.Unit != "decoded UTF-16 code units" {
		t.Fatalf("text string bound = %d %q, frozen upstream runtime = %d", DefaultFilesystemMaxTextUnits, oracle.ReadRuntimeBoundary.Unit, oracle.ReadRuntimeBoundary.BufferMaxStringLength)
	}
	if oracle.Generator.NodeVersion != "v24.18.1" || oracle.Generator.ToolPlatform != "darwin-arm64" || oracle.Generator.RGVersion != "ripgrep 15.2.0 (rev e89fff89ac)" || oracle.Generator.FDVersion != "fd 10.4.2" || oracle.Generator.Corpus != "upstream_oracle_corpus.json" {
		t.Fatalf("unpinned oracle generator metadata = %#v", oracle.Generator)
	}
	if _, err := os.Stat("testdata/" + oracle.Generator.Corpus); err != nil {
		t.Fatalf("oracle corpus is unavailable: %v", err)
	}
	var corpus struct {
		Tools map[string]struct {
			Version          string            `json:"version"`
			SHA256ByPlatform map[string]string `json:"sha256ByPlatform"`
		} `json:"tools"`
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
	corpusData, err := os.ReadFile("testdata/" + oracle.Generator.Corpus)
	if err != nil {
		t.Fatalf("read frozen tool corpus: %v", err)
	}
	if err := json.Unmarshal(corpusData, &corpus); err != nil {
		t.Fatalf("decode frozen tool corpus: %v", err)
	}
	for _, name := range []string{"rg", "fd"} {
		input, ok := corpus.Tools[name]
		if !ok || input.Version == "" || input.SHA256ByPlatform["darwin-arm64"] == "" {
			t.Fatalf("%s oracle input is not pinned by registered-platform SHA: %#v", name, input)
		}
		frozenDigest := oracle.Generator.RGSHA256
		if name == "fd" {
			frozenDigest = oracle.Generator.FDSHA256
		}
		if frozenDigest != input.SHA256ByPlatform[oracle.Generator.ToolPlatform] {
			t.Fatalf("%s frozen generator digest=%q, corpus=%q", name, frozenDigest, input.SHA256ByPlatform[oracle.Generator.ToolPlatform])
		}
	}
	suite := newTestSuite(t)
	for _, name := range []string{"main.go", "web.ts", "web.test.ts", "notes.md", "!literal.txt"} {
		writeTestFile(t, suite.WorkingDir(), name, "TOKEN\n")
	}
	assertGrep := func(pattern string, want []string) {
		t.Helper()
		result, err := suite.Grep(context.Background(), GrepInput{Pattern: "TOKEN", Glob: &pattern})
		if err != nil {
			t.Fatal(err)
		}
		got := splitNonemptyLines(result.Text)
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("grep glob %q = %#v, frozen upstream = %#v", pattern, got, want)
		}
	}
	assertGrep("{*.go,*.ts}", oracle.GrepGlob.Brace)
	assertGrep("!*.test.ts", oracle.GrepGlob.Negative)
	if oracle.FindGlob.Input != oracle.FindGlob.ForwardedPattern {
		t.Fatalf("frozen find oracle does not preserve leading bang: %#v", oracle.FindGlob)
	}
	if oracle.FindGlob.Operation != "default createFindToolDefinition with verified managed fd" {
		t.Fatalf("frozen find oracle did not execute the default fd operation: %#v", oracle.FindGlob)
	}
	result, err := suite.Find(context.Background(), FindInput{Pattern: oracle.FindGlob.Input})
	if err != nil || result.Text != oracle.FindGlob.Output {
		t.Fatalf("find glob = %q, %v; frozen upstream = %q", result.Text, err, oracle.FindGlob.Output)
	}

	if oracle.IgnoreSemantics.Operation != "default createFindToolDefinition/createGrepToolDefinition with verified managed fd/rg" {
		t.Fatalf("frozen ignore oracle did not execute default managed tools: %q", oracle.IgnoreSemantics.Operation)
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
		t.Fatalf("find ignore semantics = %#v, frozen pinned fd = %#v", gotFind, oracle.IgnoreSemantics.Find)
	}
	result, err = ignoreSuite.Grep(context.Background(), GrepInput{Pattern: corpus.Ignore.GrepPattern})
	if err != nil {
		t.Fatal(err)
	}
	gotGrep := splitNonemptyLines(result.Text)
	sort.Strings(gotGrep)
	if !reflect.DeepEqual(gotGrep, oracle.IgnoreSemantics.Grep) {
		t.Fatalf("grep ignore semantics = %#v, frozen pinned rg = %#v", gotGrep, oracle.IgnoreSemantics.Grep)
	}

	if oracle.AncestorIgnoreSemantics.Operation != "default managed fd/rg ancestor discovery" {
		t.Fatalf("frozen ancestor oracle did not use default managed tools: %q", oracle.AncestorIgnoreSemantics.Operation)
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
			t.Fatalf("Find ancestor scope at %q = %#v, frozen pinned fd = %#v", search, got, want)
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
			t.Fatalf("Grep ancestor scope at %q = %#v, frozen pinned rg = %#v", search, got, want)
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
