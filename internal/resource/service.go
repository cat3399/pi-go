package resource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

type Service struct {
	config   Config
	trust    *TrustStore
	mu       sync.RWMutex
	snapshot *Snapshot
}

// afterDirectoryLstat is a package-private fault seam. It is only set by
// deterministic tests to model a parent-directory replacement in the narrow
// interval between name inspection and descriptor acquisition.
var afterDirectoryLstat func(string)

func New(config Config) (*Service, error) {
	config, err := validateConfig(config)
	if err != nil {
		return nil, err
	}
	trust, err := NewTrustStore(config.AgentDir)
	if err != nil {
		return nil, err
	}
	return &Service{config: config, trust: trust}, nil
}
func (s *Service) Trust() *TrustStore { return s.trust }
func (s *Service) Snapshot() (Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.snapshot == nil {
		return Snapshot{}, ErrUnavailable
	}
	return s.snapshot.clone(), nil
}

// Reload constructs everything off-lock. A failed reload leaves the last
// healthy snapshot observable, while an initial failure has no snapshot.
func (s *Service) Reload(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	decision, err := s.trust.decision(ctx, s.config.CWD)
	if err != nil {
		return err
	}
	config := s.config
	if decision.Known && decision.Trusted {
		config.CWD, decision.Root, err = admitTrustedPath(config.CWD, decision.Root)
		if err != nil {
			return err
		}
	}
	next, err := load(ctx, config, decision)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.snapshot = &next
	s.mu.Unlock()
	return nil
}

// admitTrustedPath is intentionally called only after a lexical trust match.
// Canonicalization makes the subsequent filesystem walk stable, and the
// ancestor check prevents an in-anchor symlink from escaping to an unrelated
// physical tree.
func admitTrustedPath(cwd, root string) (string, string, error) {
	physicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", fmt.Errorf("%w: resolve trusted anchor", ErrUnsafePath)
	}
	physicalCWD, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", "", fmt.Errorf("%w: resolve trusted cwd", ErrUnsafePath)
	}
	rootInfo, err := os.Stat(physicalRoot)
	if err != nil || !rootInfo.IsDir() {
		return "", "", fmt.Errorf("%w: trusted anchor", ErrUnsafePath)
	}
	cwdInfo, err := os.Stat(physicalCWD)
	if err != nil || !cwdInfo.IsDir() {
		return "", "", fmt.Errorf("%w: trusted cwd", ErrUnsafePath)
	}
	relative, err := filepath.Rel(physicalRoot, physicalCWD)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", "", fmt.Errorf("%w: trusted cwd escapes anchor", ErrUnsafePath)
	}
	return filepath.Clean(physicalCWD), filepath.Clean(physicalRoot), nil
}

// verifyTrustedPath rejects every symlink component under a trusted anchor.
// It is repeated immediately before descriptor acquisition: if a parent is
// swapped after this check, the Lstat/Open identity comparison below rejects
// the newly opened directory or file instead.
func verifyTrustedPath(anchor, path string) error {
	if anchor == "" {
		return nil
	}
	relative, err := filepath.Rel(anchor, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("%w: resource escapes trusted anchor", ErrUnsafePath)
	}
	current := anchor
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: trusted resource path", ErrUnsafePath)
		}
	}
	resolved, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: trusted resource path", ErrUnsafePath)
	}
	relative, err = filepath.Rel(anchor, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("%w: resource escapes trusted anchor", ErrUnsafePath)
	}
	return nil
}

func load(ctx context.Context, c Config, decision trustDecision) (Snapshot, error) {
	trusted := decision.Known && decision.Trusted
	out := Snapshot{Trusted: trusted}
	var err error
	if out.Instructions, err = loadInstructions(ctx, c.AgentDir, ScopeGlobal, c.MaxFileBytes, ""); err != nil {
		return Snapshot{}, err
	}
	if trusted {
		project, err := loadProjectInstructions(ctx, c.CWD, decision.Root, c.MaxFileBytes)
		if err != nil {
			return Snapshot{}, err
		}
		out.Instructions = append(out.Instructions, project...)
	}
	if out.SystemPrompt, err = optionalFile(ctx, filepath.Join(c.AgentDir, "SYSTEM.md"), c.MaxFileBytes, ""); err != nil {
		return Snapshot{}, err
	}
	if trusted {
		value, err := optionalFile(ctx, filepath.Join(c.CWD, ".pi", "SYSTEM.md"), c.MaxFileBytes, decision.Root)
		if err != nil {
			return Snapshot{}, err
		}
		if value != "" {
			out.SystemPrompt = value
		}
	}
	if value, err := optionalFile(ctx, filepath.Join(c.AgentDir, "APPEND_SYSTEM.md"), c.MaxFileBytes, ""); err != nil {
		return Snapshot{}, err
	} else if value != "" {
		out.AppendSystem = append(out.AppendSystem, value)
	}
	if trusted {
		value, err := optionalFile(ctx, filepath.Join(c.CWD, ".pi", "APPEND_SYSTEM.md"), c.MaxFileBytes, decision.Root)
		if err != nil {
			return Snapshot{}, err
		} else if value != "" {
			out.AppendSystem = append(out.AppendSystem, value)
		}
	}
	if out.Templates, err = loadTemplates(ctx, filepath.Join(c.AgentDir, "prompts"), ScopeGlobal, c.MaxFileBytes, ""); err != nil {
		return Snapshot{}, err
	}
	if trusted {
		project, err := loadTemplates(ctx, filepath.Join(c.CWD, ".pi", "prompts"), ScopeProject, c.MaxFileBytes, decision.Root)
		if err != nil {
			return Snapshot{}, err
		}
		out.Templates, out.Diagnostics = mergeTemplates(out.Templates, project, out.Diagnostics)
	}
	if out.Skills, err = loadSkills(ctx, filepath.Join(c.AgentDir, "skills"), ScopeGlobal, c.MaxFileBytes, ""); err != nil {
		return Snapshot{}, err
	}
	out.Skills, out.Diagnostics = mergeSkills(nil, out.Skills, out.Diagnostics)
	if trusted {
		for _, dir := range []string{filepath.Join(c.CWD, ".pi", "skills"), filepath.Join(c.CWD, ".agents", "skills")} {
			project, err := loadSkills(ctx, dir, ScopeProject, c.MaxFileBytes, decision.Root)
			if err != nil {
				return Snapshot{}, err
			}
			out.Skills, out.Diagnostics = mergeSkills(out.Skills, project, out.Diagnostics)
		}
	}
	out.SystemPrompt, err = assemble(c, out)
	if err != nil {
		return Snapshot{}, err
	}
	return out, nil
}

func check(ctx context.Context) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	return nil
}
func safeRead(ctx context.Context, path string, max int64, anchor string) (string, error) {
	if err := check(ctx); err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("%w: inspect resource", ErrMalformed)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %s", ErrUnsafePath, filepath.Base(path))
	}
	if err := verifyTrustedPath(anchor, path); err != nil {
		return "", err
	}
	if info.Size() > max {
		return "", fmt.Errorf("%w: %s", ErrTooLarge, filepath.Base(path))
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("%w: read resource", ErrMalformed)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return "", fmt.Errorf("%w: %s", ErrUnsafePath, filepath.Base(path))
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return "", fmt.Errorf("%w: read resource", ErrMalformed)
	}
	if int64(len(data)) > max || !utf8.Valid(data) {
		return "", fmt.Errorf("%w: %s", ErrMalformed, filepath.Base(path))
	}
	return string(data), nil
}
func optionalFile(ctx context.Context, path string, max int64, anchor string) (string, error) {
	return safeRead(ctx, path, max, anchor)
}
func directory(ctx context.Context, path string, anchor string) ([]os.DirEntry, error) {
	if err := check(ctx); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: inspect resource directory", ErrMalformed)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("%w: resource directory", ErrUnsafePath)
	}
	if err := verifyTrustedPath(anchor, path); err != nil {
		return nil, err
	}
	if afterDirectoryLstat != nil {
		afterDirectoryLstat(path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: read resource directory", ErrMalformed)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.IsDir() || !os.SameFile(info, opened) {
		return nil, fmt.Errorf("%w: resource directory", ErrUnsafePath)
	}
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("%w: read resource directory", ErrMalformed)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}
func loadInstructions(ctx context.Context, dir string, scope Scope, max int64, anchor string) ([]Instruction, error) {
	for _, name := range []string{"AGENTS.md", "AGENTS.MD", "CLAUDE.md", "CLAUDE.MD"} {
		value, err := safeRead(ctx, filepath.Join(dir, name), max, anchor)
		if err != nil {
			return nil, err
		}
		if value != "" {
			return []Instruction{{Source: Source{Path: filepath.Join(dir, name), Scope: scope}, Content: value}}, nil
		}
	}
	return nil, nil
}
func loadProjectInstructions(ctx context.Context, cwd, trustRoot string, max int64) ([]Instruction, error) {
	if trustRoot == "" {
		return nil, fmt.Errorf("%w: missing trust root", ErrTrustStore)
	}
	var dirs []string
	for d := cwd; ; d = filepath.Dir(d) {
		dirs = append(dirs, d)
		if d == trustRoot {
			break
		}
		if filepath.Dir(d) == d {
			return nil, fmt.Errorf("%w: trust root is not an ancestor", ErrTrustStore)
		}
	}
	for i, j := 0, len(dirs)-1; i < j; i, j = i+1, j-1 {
		dirs[i], dirs[j] = dirs[j], dirs[i]
	}
	var out []Instruction
	for _, dir := range dirs {
		got, err := loadInstructions(ctx, dir, ScopeProject, max, trustRoot)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}
func loadTemplates(ctx context.Context, dir string, scope Scope, max int64, anchor string) ([]Template, error) {
	entries, err := directory(ctx, dir, anchor)
	if err != nil {
		return nil, err
	}
	var out []Template
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, err := safeRead(ctx, path, max, anchor)
		if err != nil {
			return nil, err
		}
		if raw == "" {
			continue
		}
		front, body, err := frontmatter(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: template %s", ErrMalformed, entry.Name())
		}
		desc := front["description"]
		if desc == "" {
			for _, line := range strings.Split(body, "\n") {
				if strings.TrimSpace(line) != "" {
					desc = line
					if utf8.RuneCountInString(desc) > 60 {
						desc = truncateRunes(desc, 60) + "..."
					}
					break
				}
			}
		}
		out = append(out, Template{Source: Source{path, scope}, Name: strings.TrimSuffix(entry.Name(), ".md"), Description: desc, ArgumentHint: front["argument-hint"], Content: body})
	}
	return out, nil
}
func loadSkills(ctx context.Context, dir string, scope Scope, max int64, anchor string) ([]Skill, error) {
	var out []Skill
	var visit func(string) error
	visit = func(current string) error {
		entries, err := directory(ctx, current, anchor)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.Name() == "SKILL.md" {
				raw, err := safeRead(ctx, filepath.Join(current, entry.Name()), max, anchor)
				if err != nil {
					return err
				}
				front, _, kinds, err := frontmatterDetailed(raw)
				if err != nil {
					return fmt.Errorf("%w: SKILL.md", ErrMalformed)
				}
				name := front["name"]
				if name == "" {
					name = filepath.Base(current)
				}
				desc := front["description"]
				if !validSkill(name, desc) {
					return fmt.Errorf("%w: invalid skill", ErrMalformed)
				}
				disable := false
				if raw, exists := front["disable-model-invocation"]; exists {
					boolean := strings.ToLower(raw)
					if kinds["disable-model-invocation"] == scalarQuoted || (boolean != "true" && boolean != "false") {
						return fmt.Errorf("%w: invalid disable-model-invocation", ErrMalformed)
					}
					disable = boolean == "true"
				}
				out = append(out, Skill{Source: Source{filepath.Join(current, entry.Name()), scope}, Name: name, Description: desc, BaseDir: current, DisableModelInvocation: disable})
				return nil
			}
			if strings.HasPrefix(entry.Name(), ".") || entry.Name() == "node_modules" {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return fmt.Errorf("%w: inspect skill", ErrMalformed)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%w: skill symlink", ErrUnsafePath)
			}
			if info.IsDir() {
				if err := visit(filepath.Join(current, entry.Name())); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(dir); err != nil {
		return nil, err
	}
	return out, nil
}
func validSkill(name, desc string) bool {
	if name == "" || len(name) > 64 || strings.TrimSpace(desc) == "" || utf8.RuneCountInString(desc) > 1024 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return !strings.HasPrefix(name, "-") && !strings.HasSuffix(name, "-") && !strings.Contains(name, "--")
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	count := 0
	for offset := range value {
		if count == limit {
			return value[:offset]
		}
		count++
	}
	return value
}
func mergeTemplates(base, project []Template, diagnostics []Diagnostic) ([]Template, []Diagnostic) {
	index := map[string]int{}
	for i, v := range base {
		index[v.Name] = i
	}
	for _, v := range project {
		if i, ok := index[v.Name]; ok {
			diagnostics = append(diagnostics, Diagnostic{Kind: "collision", Resource: "template", Name: v.Name, WinnerPath: v.Path, LoserPath: base[i].Path})
			base[i] = v
		} else {
			index[v.Name] = len(base)
			base = append(base, v)
		}
	}
	return base, diagnostics
}
func mergeSkills(base, project []Skill, diagnostics []Diagnostic) ([]Skill, []Diagnostic) {
	index := map[string]int{}
	for i, v := range base {
		index[v.Name] = i
	}
	for _, v := range project {
		if i, ok := index[v.Name]; ok {
			winner := base[i]
			loser := v
			if winner.Scope == ScopeGlobal && v.Scope == ScopeProject {
				winner, loser = v, winner
				base[i] = v
			}
			diagnostics = append(diagnostics, Diagnostic{Kind: "collision", Resource: "skill", Name: v.Name, WinnerPath: winner.Path, LoserPath: loser.Path})
		} else {
			index[v.Name] = len(base)
			base = append(base, v)
		}
	}
	return base, diagnostics
}
