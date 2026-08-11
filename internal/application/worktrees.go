package application

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const gitOperationTimeout = 10 * time.Second

var invalidWorktreeDirectoryCharacters = regexp.MustCompile(`[\\/:*?"<>|\s]+`)

type WorktreeInfo struct {
	Path   string  `json:"path"`
	Branch *string `json:"branch"`
	IsMain bool    `json:"isMain"`
}

type WorktreeList struct {
	ProjectRoot string
	IsGit       bool
	IsTopLevel  bool
	Worktrees   []WorktreeInfo
}

type WorktreeCreated struct {
	Path   string
	Branch string
}

type projectInfo struct {
	root      string
	branch    *string
	worktree  bool
	topLevel  bool
	gitBacked bool
}

func (s *Service) ListWorktrees(ctx context.Context, cwd string) (WorktreeList, error) {
	resolved, err := s.authorizeResourcePath(cwd, true)
	if err != nil {
		return WorktreeList{}, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return WorktreeList{}, err
	}
	if !info.IsDir() {
		return WorktreeList{}, ErrPathNotDirectory
	}
	project := resolveGitProject(normalizeContext(ctx), resolved)
	result := WorktreeList{
		ProjectRoot: project.root, IsGit: project.gitBacked, IsTopLevel: project.topLevel,
		Worktrees: []WorktreeInfo{},
	}
	if !project.gitBacked {
		return result, nil
	}
	worktrees, err := listGitWorktrees(normalizeContext(ctx), resolved)
	if err != nil {
		result.IsGit = false
		return result, nil
	}
	result.Worktrees = worktrees
	s.allowResourceRoot(project.root)
	for _, worktree := range worktrees {
		s.allowResourceRoot(worktree.Path)
	}
	return result, nil
}

func (s *Service) AddWorktree(ctx context.Context, cwd, branch string) (WorktreeCreated, error) {
	resolved, err := s.authorizeResourcePath(cwd, true)
	if err != nil {
		return WorktreeCreated{}, err
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return WorktreeCreated{}, errors.New("branch is required")
	}
	directoryName := strings.Trim(invalidWorktreeDirectoryCharacters.ReplaceAllString(branch, "-"), "-")
	if directoryName == "" {
		return WorktreeCreated{}, fmt.Errorf("invalid branch name: %s", branch)
	}
	commonDirectory, err := runGit(ctx, resolved, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return WorktreeCreated{}, err
	}
	repositoryRoot := filepath.Dir(strings.TrimSpace(commonDirectory))
	baseDirectory := repositoryRoot + "-worktrees"
	worktreePath := filepath.Join(baseDirectory, directoryName)
	if _, err := os.Stat(worktreePath); err == nil {
		return WorktreeCreated{}, fmt.Errorf("directory already exists: %s", worktreePath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return WorktreeCreated{}, err
	}
	if err := os.MkdirAll(baseDirectory, 0o700); err != nil {
		return WorktreeCreated{}, err
	}
	_, branchErr := runGit(ctx, repositoryRoot, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	arguments := []string{"worktree", "add"}
	if branchErr != nil {
		arguments = append(arguments, "-b", branch)
	}
	arguments = append(arguments, "--", worktreePath)
	if branchErr == nil {
		arguments = append(arguments, branch)
	}
	if _, err := runGit(ctx, repositoryRoot, arguments...); err != nil {
		return WorktreeCreated{}, err
	}
	s.allowResourceRoot(worktreePath)
	return WorktreeCreated{Path: worktreePath, Branch: branch}, nil
}

func (s *Service) RemoveWorktree(ctx context.Context, cwd, target string, force bool) error {
	resolved, err := s.authorizeResourcePath(cwd, true)
	if err != nil {
		return err
	}
	worktrees, err := listGitWorktrees(normalizeContext(ctx), resolved)
	if err != nil {
		return err
	}
	target = filepath.Clean(strings.TrimSpace(target))
	var selected *WorktreeInfo
	for index := range worktrees {
		if filepath.Clean(worktrees[index].Path) == target {
			selected = &worktrees[index]
			break
		}
	}
	if selected == nil {
		return fmt.Errorf("not a worktree of this repository: %s", target)
	}
	if selected.IsMain {
		return errors.New("cannot remove the main worktree")
	}
	arguments := []string{"worktree", "remove"}
	if force {
		arguments = append(arguments, "--force")
	}
	arguments = append(arguments, target)
	_, err = runGit(ctx, resolved, arguments...)
	return err
}

func resolveGitProject(ctx context.Context, cwd string) projectInfo {
	fallback := projectInfo{root: cwd}
	output, err := runGit(ctx, cwd,
		"rev-parse", "--path-format=absolute", "--git-common-dir", "--git-dir", "--show-toplevel", "--abbrev-ref", "HEAD",
	)
	if err != nil {
		return fallback
	}
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 4 {
		return fallback
	}
	commonDirectory := strings.TrimSpace(lines[0])
	gitDirectory := strings.TrimSpace(lines[1])
	topLevel := strings.TrimSpace(lines[2])
	branchName := strings.TrimSpace(lines[3])
	realCWD := cwd
	if value, err := filepath.EvalSymlinks(cwd); err == nil {
		realCWD = value
	}
	isTopLevel := filepath.Clean(topLevel) == filepath.Clean(realCWD)
	isWorktree := gitDirectory != commonDirectory && isTopLevel
	root := cwd
	if isWorktree {
		root = filepath.Dir(commonDirectory)
	}
	var branch *string
	if branchName != "" && branchName != "HEAD" {
		branch = &branchName
	}
	return projectInfo{root: root, branch: branch, worktree: isWorktree, topLevel: isTopLevel, gitBacked: true}
}

func listGitWorktrees(ctx context.Context, cwd string) ([]WorktreeInfo, error) {
	output, err := runGit(ctx, cwd, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	result := make([]WorktreeInfo, 0)
	type pendingWorktree struct {
		path     string
		branch   *string
		prunable bool
	}
	current := pendingWorktree{}
	flush := func() {
		if current.path != "" && !current.prunable {
			if info, statErr := os.Stat(current.path); statErr == nil && info.IsDir() {
				result = append(result, WorktreeInfo{Path: current.path, Branch: current.branch, IsMain: len(result) == 0})
			}
		}
		current = pendingWorktree{}
	}
	for _, line := range strings.Split(output, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			current.path = strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
		case strings.HasPrefix(line, "branch ") && current.path != "":
			branch := strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(line, "branch ")), "refs/heads/")
			current.branch = &branch
		case strings.HasPrefix(line, "prunable"):
			current.prunable = true
		case strings.TrimSpace(line) == "":
			flush()
		}
	}
	flush()
	return result, nil
}

func runGit(ctx context.Context, cwd string, arguments ...string) (string, error) {
	operationContext, cancel := context.WithTimeout(normalizeContext(ctx), gitOperationTimeout)
	defer cancel()
	command := exec.CommandContext(operationContext, "git", append([]string{"-C", cwd}, arguments...)...)
	command.Env = append(os.Environ(), "LC_ALL=C")
	output, err := command.Output()
	if err == nil {
		return strings.TrimSpace(string(output)), nil
	}
	if errors.Is(operationContext.Err(), context.DeadlineExceeded) {
		return "", errors.New("git operation timed out")
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		message := strings.TrimSpace(string(exit.Stderr))
		if message != "" {
			return "", errors.New(message)
		}
	}
	return "", err
}
