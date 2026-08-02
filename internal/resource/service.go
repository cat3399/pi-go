package resource

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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

func New(config Config) (*Service, error) {
	config, err := validateConfig(config)
	if err != nil {
		return nil, err
	}
	// Keep filesystem discovery and trust lookup on the same physical cwd.  This
	// also makes an ancestor decision stable when callers entered via a symlink.
	if config.CWD, err = normalize(config.CWD); err != nil {
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
	next, err := load(ctx, s.config, decision)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.snapshot = &next
	s.mu.Unlock()
	return nil
}

func load(ctx context.Context, c Config, decision trustDecision) (Snapshot, error) {
	trusted := decision.Known && decision.Trusted
	out := Snapshot{Trusted: trusted}
	var err error
	if out.Instructions, err = loadInstructions(ctx, c.AgentDir, ScopeGlobal, c.MaxFileBytes); err != nil {
		return Snapshot{}, err
	}
	if trusted {
		project, err := loadProjectInstructions(ctx, c.CWD, decision.Root, c.MaxFileBytes)
		if err != nil {
			return Snapshot{}, err
		}
		out.Instructions = append(out.Instructions, project...)
	}
	if out.SystemPrompt, err = optionalFile(ctx, filepath.Join(c.AgentDir, "SYSTEM.md"), c.MaxFileBytes); err != nil {
		return Snapshot{}, err
	}
	if trusted {
		value, err := optionalFile(ctx, filepath.Join(c.CWD, ".pi", "SYSTEM.md"), c.MaxFileBytes)
		if err != nil {
			return Snapshot{}, err
		}
		if value != "" {
			out.SystemPrompt = value
		}
	}
	if value, err := optionalFile(ctx, filepath.Join(c.AgentDir, "APPEND_SYSTEM.md"), c.MaxFileBytes); err != nil {
		return Snapshot{}, err
	} else if value != "" {
		out.AppendSystem = append(out.AppendSystem, value)
	}
	if trusted {
		value, err := optionalFile(ctx, filepath.Join(c.CWD, ".pi", "APPEND_SYSTEM.md"), c.MaxFileBytes)
		if err != nil {
			return Snapshot{}, err
		} else if value != "" {
			out.AppendSystem = append(out.AppendSystem, value)
		}
	}
	if out.Templates, err = loadTemplates(ctx, filepath.Join(c.AgentDir, "prompts"), ScopeGlobal, c.MaxFileBytes); err != nil {
		return Snapshot{}, err
	}
	if trusted {
		project, err := loadTemplates(ctx, filepath.Join(c.CWD, ".pi", "prompts"), ScopeProject, c.MaxFileBytes)
		if err != nil {
			return Snapshot{}, err
		}
		out.Templates, out.Diagnostics = mergeTemplates(out.Templates, project, out.Diagnostics)
	}
	if out.Skills, err = loadSkills(ctx, filepath.Join(c.AgentDir, "skills"), ScopeGlobal, c.MaxFileBytes); err != nil {
		return Snapshot{}, err
	}
	if trusted {
		for _, dir := range []string{filepath.Join(c.CWD, ".pi", "skills"), filepath.Join(c.CWD, ".agents", "skills")} {
			project, err := loadSkills(ctx, dir, ScopeProject, c.MaxFileBytes)
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
func safeRead(ctx context.Context, path string, max int64) (string, error) {
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
func optionalFile(ctx context.Context, path string, max int64) (string, error) {
	return safeRead(ctx, path, max)
}
func directory(ctx context.Context, path string) ([]os.DirEntry, error) {
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
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("%w: read resource directory", ErrMalformed)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}
func loadInstructions(ctx context.Context, dir string, scope Scope, max int64) ([]Instruction, error) {
	for _, name := range []string{"AGENTS.md", "AGENTS.MD", "CLAUDE.md", "CLAUDE.MD"} {
		value, err := safeRead(ctx, filepath.Join(dir, name), max)
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
		got, err := loadInstructions(ctx, dir, ScopeProject, max)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}
func loadTemplates(ctx context.Context, dir string, scope Scope, max int64) ([]Template, error) {
	entries, err := directory(ctx, dir)
	if err != nil {
		return nil, err
	}
	var out []Template
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, err := safeRead(ctx, path, max)
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
					if len(desc) > 60 {
						desc = desc[:60] + "..."
					}
					break
				}
			}
		}
		out = append(out, Template{Source: Source{path, scope}, Name: strings.TrimSuffix(entry.Name(), ".md"), Description: desc, ArgumentHint: front["argument-hint"], Content: body})
	}
	return out, nil
}
func loadSkills(ctx context.Context, dir string, scope Scope, max int64) ([]Skill, error) {
	var out []Skill
	var visit func(string) error
	visit = func(current string) error {
		entries, err := directory(ctx, current)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.Name() == "SKILL.md" {
				raw, err := safeRead(ctx, filepath.Join(current, entry.Name()), max)
				if err != nil {
					return err
				}
				front, _, err := frontmatter(raw)
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
					parsed, parseErr := strconv.ParseBool(raw)
					if parseErr != nil {
						return fmt.Errorf("%w: invalid disable-model-invocation", ErrMalformed)
					}
					disable = parsed
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
	if name == "" || len(name) > 64 || desc == "" || len(desc) > 1024 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return !strings.HasPrefix(name, "-") && !strings.HasSuffix(name, "-") && !strings.Contains(name, "--")
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
			diagnostics = append(diagnostics, Diagnostic{Kind: "collision", Resource: "skill", Name: v.Name, WinnerPath: v.Path, LoserPath: base[i].Path})
			base[i] = v
		} else {
			index[v.Name] = len(base)
			base = append(base, v)
		}
	}
	return base, diagnostics
}
