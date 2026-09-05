package resource

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestContextFilesLoadThroughAllAncestorsIndependentlyOfTrust(t *testing.T) {
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
	if len(snapshot.Instructions) != 3 || snapshot.Instructions[0].Content != "�" || snapshot.Instructions[1].Content != "trusted ancestor" || snapshot.Instructions[2].Content != "trusted cwd" {
		t.Fatalf("instructions = %#v", snapshot.Instructions)
	}
}

func TestUntrustedProjectConfigIsGatedButContextAlwaysLoads(t *testing.T) {
	s, agent, cwd := newService(t)
	write(t, filepath.Join(agent, "SYSTEM.md"), "global")
	for _, path := range []string{
		filepath.Join(cwd, "AGENTS.md"),
		filepath.Join(cwd, ".pi-go", "SYSTEM.md"),
		filepath.Join(cwd, ".pi-go", "APPEND_SYSTEM.md"),
		filepath.Join(cwd, ".pi-go", "prompts", "bad.md"),
		filepath.Join(cwd, ".pi-go", "skills", "bad", "SKILL.md"),
		filepath.Join(cwd, ".agents", "skills", "bad", "SKILL.md"),
	} {
		write(t, path, string([]byte{0xff}))
	}
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("untrusted assets affected reload: %v", err)
	}
	first, _ := s.Snapshot()
	if first.Trusted || first.BaseSystemPrompt != "global" || len(first.Instructions) != 1 || first.Instructions[0].Content != "�" || len(first.Templates) != 0 || len(first.Skills) != 0 || len(first.AppendSystem) != 0 || !strings.Contains(first.SystemPrompt, "global") {
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
		if !got.Trusted || !strings.Contains(got.SystemPrompt, "global") {
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
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("trusted symlink cwd = %v", err)
	}
	got, _ := s.Snapshot()
	if len(got.Instructions) == 0 || got.Instructions[len(got.Instructions)-1].Content != "must not escape" {
		t.Fatalf("symlink cwd context = %#v", got.Instructions)
	}
}

func TestInstructionCandidateOrderAndSymlinkFiles(t *testing.T) {
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
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("symlink instruction = %v", err)
	}
	snapshot, _ = s.Snapshot()
	if len(snapshot.Instructions) != 1 || snapshot.Instructions[0].Content != "claude" {
		t.Fatalf("symlink instruction content = %#v", snapshot.Instructions)
	}
}

func TestTemplatesExpansionAndPrecedenceAreDeterministic(t *testing.T) {
	s, agent, cwd := newService(t)
	write(t, filepath.Join(agent, "prompts", "a.md"), "---\ndescription: A & <global>\n---\n$1|$2|$@|$ARGUMENTS|${1:-one}|${@:2}|${@:2:1}")
	write(t, filepath.Join(agent, "prompts", "z.md"), "z")
	write(t, filepath.Join(cwd, ".pi-go", "prompts", "a.md"), "---\ndescription: project\nargument-hint: <item>\n---\nproject ${1:-fallback}")
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
	if len(snapshot.Diagnostics) != 1 || snapshot.Diagnostics[0].Resource != "template" || snapshot.Diagnostics[0].WinnerPath != filepath.Join(cwd, ".pi-go", "prompts", "a.md") {
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

func TestMalformedTemplatesAreSkipped(t *testing.T) {
	s, agent, _ := newService(t)
	write(t, filepath.Join(agent, "prompts", "ok.md"), "body")
	if err := s.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Snapshot()
	write(t, filepath.Join(agent, "prompts", "bad.md"), "---\ndescription missing colon\n---\nbody")
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("malformed template = %v", err)
	}
	after, _ := s.Snapshot()
	if after.SystemPrompt != before.SystemPrompt || len(after.Templates) != 1 || after.Templates[0].Name != "ok" || len(after.Diagnostics) != 1 || after.Diagnostics[0].Path != filepath.Join(agent, "prompts", "bad.md") {
		t.Fatalf("malformed template behavior: templates %#v diagnostics %#v", after.Templates, after.Diagnostics)
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
	if len(snapshot.Skills) != 3 || snapshot.Skills[0].Description != "project winner" || snapshot.Skills[1].Name != "hidden" || snapshot.Skills[2].Name != "root" {
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
	write(t, filepath.Join(cwd, ".pi-go", "skills", "a", "SKILL.md"), "---\nname: same\ndescription: project first\n---")
	write(t, filepath.Join(cwd, ".pi-go", "skills", "b", "SKILL.md"), "---\nname: same\ndescription: project second\n---")
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
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("overlong rune description = %v", err)
	}
	snapshot, _ = s.Snapshot()
	if len(snapshot.Skills) != 2 || len(snapshot.Diagnostics) != 1 || snapshot.Diagnostics[0].Kind != "warning" {
		t.Fatalf("overlong description warning/load = skills %#v diagnostics %#v", snapshot.Skills, snapshot.Diagnostics)
	}
}

func TestSkillValidationWarnsAndSymlinksAreFollowed(t *testing.T) {
	linked := t.TempDir()
	write(t, filepath.Join(linked, "linked", "SKILL.md"), "---\nname: linked\ndescription: followed\n---")
	s, agent, _ := newService(t)
	if err := os.MkdirAll(filepath.Join(agent, "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(linked, filepath.Join(agent, "skills", "linked-root")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(agent, "skills", "invalid", "SKILL.md"), "---\nname: INVALID\ndescription: x\n---")
	write(t, filepath.Join(agent, "skills", "typed", "SKILL.md"), "---\nname: typed\ndescription: x\ndisable-model-invocation: perhaps\n---")
	write(t, filepath.Join(agent, "skills", "missing", "SKILL.md"), "---\nname: missing\n---")
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("linked skill directory = %v", err)
	}
	snapshot, _ := s.Snapshot()
	if len(snapshot.Skills) != 2 || len(snapshot.Diagnostics) < 3 {
		t.Fatalf("skill warning/follow behavior = skills %#v diagnostics %#v", snapshot.Skills, snapshot.Diagnostics)
	}
}

func TestSkillBooleanUsesYAMLBooleanAndMissingDescriptionIsSkipped(t *testing.T) {
	for _, value := range []string{"1", "t", "yes", "'true'", `"true"`} {
		s, agent, _ := newService(t)
		write(t, filepath.Join(agent, "skills", "candidate", "SKILL.md"), "---\nname: candidate\ndescription: valid\ndisable-model-invocation: "+value+"\n---")
		if err := s.Reload(context.Background()); err != nil {
			t.Fatalf("disable-model-invocation %q = %v", value, err)
		}
		snapshot, _ := s.Snapshot()
		if len(snapshot.Skills) != 0 || len(snapshot.Diagnostics) != 1 || !strings.Contains(snapshot.Diagnostics[0].Message, "must be a boolean") {
			t.Fatalf("non-boolean disable value %q = %#v", value, snapshot.Skills)
		}
	}
	s, agent, _ := newService(t)
	write(t, filepath.Join(agent, "skills", "blank", "SKILL.md"), "---\nname: blank\ndescription: ' \t '\n---")
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("trim-empty description = %v", err)
	}
	snapshot, _ := s.Snapshot()
	if len(snapshot.Skills) != 0 || len(snapshot.Diagnostics) != 1 {
		t.Fatalf("blank description behavior = %#v %#v", snapshot.Skills, snapshot.Diagnostics)
	}
	s, agent, _ = newService(t)
	write(t, filepath.Join(agent, "skills", "visible", "SKILL.md"), "---\nname: visible\ndescription: valid\ndisable-model-invocation: false\n---")
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("literal false = %v", err)
	}
	snapshot, _ = s.Snapshot()
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
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("initial malformed reload = %v", err)
	}
	initial, err := s.Snapshot()
	if err != nil || !strings.Contains(initial.SystemPrompt, "�") {
		t.Fatalf("initial replacement-decoded snapshot = %#v, %v", initial, err)
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
	s, _, cwd := newService(t)
	write(t, filepath.Join(cwd, ".pi-go", "SYSTEM.md"), "stale trusted prompt")
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

func TestPromptAssemblyPreservesToolOrderOmitsTemplatesAndEscapesSkills(t *testing.T) {
	snapshot := Snapshot{
		AppendSystem: []string{"appendix"},
		Instructions: []Instruction{{Source: Source{Path: `/a&"`, Scope: ScopeProject}, Content: "rule"}},
		Templates:    []Template{{Source: Source{Path: "/template", Scope: ScopeGlobal}, Name: "a<&", Description: `say "hello"`}},
		Skills: []Skill{
			{Source: Source{Path: "/hidden", Scope: ScopeGlobal}, Name: "hidden", Description: "hidden", DisableModelInvocation: true},
			{Source: Source{Path: "/skill", Scope: ScopeGlobal}, Name: "visible", Description: "use <this> & that"},
		},
	}
	prompt, err := assemble(Config{
		CWD: `C:\work`, Tools: []Tool{{Name: "z", Snippet: "last"}, {Name: "a", Snippet: "first"}},
		SelectedTools: []string{"z", "a"}, MaxPromptBytes: 4096,
	}, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(prompt, "- z: last") > strings.Index(prompt, "- a: first") ||
		!strings.Contains(prompt, `path="/a&""`) ||
		strings.Contains(prompt, "available_prompt_templates") || strings.Contains(prompt, `name="a&lt;&amp;"`) ||
		strings.Contains(prompt, "available_skills") ||
		strings.Contains(prompt, "hidden\" description") ||
		!strings.Contains(prompt, "Current working directory: C:/work") {
		t.Fatalf("assembled prompt = %q", prompt)
	}
	if _, err := assemble(Config{CWD: "/work", MaxPromptBytes: 1}, Snapshot{}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("whole prompt limit = %v", err)
	}
}

func TestConfigValidationAndFullYAMLFrontmatter(t *testing.T) {
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
	if _, _, err := frontmatter("---\nname: duplicate\nname: duplicate\n---\nbody"); err == nil {
		t.Fatal("duplicate YAML key unexpectedly succeeded")
	}
	for _, raw := range []string{"---\nname: broken\nbody", "---\nname: never closes\n"} {
		front, body, err := frontmatter(raw)
		if err != nil || len(front) != 0 || body != raw {
			t.Fatalf("unterminated marker behavior for %q = %#v %q %v", raw, front, body, err)
		}
	}
	front, body, err := frontmatter("---\ndescription: >\n text\n---\nbody")
	if err != nil || front["description"] != "text" || body != "body" {
		t.Fatalf("folded YAML = %#v %q %v", front, body, err)
	}
	front, body, err = frontmatter("---\nname: 'quoted'\ndescription: \"works\"\n---\nbody")
	if err != nil || front["name"] != "quoted" || front["description"] != "works" || body != "body" {
		t.Fatalf("frontmatter quoted scalar = %#v %q %v", front, body, err)
	}
}

func TestTrustOptionsAndClearingRestoreParentDecision(t *testing.T) {
	s, _, cwd := newService(t)
	parent := filepath.Dir(cwd)
	options, err := s.Trust().Options(cwd)
	if err != nil || len(options) != 3 || options[0].Label != "Trust" || !strings.Contains(options[1].Label, parent) || options[2].Trusted {
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

func TestTrustNullInheritsAndInvalidValuesAreRejected(t *testing.T) {
	s, agent, cwd := newService(t)
	parent := filepath.Dir(cwd)
	canonicalParent, _ := normalize(parent)
	canonicalCWD, _ := normalize(cwd)
	content := "{\n  \"" + canonicalParent + "\": true,\n  \"" + canonicalCWD + "\": null\n}\n"
	write(t, filepath.Join(agent, "trust.json"), content)
	if trusted, known, err := s.Trust().Get(context.Background(), cwd); err != nil || !known || !trusted {
		t.Fatalf("null entry should inherit parent = %t,%t,%v", trusted, known, err)
	}
	write(t, filepath.Join(agent, "trust.json"), `{"future":{"trusted":false}}`)
	if _, _, err := s.Trust().Get(context.Background(), cwd); !errors.Is(err, ErrTrustStore) {
		t.Fatalf("invalid trust value = %v", err)
	}
}

func TestTrustFutureRawNestedDuplicateIsRejected(t *testing.T) {
	s, agent, cwd := newService(t)
	write(t, filepath.Join(agent, "trust.json"), "{\n  \""+cwd+"\": {\"x\": 1, \"x\": 2}\n}\n")
	if _, _, err := s.Trust().Get(context.Background(), cwd); !errors.Is(err, ErrTrustStore) {
		t.Fatalf("nested duplicate future value = %v", err)
	}
}

func TestTrustSerializationCancellationAndCommitUnknownReconcile(t *testing.T) {
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

func TestDirectorySymlinkReplacementFollowsResolvedDirectory(t *testing.T) {
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
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("directory swap = %v", err)
	}
	snapshot, _ := s.Snapshot()
	if len(snapshot.Templates) != 1 || snapshot.Templates[0].Name != "outside" {
		t.Fatalf("replaced prompt directory = %#v", snapshot.Templates)
	}
}

func TestTrustedProjectResourceParentSymlinkIsFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a directory symlink requires Windows developer mode or elevation")
	}
	s, _, cwd := newService(t)
	outside := t.TempDir()
	write(t, filepath.Join(outside, "SYSTEM.md"), "outside prompt")
	if err := os.Symlink(outside, filepath.Join(cwd, ".pi-go")); err != nil {
		t.Fatal(err)
	}
	if err := s.Trust().Set(context.Background(), cwd, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("project resource parent symlink = %v", err)
	}
	snapshot, _ := s.Snapshot()
	if snapshot.BaseSystemPrompt != "outside prompt" {
		t.Fatalf("project resource symlink prompt = %#v", snapshot)
	}
}

func TestTrustStoreRejectsUnsafeInputsAndFaultKeepsOldFile(t *testing.T) {
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
	if trusted, known, err := store.Get(context.Background(), cwd); err != nil || !known || !trusted {
		t.Fatalf("public trust file = %v", err)
	}
	if runtime.GOOS == "windows" {
		// The durable/private-file behavior above is portable. The remaining
		// symlink fixture requires developer mode or elevation on Windows.
		return
	}
	if err := os.Remove(store.Path()); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(agent, "target.json"), "{}")
	if err := os.Symlink(filepath.Join(agent, "target.json"), store.Path()); err != nil {
		t.Fatal(err)
	}
	if trusted, known, err := store.Get(context.Background(), cwd); err != nil || known || trusted {
		t.Fatalf("symlink trust store = %v", err)
	}
}

func TestTrustStoreCancellationLockAndMultiInstanceRace(t *testing.T) {
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

func TestTrustControlCharacterKeyRoundTripPreservesRawAndBoundary(t *testing.T) {
	s, agent, cwd := newService(t)
	canonicalCWD, _ := normalize(cwd)
	cwdJSON, err := json.Marshal(canonicalCWD)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(agent, "trust.json"), "{\n  "+string(cwdJSON)+": true\n}\n")
	controlPath := filepath.Join(filepath.Dir(cwd), "control\x01segment")
	no := false
	if err := s.Trust().SetMany(context.Background(), []TrustUpdate{{Path: controlPath, Decision: &no}}); err != nil {
		t.Fatalf("SetMany(control path) = %v", err)
	}
	committed, err := os.ReadFile(s.Trust().Path())
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(committed) || bytes.Contains(committed, []byte(`\x01`)) || !bytes.Contains(committed, []byte(`\u0001`)) {
		t.Fatalf("encoded trust store = %q", committed)
	}
	reopened, err := NewTrustStore(agent)
	if err != nil {
		t.Fatal(err)
	}
	if trusted, known, err := reopened.Get(context.Background(), cwd); err != nil || !known || !trusted {
		t.Fatalf("existing decision after control key = %t,%t,%v", trusted, known, err)
	}
	if trusted, known, err := reopened.Get(context.Background(), controlPath); err != nil || !known || trusted {
		t.Fatalf("control-key decision after reopen = %t,%t,%v", trusted, known, err)
	}
	reopened.max = int64(len(committed))
	if err := reopened.SetMany(context.Background(), []TrustUpdate{{Path: controlPath, Decision: &no}}); err != nil {
		t.Fatalf("exact SetMany boundary = %v", err)
	}
	extra := filepath.Join(filepath.Dir(cwd), "control\nextra")
	if err := reopened.SetMany(context.Background(), []TrustUpdate{{Path: extra, Decision: &no}}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("control-key overflow = %v", err)
	}
	after, err := os.ReadFile(reopened.Path())
	if err != nil || string(after) != string(committed) {
		t.Fatalf("overflow replaced committed store = %q, %v", after, err)
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
