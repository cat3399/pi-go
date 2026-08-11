package application

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cat3399/pi-go/internal/app"
)

func TestWorktreesMatchGitProjectAndCreateBranch(t *testing.T) {
	parent := t.TempDir()
	repository := filepath.Join(parent, "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repository, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repository, "add", "README.md")
	runTestGit(t, repository, "-c", "user.name=Pi Go Test", "-c", "user.email=pi-go@example.invalid", "commit", "-m", "initial")
	realRepository, err := filepath.EvalSymlinks(repository)
	if err != nil {
		t.Fatal(err)
	}

	service, err := NewService(ServiceOptions{
		Production: app.ProductionConfig{
			WorkingDir:  repository,
			AgentDir:    filepath.Join(parent, "agent"),
			Environment: []string{},
		},
		DisableReaper: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })

	listed, err := service.ListWorktrees(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	if !listed.IsGit || !listed.IsTopLevel || listed.ProjectRoot != realRepository || len(listed.Worktrees) != 1 {
		t.Fatalf("initial worktree list = %#v", listed)
	}
	if !listed.Worktrees[0].IsMain || listed.Worktrees[0].Branch == nil || *listed.Worktrees[0].Branch != "main" {
		t.Fatalf("main worktree = %#v", listed.Worktrees[0])
	}

	created, err := service.AddWorktree(context.Background(), repository, "feature/api-test")
	if err != nil {
		t.Fatal(err)
	}
	if created.Branch != "feature/api-test" || created.Path != filepath.Join(filepath.Dir(realRepository), "repository-worktrees", "feature-api-test") {
		t.Fatalf("created worktree = %#v", created)
	}
	if info, err := os.Stat(created.Path); err != nil || !info.IsDir() {
		t.Fatalf("created path stat = %#v, %v", info, err)
	}

	listed, err = service.ListWorktrees(context.Background(), created.Path)
	if err != nil {
		t.Fatal(err)
	}
	if listed.ProjectRoot != realRepository || !listed.IsTopLevel || len(listed.Worktrees) != 2 {
		t.Fatalf("linked worktree list = %#v", listed)
	}
}

func TestListWorktreesRejectsPathsOutsideApplicationRoots(t *testing.T) {
	root := t.TempDir()
	service, err := NewService(ServiceOptions{
		Production:    app.ProductionConfig{WorkingDir: root, AgentDir: filepath.Join(root, "agent"), Environment: []string{}},
		DisableReaper: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if _, err := service.ListWorktrees(context.Background(), filepath.Dir(root)); err != ErrResourceAccessDenied {
		t.Fatalf("outside-root error = %v", err)
	}
}

func runTestGit(t *testing.T, cwd string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", cwd}, arguments...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
}
