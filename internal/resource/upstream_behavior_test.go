package resource

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAssembleUsesOnlySelectedToolGuidelines(t *testing.T) {
	prompt, err := assemble(Config{
		CWD:           "/work",
		SelectedTools: []string{"bash"},
		Tools: []Tool{
			{Name: "read", Snippet: "Read", PromptGuidelines: []string{"READ-ONLY-GUIDELINE"}},
			{Name: "bash", Snippet: "Run", PromptGuidelines: []string{"BASH-ONLY-GUIDELINE"}},
		},
		ReadmePath: "/installation/README.md", DocsPath: "/installation/docs",
	}, Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "- bash: Run") || !strings.Contains(prompt, "BASH-ONLY-GUIDELINE") || !strings.Contains(prompt, "Use bash for file operations like ls, rg, find") {
		t.Fatalf("selected bash prompt = %q", prompt)
	}
	if strings.Contains(prompt, "- read: Read") || strings.Contains(prompt, "READ-ONLY-GUIDELINE") {
		t.Fatalf("inactive read metadata leaked into prompt = %q", prompt)
	}
}

func TestExplicitResourcePathsRemainEnabledByNoFlags(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "agent")
	cwd := filepath.Join(root, "project")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	explicitSkill := filepath.Join(root, "explicit", "SKILL.md")
	explicitPrompt := filepath.Join(root, "explicit", "review.md")
	write(t, explicitSkill, "---\nname: explicit\ndescription: explicit skill\n---\nbody")
	write(t, explicitPrompt, "---\ndescription: explicit prompt\n---\nreview $1")
	write(t, filepath.Join(agentDir, "skills", "default", "SKILL.md"), "---\nname: default\ndescription: default skill\n---")
	write(t, filepath.Join(agentDir, "prompts", "default.md"), "default prompt")

	service, err := New(Config{
		CWD: cwd, AgentDir: agentDir, NoSkills: true, NoPromptTemplates: true,
		SkillPaths: []string{explicitSkill}, PromptPaths: []string{explicitPrompt},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Skills) != 1 || snapshot.Skills[0].Name != "explicit" {
		t.Fatalf("explicit skills = %#v", snapshot.Skills)
	}
	if len(snapshot.Templates) != 1 || snapshot.Templates[0].Name != "review" {
		t.Fatalf("explicit templates = %#v", snapshot.Templates)
	}
}

func TestResourceFreeProjectIgnoresStoredRejectionUntilGatedResourcesAppear(t *testing.T) {
	service, _, cwd := newService(t)
	if err := service.Trust().Set(context.Background(), cwd, false); err != nil {
		t.Fatal(err)
	}
	if err := service.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := service.Snapshot()
	if !snapshot.Trusted {
		t.Fatalf("resource-free project remained untrusted: %#v", snapshot)
	}

	write(t, filepath.Join(cwd, ".pi-go", "SYSTEM.md"), "gated")
	if err := service.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = service.Snapshot()
	if snapshot.Trusted || snapshot.BaseSystemPrompt != "" || strings.Contains(snapshot.SystemPrompt, "gated") {
		t.Fatalf("stored rejection did not reactivate: %#v", snapshot)
	}
}

func TestExplicitAppendSourcesPreserveEmptyFiles(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "agent")
	cwd := filepath.Join(root, "project")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(root, "empty.md")
	write(t, empty, "")
	service, err := New(Config{
		CWD: cwd, AgentDir: agentDir,
		SystemPromptSource:        "base",
		AppendSystemPromptSources: []string{"left", empty, "right"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := service.Snapshot()
	if len(snapshot.AppendSystem) != 3 || snapshot.AppendSystem[1] != "" {
		t.Fatalf("append sources = %#v", snapshot.AppendSystem)
	}
	if !strings.Contains(snapshot.SystemPrompt, "base\n\nleft\n\n\n\nright") {
		t.Fatalf("assembled explicit append = %q", snapshot.SystemPrompt)
	}
}

func TestExpandSkillCommandReadsLatestBodyAndPrecedesTemplates(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "demo", "SKILL.md")
	write(t, path, "---\nname: demo\ndescription: test\n---\nfirst body")
	skill := Skill{Source: Source{Path: path, Scope: ScopeGlobal}, Name: "demo", Description: "test", BaseDir: filepath.Dir(path)}
	wantFirst := "<skill name=\"demo\" location=\"" + path + "\">\nReferences are relative to " + filepath.Dir(path) + ".\n\nfirst body\n</skill>\n\nrequest"
	if got := ExpandSkillCommand("/skill:demo   request", []Skill{skill}); got != wantFirst {
		t.Fatalf("first expansion = %q", got)
	}

	write(t, path, "---\nname: demo\ndescription: test\n---\nsecond body")
	got := ExpandPromptInput("/skill:demo", Snapshot{
		Skills:    []Skill{skill},
		Templates: []Template{{Name: "skill:demo", Content: "template must not win"}},
	})
	if !strings.Contains(got, "second body") || strings.Contains(got, "template must not win") {
		t.Fatalf("fresh skill/template precedence = %q", got)
	}
	if got := ExpandPromptInput("/skill:unknown", Snapshot{Templates: []Template{{Name: "skill:unknown", Content: "template fallback"}}}); got != "template fallback" {
		t.Fatalf("unknown skill template fallback = %q", got)
	}
	if got := ExpandSkillCommand("ordinary", []Skill{skill}); got != "ordinary" {
		t.Fatalf("ordinary input changed to %q", got)
	}
}

func TestEmptyAgentsFileWinsInstructionCandidatePriority(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "AGENTS.md"), "")
	write(t, filepath.Join(root, "CLAUDE.md"), "must not load")
	loaded, err := loadInstructions(context.Background(), root, ScopeProject, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || filepath.Base(loaded[0].Path) != "AGENTS.md" || loaded[0].Content != "" {
		t.Fatalf("instruction candidate = %#v", loaded)
	}
}

func TestSkillIgnoreFilesMatchOriginalGlobAndWhitespaceBehavior(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, ".gitignore"), " # this is a comment\n[ab]*/\ntrailing\\ \n")
	for _, name := range []string{"alpha", "beta", "commented", "keep", "trailing "} {
		write(t, filepath.Join(root, name, "SKILL.md"), "---\nname: "+strings.TrimSpace(name)+"\ndescription: "+name+"\n---")
	}
	skills, diagnostics := loadSkillPath(context.Background(), root, defaultResourceSource(root, ScopeUser), true, 0)
	if len(diagnostics) != 0 {
		t.Fatalf("ignore diagnostics = %#v", diagnostics)
	}
	if len(skills) != 2 || skills[0].Name != "commented" || skills[1].Name != "keep" {
		t.Fatalf("ignored skills = %#v", skills)
	}
}

func TestNestedLinkedWorktreeContextDedupMatchesUpstreamCase(t *testing.T) {
	t.Run("same filename shadows main checkout", func(t *testing.T) {
		root, agentDir, main, worktree, worktreeSrc := setupNestedWorktreeForTest(t)
		_ = root
		write(t, filepath.Join(main, "AGENTS.md"), "main")
		write(t, filepath.Join(worktree, "AGENTS.md"), "worktree")
		assertInstructionContents(t, agentDir, worktreeSrc, []string{"worktree"})
	})

	t.Run("main context remains without worktree context", func(t *testing.T) {
		_, agentDir, main, _, worktreeSrc := setupNestedWorktreeForTest(t)
		write(t, filepath.Join(main, "AGENTS.md"), "main")
		assertInstructionContents(t, agentDir, worktreeSrc, []string{"main"})
	})

	t.Run("different filenames both remain", func(t *testing.T) {
		_, agentDir, main, worktree, worktreeSrc := setupNestedWorktreeForTest(t)
		write(t, filepath.Join(main, "CLAUDE.md"), "main")
		write(t, filepath.Join(worktree, "AGENTS.md"), "worktree")
		assertInstructionContents(t, agentDir, worktreeSrc, []string{"main", "worktree"})
	})

	t.Run("ancestors above main checkout remain", func(t *testing.T) {
		root, agentDir, main, worktree, worktreeSrc := setupNestedWorktreeForTest(t)
		outer := filepath.Join(root, "outer")
		write(t, filepath.Join(outer, "AGENTS.md"), "outer")
		write(t, filepath.Join(main, "AGENTS.md"), "main")
		write(t, filepath.Join(worktree, "AGENTS.md"), "worktree")
		assertInstructionContents(t, agentDir, worktreeSrc, []string{"outer", "worktree"})
	})

	t.Run("bare layout container is not shadowed", func(t *testing.T) {
		root := t.TempDir()
		agentDir := filepath.Join(root, "agent")
		project := filepath.Join(root, "project")
		bare := filepath.Join(project, ".bare")
		worktree := filepath.Join(project, "main")
		gitDir := filepath.Join(bare, "worktrees", "main")
		if err := os.MkdirAll(agentDir, 0o700); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(bare, "HEAD"), "ref: refs/heads/main\n")
		write(t, filepath.Join(gitDir, "HEAD"), "ref: refs/heads/main\n")
		write(t, filepath.Join(gitDir, "commondir"), "../..")
		write(t, filepath.Join(worktree, ".git"), "gitdir: "+gitDir+"\n")
		write(t, filepath.Join(project, "AGENTS.md"), "container")
		write(t, filepath.Join(worktree, "AGENTS.md"), "worktree")
		assertInstructionContents(t, agentDir, worktree, []string{"container", "worktree"})
	})

	t.Run("corrupt gitdir climbs normally", func(t *testing.T) {
		root := t.TempDir()
		agentDir := filepath.Join(root, "agent")
		repo := filepath.Join(root, "repo")
		src := filepath.Join(repo, "src")
		if err := os.MkdirAll(agentDir, 0o700); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(repo, ".git"), "gitdir: /nonexistent/path/worktrees/feat\n")
		write(t, filepath.Join(repo, "AGENTS.md"), "repo")
		write(t, filepath.Join(src, "AGENTS.md"), "src")
		assertInstructionContents(t, agentDir, src, []string{"repo", "src"})
	})
}

func setupNestedWorktreeForTest(t *testing.T) (root, agentDir, main, worktree, worktreeSrc string) {
	t.Helper()
	root = t.TempDir()
	agentDir = filepath.Join(root, "agent")
	outer := filepath.Join(root, "outer")
	main = filepath.Join(outer, "main")
	worktree = filepath.Join(main, "worktrees", "feat")
	worktreeSrc = filepath.Join(worktree, "src")
	gitDir := filepath.Join(main, ".git", "worktrees", "feat")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktreeSrc, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(main, ".git", "HEAD"), "ref: refs/heads/main\n")
	write(t, filepath.Join(gitDir, "HEAD"), "ref: refs/heads/feat\n")
	write(t, filepath.Join(gitDir, "commondir"), "../..")
	write(t, filepath.Join(worktree, ".git"), "gitdir: "+gitDir+"\n")
	return root, agentDir, main, worktree, worktreeSrc
}

func assertInstructionContents(t *testing.T, agentDir, cwd string, want []string) {
	t.Helper()
	loaded, err := loadAllInstructions(context.Background(), cwd, agentDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(loaded))
	for index := range loaded {
		got[index] = loaded[index].Content
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("instruction contents = %#v, want %#v", got, want)
	}
}
