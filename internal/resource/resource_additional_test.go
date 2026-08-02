package resource

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestExactTrustDoesNotReadAboveTrustAnchor(t *testing.T) {
	root := t.TempDir()
	agent := filepath.Join(root, "agent")
	trusted := filepath.Join(root, "trusted")
	cwd := filepath.Join(trusted, "child")
	if err := os.MkdirAll(agent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "AGENTS.md"), string([]byte{0xff}))
	write(t, filepath.Join(trusted, "AGENTS.md"), "trusted ancestor")
	write(t, filepath.Join(cwd, "AGENTS.md"), "trusted cwd")
	s, err := New(Config{CWD: cwd, AgentDir: agent})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Trust().Set(context.Background(), trusted, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("Reload() = %v", err)
	}
	snapshot, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Instructions) != 2 || snapshot.Instructions[0].Content != "trusted ancestor" || snapshot.Instructions[1].Content != "trusted cwd" {
		t.Fatalf("instructions = %#v", snapshot.Instructions)
	}
}

func TestUntrustedProjectAssetsCannotChangeFailureOrSnapshot(t *testing.T) {
	s, agent, cwd := newService(t)
	write(t, filepath.Join(agent, "SYSTEM.md"), "global")
	for _, path := range []string{
		filepath.Join(cwd, "AGENTS.md"),
		filepath.Join(cwd, ".pi", "SYSTEM.md"),
		filepath.Join(cwd, ".pi", "APPEND_SYSTEM.md"),
		filepath.Join(cwd, ".pi", "prompts", "bad.md"),
		filepath.Join(cwd, ".pi", "skills", "bad", "SKILL.md"),
		filepath.Join(cwd, ".agents", "skills", "bad", "SKILL.md"),
	} {
		write(t, path, string([]byte{0xff}))
	}
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("untrusted assets affected reload: %v", err)
	}
	first, _ := s.Snapshot()
	if first.Trusted || first.SystemPrompt != "global\n\n<available_tools>\n- read: Read files\n</available_tools>\n\nCurrent working directory: "+strings.ReplaceAll(cwd, "\\", "/") {
		t.Fatalf("snapshot = %#v", first)
	}
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("repeat untrusted reload = %v", err)
	}
	second, _ := s.Snapshot()
	if second.SystemPrompt != first.SystemPrompt || len(second.Diagnostics) != 0 {
		t.Fatalf("untrusted snapshot changed: %#v", second)
	}
}

func TestUntrustedMissingLoopAndAliasedCWDDoNotProbeOrAuthorize(t *testing.T) {
	root := t.TempDir()
	agent := filepath.Join(root, "agent")
	if err := os.MkdirAll(agent, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(agent, "SYSTEM.md"), "global")
	loop := filepath.Join(root, "loop")
	if err := os.Symlink("loop", loop); err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{loop, filepath.Join(root, "does-not-exist")} {
		s, err := New(Config{CWD: cwd, AgentDir: agent})
		if err != nil {
			t.Fatalf("New(%q) = %v", cwd, err)
		}
		if err := s.Reload(context.Background()); err != nil {
			t.Fatalf("untrusted Reload(%q) = %v", cwd, err)
		}
		got, _ := s.Snapshot()
		if got.Trusted || !strings.Contains(got.SystemPrompt, "global") {
			t.Fatalf("untrusted snapshot for %q = %#v", cwd, got)
		}
	}
	anchor := filepath.Join(root, "anchor")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(anchor, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(outside, "AGENTS.md"), "must not escape")
	if err := os.Symlink(outside, filepath.Join(anchor, "escape")); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{CWD: filepath.Join(anchor, "escape"), AgentDir: agent})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Trust().Set(context.Background(), anchor, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(context.Background()); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("trusted symlink escape = %v", err)
	}
}

func TestInstructionCandidateOrderAndSafety(t *testing.T) {
	s, agent, _ := newService(t)
	write(t, filepath.Join(agent, "AGENTS.MD"), "agents upper")
	write(t, filepath.Join(agent, "CLAUDE.md"), "claude")
	if err := s.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := s.Snapshot()
	if len(snapshot.Instructions) != 1 || snapshot.Instructions[0].Content != "agents upper" {
		t.Fatalf("candidate priority = %#v", snapshot.Instructions)
	}
	if err := os.Remove(filepath.Join(agent, "AGENTS.MD")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(agent, "CLAUDE.md"), filepath.Join(agent, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(context.Background()); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("symlink instruction = %v", err)
	}
}

func TestTemplatesExpansionAndPrecedenceAreDeterministic(t *testing.T) {
	s, agent, cwd := newService(t)
	write(t, filepath.Join(agent, "prompts", "a.md"), "---\ndescription: A & <global>\n---\n$1|$2|$@|$ARGUMENTS|${1:-one}|${@:2}|${@:2:1}")
	write(t, filepath.Join(agent, "prompts", "z.md"), "z")
	write(t, filepath.Join(cwd, ".pi", "prompts", "a.md"), "---\ndescription: project\nargument-hint: <item>\n---\nproject ${1:-fallback}")
	if err := s.Trust().Set(context.Background(), cwd, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := s.Snapshot()
	if len(snapshot.Templates) != 2 || snapshot.Templates[0].Name != "a" || snapshot.Templates[0].ArgumentHint != "<item>" || snapshot.Templates[1].Name != "z" {
		t.Fatalf("templates = %#v", snapshot.Templates)
	}
	if got := ExpandTemplate(`/a ""`, snapshot.Templates); got != "project fallback" {
		t.Fatalf("empty quoted argument expansion = %q", got)
	}
	if got := substitute("$1|$2|$@|${1:-x}|${@:2}|${@:2:1}", parseArgs(`one "two words" three`)); got != "one|two words|one two words three|one|two words three|two words" {
		t.Fatalf("substitution = %q", got)
	}
	if got := substitute("${999999999999999999999999:-x}|${@:999999999999999999999999}", []string{"one"}); got != "x|" {
		t.Fatalf("overflow substitution = %q", got)
	}
	physicalCWD, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Diagnostics) != 1 || snapshot.Diagnostics[0].Resource != "template" || snapshot.Diagnostics[0].WinnerPath != filepath.Join(physicalCWD, ".pi", "prompts", "a.md") {
		t.Fatalf("template collision = %#v", snapshot.Diagnostics)
	}
}

func TestTemplateInvocationUsesUnicodeWhitespaceBoundaries(t *testing.T) {
	templates := []Template{{Name: "review", Content: "$1|$2|$@"}}
	for input, want := range map[string]string{
		"/review\tone two":      "one|two|one two",
		"/review\none\ttwo":     "one|two|one two",
		"/review\r\none\ftwo":   "one|two|one two",
		"/review\u00a0one\vtwo": "one|two|one two",
		"/review\ufeffone two":  "one|two|one two",
	} {
		if got := ExpandTemplate(input, templates); got != want {
			t.Fatalf("ExpandTemplate(%q) = %q, want %q", input, got, want)
		}
	}
	if got := ExpandTemplate("/ review", templates); got != "/ review" {
		t.Fatalf("empty command name expanded to %q", got)
	}
	if got := ExpandTemplate("/review\u0085one", templates); got != "/review\u0085one" {
		t.Fatalf("NEL incorrectly separated command: %q", got)
	}
	if got := ExpandTemplate("/review one\u0085two", templates); got != "one\u0085two||one\u0085two" {
		t.Fatalf("NEL incorrectly separated argument: %q", got)
	}
	if got := substitute("<$1>", parseArgs(`"line one
line two"`)); got != "<line one\nline two>" {
		t.Fatalf("quoted argument text = %q", got)
	}
}

func TestMalformedTemplatesFailWithoutReplacingHealthySnapshot(t *testing.T) {
	s, agent, _ := newService(t)
	write(t, filepath.Join(agent, "prompts", "ok.md"), "body")
	if err := s.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Snapshot()
	write(t, filepath.Join(agent, "prompts", "bad.md"), "---\ndescription missing colon\n---\nbody")
	if err := s.Reload(context.Background()); !errors.Is(err, ErrMalformed) {
		t.Fatalf("malformed template = %v", err)
	}
	after, _ := s.Snapshot()
	if after.SystemPrompt != before.SystemPrompt {
		t.Fatalf("failed template reload changed snapshot")
	}
}

func TestSkillsRootNestedCollisionAndDisableInvocation(t *testing.T) {
	s, agent, cwd := newService(t)
	write(t, filepath.Join(agent, "skills", "root", "SKILL.md"), "---\nname: root\ndescription: root description\n---\nbody")
	write(t, filepath.Join(agent, "skills", "root", "nested", "SKILL.md"), "---\nname: nested\ndescription: ignored\n---")
	write(t, filepath.Join(agent, "skills", "visible", "SKILL.md"), "---\nname: visible\ndescription: <visible & skill>\n---")
	write(t, filepath.Join(agent, "skills", "hidden", "SKILL.md"), "---\nname: hidden\ndescription: hidden skill\ndisable-model-invocation: true\n---")
	write(t, filepath.Join(cwd, ".agents", "skills", "visible", "SKILL.md"), "---\nname: visible\ndescription: project winner\n---")
	if err := s.Trust().Set(context.Background(), cwd, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := s.Snapshot()
	if len(snapshot.Skills) != 3 || snapshot.Skills[0].Name != "hidden" || snapshot.Skills[1].Name != "root" || snapshot.Skills[2].Description != "project winner" {
		t.Fatalf("skills = %#v", snapshot.Skills)
	}
	if strings.Contains(snapshot.SystemPrompt, "hidden skill") || !strings.Contains(snapshot.SystemPrompt, "project winner") || strings.Contains(snapshot.SystemPrompt, "nested") {
		t.Fatalf("skill prompt visibility = %q", snapshot.SystemPrompt)
	}
	if len(snapshot.Diagnostics) != 1 || snapshot.Diagnostics[0].Resource != "skill" {
		t.Fatalf("skill diagnostics = %#v", snapshot.Diagnostics)
	}
}

func TestSkillCollisionsWithinAndAcrossScopesHaveOneWinner(t *testing.T) {
	s, agent, cwd := newService(t)
	write(t, filepath.Join(agent, "skills", "a", "SKILL.md"), "---\nname: same\ndescription: global first\n---")
	write(t, filepath.Join(agent, "skills", "b", "SKILL.md"), "---\nname: same\ndescription: global second\n---")
	write(t, filepath.Join(cwd, ".pi", "skills", "a", "SKILL.md"), "---\nname: same\ndescription: project first\n---")
	write(t, filepath.Join(cwd, ".pi", "skills", "b", "SKILL.md"), "---\nname: same\ndescription: project second\n---")
	if err := s.Trust().Set(context.Background(), cwd, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := s.Snapshot()
	if len(snapshot.Skills) != 1 || snapshot.Skills[0].Description != "project first" {
		t.Fatalf("skill winners = %#v", snapshot.Skills)
	}
	if len(snapshot.Diagnostics) != 3 {
		t.Fatalf("skill collision diagnostics = %#v", snapshot.Diagnostics)
	}
	for _, diagnostic := range snapshot.Diagnostics {
		if diagnostic.Resource != "skill" || diagnostic.Name != "same" || diagnostic.WinnerPath == diagnostic.LoserPath {
			t.Fatalf("invalid collision diagnostic = %#v", diagnostic)
		}
	}
	if !strings.Contains(snapshot.SystemPrompt, "project first") || strings.Contains(snapshot.SystemPrompt, "global first") || strings.Contains(snapshot.SystemPrompt, "project second") {
		t.Fatalf("skill winner prompt = %q", snapshot.SystemPrompt)
	}
}

func TestWideDescriptionsKeepValidUTF8Boundaries(t *testing.T) {
	s, agent, _ := newService(t)
	wide := strings.Repeat("界", 61)
	write(t, filepath.Join(agent, "prompts", "wide.md"), wide+"\nbody")
	write(t, filepath.Join(agent, "skills", "wide", "SKILL.md"), "---\nname: wide\ndescription: "+strings.Repeat("界", 1024)+"\n---")
	if err := s.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := s.Snapshot()
	if len(snapshot.Templates) != 1 || snapshot.Templates[0].Description != strings.Repeat("界", 60)+"..." {
		t.Fatalf("wide template description = %q", snapshot.Templates[0].Description)
	}
	if len(snapshot.Skills) != 1 || utf8.RuneCountInString(snapshot.Skills[0].Description) != 1024 || !utf8.ValidString(snapshot.SystemPrompt) {
		t.Fatalf("wide skill/prompt = %#v, valid=%t", snapshot.Skills, utf8.ValidString(snapshot.SystemPrompt))
	}
	write(t, filepath.Join(agent, "skills", "too-wide", "SKILL.md"), "---\nname: too-wide\ndescription: "+strings.Repeat("界", 1025)+"\n---")
	if err := s.Reload(context.Background()); !errors.Is(err, ErrMalformed) {
		t.Fatalf("overlong rune description = %v", err)
	}
}

func TestSkillValidationAndSymlinkFailClosed(t *testing.T) {
	for _, content := range []string{
		"---\nname: INVALID\ndescription: x\n---",
		"---\nname: good\ndescription: x\ndisable-model-invocation: perhaps\n---",
		"---\nname: good\ndescription: \n---",
	} {
		s, agent, _ := newService(t)
		write(t, filepath.Join(agent, "skills", "candidate", "SKILL.md"), content)
		if err := s.Reload(context.Background()); !errors.Is(err, ErrMalformed) {
			t.Fatalf("skill %q reload = %v", content, err)
		}
	}
	s, agent, _ := newService(t)
	if err := os.MkdirAll(filepath.Join(agent, "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(agent, "skills", "linked")); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(context.Background()); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("linked skill directory = %v", err)
	}
}

func TestSkillBooleanAndTrimEmptyDescriptionAreStrict(t *testing.T) {
	for _, value := range []string{"1", "t", "yes", "'true'", `"true"`} {
		s, agent, _ := newService(t)
		write(t, filepath.Join(agent, "skills", "candidate", "SKILL.md"), "---\nname: candidate\ndescription: valid\ndisable-model-invocation: "+value+"\n---")
		if err := s.Reload(context.Background()); !errors.Is(err, ErrMalformed) {
			t.Fatalf("disable-model-invocation %q = %v", value, err)
		}
	}
	s, agent, _ := newService(t)
	write(t, filepath.Join(agent, "skills", "blank", "SKILL.md"), "---\nname: blank\ndescription: ' \t '\n---")
	if err := s.Reload(context.Background()); !errors.Is(err, ErrMalformed) {
		t.Fatalf("trim-empty description = %v", err)
	}
	s, agent, _ = newService(t)
	write(t, filepath.Join(agent, "skills", "visible", "SKILL.md"), "---\nname: visible\ndescription: valid\ndisable-model-invocation: false\n---")
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("literal false = %v", err)
	}
	snapshot, _ := s.Snapshot()
	if snapshot.Skills[0].DisableModelInvocation {
		t.Fatalf("literal false disabled skill")
	}
	s, agent, _ = newService(t)
	write(t, filepath.Join(agent, "skills", "hidden", "SKILL.md"), "---\nname: hidden\ndescription: valid\ndisable-model-invocation: TRUE\n---")
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("YAML TRUE = %v", err)
	}
	snapshot, _ = s.Snapshot()
	if !snapshot.Skills[0].DisableModelInvocation {
		t.Fatalf("YAML TRUE did not disable skill")
	}
}

func TestSnapshotIsDeeplyImmutableToCallers(t *testing.T) {
	s, agent, _ := newService(t)
	write(t, filepath.Join(agent, "AGENTS.md"), "instruction")
	write(t, filepath.Join(agent, "prompts", "p.md"), "prompt")
	if err := s.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, _ := s.Snapshot()
	first.AppendSystem = append(first.AppendSystem, "caller")
	first.Instructions[0].Content = "caller"
	first.Templates[0].Content = "caller"
	second, _ := s.Snapshot()
	if second.Instructions[0].Content != "instruction" || second.Templates[0].Content != "prompt" || len(second.AppendSystem) != 0 {
		t.Fatalf("snapshot mutated through caller: %#v", second)
	}
}

func TestReloadFailureBeforeFirstSnapshotAndCancellationRetention(t *testing.T) {
	s, agent, _ := newService(t)
	write(t, filepath.Join(agent, "SYSTEM.md"), string([]byte{0xff}))
	if err := s.Reload(context.Background()); !errors.Is(err, ErrMalformed) {
		t.Fatalf("initial malformed reload = %v", err)
	}
	if _, err := s.Snapshot(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("initial failed snapshot = %v", err)
	}
	write(t, filepath.Join(agent, "SYSTEM.md"), "healthy")
	if err := s.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Reload(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled reload = %v", err)
	}
	snapshot, err := s.Snapshot()
	if err != nil || !strings.Contains(snapshot.SystemPrompt, "healthy") {
		t.Fatalf("cancelled reload changed healthy snapshot: %#v %v", snapshot, err)
	}
}

func TestReloadGenerationPreventsTrustedStalePublication(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows persistence is deliberately fail-closed")
	}
	s, _, cwd := newService(t)
	write(t, filepath.Join(cwd, ".pi", "SYSTEM.md"), "stale trusted prompt")
	if err := s.Trust().Set(context.Background(), cwd, true); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	s.beforePublish = func(generation uint64) {
		if generation == 1 {
			close(entered)
			<-release
		}
	}
	oldDone := make(chan error, 1)
	go func() { oldDone <- s.Reload(context.Background()) }()
	<-entered
	if err := s.Trust().Set(context.Background(), cwd, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("new untrusted reload = %v", err)
	}
	close(release)
	if err := <-oldDone; !errors.Is(err, ErrStaleReload) {
		t.Fatalf("old trusted reload = %v", err)
	}
	snapshot, err := s.Snapshot()
	if err != nil || snapshot.Trusted || strings.Contains(snapshot.SystemPrompt, "stale trusted prompt") {
		t.Fatalf("final snapshot = %#v, %v", snapshot, err)
	}
}

func TestPromptAssemblyEscapesOrdersAndEnforcesWholeLimit(t *testing.T) {
	snapshot := Snapshot{
		AppendSystem: []string{"appendix"},
		Instructions: []Instruction{{Source: Source{Path: `/a&"`, Scope: ScopeProject}, Content: "rule"}},
		Templates:    []Template{{Source: Source{Path: "/template", Scope: ScopeGlobal}, Name: "a<&", Description: `say "hello"`}},
		Skills: []Skill{
			{Source: Source{Path: "/hidden", Scope: ScopeGlobal}, Name: "hidden", Description: "hidden", DisableModelInvocation: true},
			{Source: Source{Path: "/skill", Scope: ScopeGlobal}, Name: "visible", Description: "use <this> & that"},
		},
	}
	prompt, err := assemble(Config{CWD: `C:\work`, Tools: []Tool{{Name: "z", Snippet: "last"}, {Name: "a", Snippet: "first"}}, MaxPromptBytes: 4096}, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(prompt, "- a: first") > strings.Index(prompt, "- z: last") ||
		!strings.Contains(prompt, `path="/a&amp;&quot;"`) ||
		!strings.Contains(prompt, `name="a&lt;&amp;"`) ||
		!strings.Contains(prompt, "use &lt;this&gt; &amp; that") ||
		strings.Contains(prompt, "hidden\" description") ||
		!strings.Contains(prompt, "Current working directory: C:/work") {
		t.Fatalf("assembled prompt = %q", prompt)
	}
	if _, err := assemble(Config{CWD: "/work", MaxPromptBytes: 1}, Snapshot{}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("whole prompt limit = %v", err)
	}
}

func TestConfigAndFrontmatterRejectAmbiguousInputs(t *testing.T) {
	for _, config := range []Config{
		{CWD: "", AgentDir: "/agent"},
		{CWD: "/cwd", AgentDir: "", MaxFileBytes: 1, MaxPromptBytes: 1},
		{CWD: "/cwd", AgentDir: "/agent", MaxFileBytes: 2, MaxPromptBytes: 1},
		{CWD: "/cwd", AgentDir: "/agent", Tools: []Tool{{Name: "", Snippet: "x"}}},
	} {
		if _, err := validateConfig(config); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("validateConfig(%#v) = %v", config, err)
		}
	}
	for _, raw := range []string{
		"---\nname: duplicate\nname: duplicate\n---\nbody",
		"---\nname: broken\nbody",
		"---\ndescription: >\n text\n---\nbody",
		"---\nname: never closes\n",
	} {
		if _, _, err := frontmatter(raw); err == nil {
			t.Fatalf("frontmatter(%q) unexpectedly succeeded", raw)
		}
	}
	front, body, err := frontmatter("---\nname: 'quoted'\ndescription: \"works\"\n---\nbody")
	if err != nil || front["name"] != "quoted" || front["description"] != "works" || body != "body" {
		t.Fatalf("frontmatter quoted scalar = %#v %q %v", front, body, err)
	}
}

func TestTrustOptionsAndClearingRestoreParentDecision(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows persistence is deliberately fail-closed")
	}
	s, _, cwd := newService(t)
	parent := filepath.Dir(cwd)
	options, err := s.Trust().Options(cwd)
	if err != nil || len(options) != 3 || !strings.Contains(options[0].Label, parent) || options[2].Trusted {
		t.Fatalf("trust options = %#v %v", options, err)
	}
	if err := s.Trust().Set(context.Background(), parent, true); err != nil {
		t.Fatal(err)
	}
	no := false
	if err := s.Trust().SetMany(context.Background(), []TrustUpdate{{Path: cwd, Decision: &no}}); err != nil {
		t.Fatal(err)
	}
	if trusted, known, err := s.Trust().Get(context.Background(), cwd); err != nil || !known || trusted {
		t.Fatalf("child rejection = %t,%t,%v", trusted, known, err)
	}
	if err := s.Trust().SetMany(context.Background(), []TrustUpdate{{Path: cwd, Decision: nil}}); err != nil {
		t.Fatal(err)
	}
	if trusted, known, err := s.Trust().Get(context.Background(), cwd); err != nil || !known || !trusted {
		t.Fatalf("cleared child inherits parent = %t,%t,%v", trusted, known, err)
	}
}

func TestTrustNullAndFutureValuesRoundTripWithoutBlockingOtherDecision(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows persistence is deliberately fail-closed")
	}
	s, agent, cwd := newService(t)
	parent := filepath.Dir(cwd)
	future := filepath.Join(cwd, "future")
	content := "{\n  \"/future-number\": 1e9999,\n  \"" + future + "\": {\"trusted\": false},\n  \"" + parent + "\": true,\n  \"" + cwd + "\": null\n}\n"
	write(t, filepath.Join(agent, "trust.json"), content)
	if trusted, known, err := s.Trust().Get(context.Background(), cwd); err != nil || !known || !trusted {
		t.Fatalf("null entry should inherit parent = %t,%t,%v", trusted, known, err)
	}
	if trusted, known, err := s.Trust().Get(context.Background(), filepath.Join(future, "child")); err != nil || known || trusted {
		t.Fatalf("future value inherited parent trust = %t,%t,%v", trusted, known, err)
	}
	child := filepath.Join(cwd, "child")
	no := false
	if err := s.Trust().Set(context.Background(), child, no); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(agent, "trust.json"))
	if err != nil || !strings.Contains(string(data), `"/future-number": 1e9999`) || !strings.Contains(string(data), `"`+future+`": {"trusted": false}`) || !strings.Contains(string(data), `"`+cwd+`": null`) {
		t.Fatalf("future/null preservation = %q %v", data, err)
	}
	if trusted, known, err := s.Trust().Get(context.Background(), child); err != nil || !known || trusted {
		t.Fatalf("new explicit decision = %t,%t,%v", trusted, known, err)
	}
}

func TestTrustFutureRawNestedDuplicateIsRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows persistence is deliberately fail-closed")
	}
	s, agent, cwd := newService(t)
	write(t, filepath.Join(agent, "trust.json"), "{\n  \""+cwd+"\": {\"x\": 1, \"x\": 2}\n}\n")
	if _, _, err := s.Trust().Get(context.Background(), cwd); !errors.Is(err, ErrTrustStore) {
		t.Fatalf("nested duplicate future value = %v", err)
	}
}

func TestTrustSerializationCancellationAndCommitUnknownReconcile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows persistence is deliberately fail-closed")
	}
	s, _, cwd := newService(t)
	store := s.Trust()
	entered := make(chan struct{})
	release := make(chan struct{})
	store.beforeRename = func() error {
		close(entered)
		<-release
		return nil
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- store.Set(context.Background(), cwd, true) }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := store.Get(ctx, cwd); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Get during slow publication = %v", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Set = %v", err)
	}
	store.beforeRename = nil
	store.syncDir = func(string) error { return errors.New("injected directory sync") }
	if err := store.Set(context.Background(), filepath.Join(cwd, "next"), false); !errors.Is(err, ErrCommitUnknown) {
		t.Fatalf("post-rename sync failure = %v", err)
	}
	// Rename has already happened, so a fresh read is the only authoritative
	// reconciliation operation; no cached decision was advanced by Set.
	store.syncDir = syncDirectory
	if trusted, known, err := store.Get(context.Background(), filepath.Join(cwd, "next")); err != nil || !known || trusted {
		t.Fatalf("reconciled post-rename decision = %t,%t,%v", trusted, known, err)
	}
}

func TestDirectoryReplacementBetweenLstatAndOpenFailsClosed(t *testing.T) {
	s, agent, _ := newService(t)
	prompts := filepath.Join(agent, "prompts")
	write(t, filepath.Join(prompts, "safe.md"), "safe")
	outside := t.TempDir()
	write(t, filepath.Join(outside, "outside.md"), "outside")
	afterDirectoryLstat = func(path string) {
		if path != prompts {
			return
		}
		afterDirectoryLstat = nil
		if err := os.Rename(prompts, filepath.Join(agent, "prompts-held")); err != nil {
			t.Fatalf("move original prompts: %v", err)
		}
		if err := os.Symlink(outside, prompts); err != nil {
			t.Fatalf("replace prompts with link: %v", err)
		}
	}
	defer func() { afterDirectoryLstat = nil }()
	if err := s.Reload(context.Background()); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("directory swap = %v", err)
	}
}

func TestTrustedProjectResourceParentSymlinkFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows persistence is deliberately fail-closed")
	}
	s, _, cwd := newService(t)
	outside := t.TempDir()
	write(t, filepath.Join(outside, "SYSTEM.md"), "outside prompt")
	if err := os.Symlink(outside, filepath.Join(cwd, ".pi")); err != nil {
		t.Fatal(err)
	}
	if err := s.Trust().Set(context.Background(), cwd, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(context.Background()); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("project resource parent symlink = %v", err)
	}
}

func TestTrustStoreRejectsUnsafeInputsAndFaultKeepsOldFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows persistence is deliberately fail-closed")
	}
	s, agent, cwd := newService(t)
	store := s.Trust()
	if err := store.Set(context.Background(), cwd, true); err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	store.beforeRename = func() error { return errors.New("injected") }
	if err := store.Set(context.Background(), filepath.Join(cwd, "next"), true); !errors.Is(err, ErrTrustStore) {
		t.Fatalf("injected atomic failure = %v", err)
	}
	store.beforeRename = nil
	got, err := os.ReadFile(store.Path())
	if err != nil || string(got) != string(old) {
		t.Fatalf("atomic failure changed durable file: %q %v", got, err)
	}
	if err := os.Chmod(store.Path(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Get(context.Background(), cwd); !errors.Is(err, ErrTrustStore) {
		t.Fatalf("public trust file = %v", err)
	}
	if err := os.Remove(store.Path()); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(agent, "target.json"), "{}")
	if err := os.Symlink(filepath.Join(agent, "target.json"), store.Path()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Get(context.Background(), cwd); !errors.Is(err, ErrTrustStore) {
		t.Fatalf("symlink trust store = %v", err)
	}
}

func TestTrustStoreCancellationLockAndMultiInstanceRace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows persistence is deliberately fail-closed")
	}
	s, agent, cwd := newService(t)
	store := s.Trust()
	if err := os.Mkdir(store.Path()+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := store.Set(ctx, cwd, true); !errors.Is(err, ErrTrustStore) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled lock acquisition = %v", err)
	}
	if err := os.Remove(store.Path() + ".lock"); err != nil {
		t.Fatal(err)
	}
	other, err := NewTrustStore(agent)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for i := 0; i < 24; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			path := filepath.Join(cwd, "r", string(rune('a'+i)))
			if err := []*TrustStore{store, other}[i%2].Set(context.Background(), path, i%2 == 0); err != nil {
				t.Errorf("Set(%d) = %v", i, err)
			}
		}(i)
	}
	group.Wait()
	data, err := os.ReadFile(store.Path())
	if err != nil || !strings.HasSuffix(string(data), "}\n") || strings.Count(string(data), "\n") < 25 {
		t.Fatalf("racing trust output = %q %v", data, err)
	}
}

func TestTrustSerializedSizeLimitPreservesOldStateAndReopens(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows persistence is deliberately fail-closed")
	}
	s, agent, cwd := newService(t)
	if err := s.Trust().Set(context.Background(), cwd, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	beforeSnapshot, _ := s.Snapshot()
	before, err := os.ReadFile(s.Trust().Path())
	if err != nil {
		t.Fatal(err)
	}
	s.Trust().max = int64(len(before))
	if err := s.Trust().Set(context.Background(), cwd, true); err != nil {
		t.Fatalf("exact serialized boundary = %v", err)
	}
	if err := s.Trust().Set(context.Background(), filepath.Join(cwd, "extra"), false); !errors.Is(err, ErrTooLarge) || !errors.Is(err, ErrTrustStore) {
		t.Fatalf("serialized overflow = %v", err)
	}
	after, err := os.ReadFile(s.Trust().Path())
	if err != nil || string(after) != string(before) {
		t.Fatalf("overflow changed old file = %q, %v", after, err)
	}
	afterSnapshot, _ := s.Snapshot()
	if afterSnapshot.SystemPrompt != beforeSnapshot.SystemPrompt || afterSnapshot.Trusted != beforeSnapshot.Trusted {
		t.Fatalf("overflow changed snapshot = %#v", afterSnapshot)
	}
	reopened, err := NewTrustStore(agent)
	if err != nil {
		t.Fatal(err)
	}
	if trusted, known, err := reopened.Get(context.Background(), cwd); err != nil || !known || !trusted {
		t.Fatalf("reopen after overflow = %t,%t,%v", trusted, known, err)
	}
	if err := reopened.Set(context.Background(), filepath.Join(cwd, "extra"), false); err != nil {
		t.Fatalf("lock not released after overflow: %v", err)
	}
}

func FuzzTemplateExpansionBoundaries(f *testing.F) {
	f.Add("$1 ${1:-x} ${@:2:1}", "one two")
	f.Add("${999999999999999999999999:-x}", "")
	f.Fuzz(func(t *testing.T, template, arguments string) {
		_ = substitute(template, parseArgs(arguments))
		if utf8.ValidString(template) {
			for _, limit := range []int{0, 1, 60, 1024} {
				if got := truncateRunes(template, limit); !utf8.ValidString(got) {
					t.Fatalf("truncateRunes produced invalid UTF-8: %q", got)
				}
			}
		}
	})
}
