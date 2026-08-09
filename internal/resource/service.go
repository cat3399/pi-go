package resource

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf16"
)

type Service struct {
	config        Config
	trust         *TrustStore
	mu            sync.RWMutex
	generation    uint64
	snapshot      *Snapshot
	beforePublish func(uint64)
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
	s.mu.Lock()
	s.generation++
	generation := s.generation
	s.mu.Unlock()
	decision, err := s.trust.decision(ctx, s.config.CWD)
	if err != nil {
		return err
	}
	admission := decision
	config := s.config
	effectiveDecision := decision
	// The original trust resolver treats a project with no trust-requiring
	// resources as trusted even when an old explicit decision is present. Keep
	// the stored decision for final publication validation; it becomes effective
	// again if a gated resource is later added.
	if !HasTrustRequiringProjectResources(config.CWD) {
		effectiveDecision = trustDecision{Trusted: true, Known: true, Root: config.CWD}
	}
	next, err := load(ctx, config, effectiveDecision)
	if err != nil {
		return err
	}
	if s.beforePublish != nil {
		s.beforePublish(generation)
	}
	// confirmDecision holds the trust-store serialization token from the final
	// durable re-read through publication. The service mutex then makes the
	// generation comparison and assignment one indivisible state transition.
	return s.trust.confirmDecision(ctx, s.config.CWD, admission, func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if generation != s.generation {
			return ErrStaleReload
		}
		s.snapshot = &next
		return nil
	})
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
	if !c.NoContextFiles {
		out.Instructions, err = loadAllInstructions(ctx, c.CWD, c.AgentDir, c.MaxFileBytes)
		if err != nil {
			return Snapshot{}, err
		}
	}
	out.BaseSystemPrompt, err = discoverSystemPrompt(ctx, c, trusted)
	if err != nil {
		return Snapshot{}, err
	}
	out.AppendSystem, err = discoverAppendSystemPrompts(ctx, c, trusted)
	if err != nil {
		return Snapshot{}, err
	}
	if !c.NoPromptTemplates || len(c.PromptSources) > 0 || len(c.PromptPaths) > 0 {
		out.Templates, out.Diagnostics = discoverTemplates(ctx, c, trusted, out.Diagnostics)
	}
	if !c.NoSkills || len(c.SkillSources) > 0 || len(c.SkillPaths) > 0 {
		out.Skills, out.Diagnostics = discoverSkills(ctx, c, trusted, out.Diagnostics)
	}
	out.SystemPrompt, err = assemble(c, out)
	if err != nil {
		return Snapshot{}, err
	}
	return out, nil
}

func loadAllInstructions(ctx context.Context, cwd, agentDir string, max int64) ([]Instruction, error) {
	var out []Instruction
	global, err := loadInstructions(ctx, agentDir, ScopeGlobal, max, "")
	if err != nil {
		return nil, err
	}
	out = append(out, global...)
	var directories []string
	for current := filepath.Clean(cwd); ; current = filepath.Dir(current) {
		directories = append(directories, current)
		if parent := filepath.Dir(current); parent == current {
			break
		}
	}
	for left, right := 0, len(directories)-1; left < right; left, right = left+1, right-1 {
		directories[left], directories[right] = directories[right], directories[left]
	}
	shadowed, err := findShadowedContextFile(ctx, cwd, max)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	for _, instruction := range out {
		seen[instruction.Path] = struct{}{}
	}
	for _, directory := range directories {
		loaded, err := loadInstructions(ctx, directory, ScopeProject, max, "")
		if err != nil {
			return nil, err
		}
		for _, instruction := range loaded {
			if shadowed != "" && canonicalResourcePath(instruction.Path) == shadowed {
				continue
			}
			if _, ok := seen[instruction.Path]; ok {
				continue
			}
			seen[instruction.Path] = struct{}{}
			out = append(out, instruction)
		}
	}
	return out, nil
}

type gitResourcePaths struct {
	repoDir, commonGitDir string
}

// findGitResourcePaths is the filesystem-only equivalent of pi's findGitPaths.
// It handles ordinary repositories and linked worktree gitdir files without
// invoking git, which keeps context discovery deterministic at startup.
func findGitResourcePaths(cwd string) (gitResourcePaths, bool) {
	for directory := filepath.Clean(cwd); ; directory = filepath.Dir(directory) {
		gitPath := filepath.Join(directory, ".git")
		info, err := os.Stat(gitPath)
		if err == nil {
			if info.Mode().IsRegular() {
				data, readErr := os.ReadFile(gitPath)
				if readErr != nil {
					return gitResourcePaths{}, false
				}
				content := strings.TrimFunc(strings.ToValidUTF8(string(data), "�"), isECMAScriptWhitespace)
				if !strings.HasPrefix(content, "gitdir: ") {
					return gitResourcePaths{}, false
				}
				gitDir := strings.TrimFunc(content[8:], isECMAScriptWhitespace)
				if !filepath.IsAbs(gitDir) {
					gitDir = filepath.Join(directory, gitDir)
				}
				gitDir = filepath.Clean(gitDir)
				if _, headErr := os.Stat(filepath.Join(gitDir, "HEAD")); headErr != nil {
					return gitResourcePaths{}, false
				}
				commonGitDir := gitDir
				if commonData, commonErr := os.ReadFile(filepath.Join(gitDir, "commondir")); commonErr == nil {
					commonGitDir = strings.TrimFunc(strings.ToValidUTF8(string(commonData), "�"), isECMAScriptWhitespace)
					if !filepath.IsAbs(commonGitDir) {
						commonGitDir = filepath.Join(gitDir, commonGitDir)
					}
					commonGitDir = filepath.Clean(commonGitDir)
				}
				return gitResourcePaths{repoDir: directory, commonGitDir: commonGitDir}, true
			}
			if info.IsDir() {
				if _, headErr := os.Stat(filepath.Join(gitPath, "HEAD")); headErr != nil {
					return gitResourcePaths{}, false
				}
				return gitResourcePaths{repoDir: directory, commonGitDir: gitPath}, true
			}
			return gitResourcePaths{}, false
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return gitResourcePaths{}, false
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return gitResourcePaths{}, false
		}
	}
}

func canonicalResourcePath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

func strictDescendant(path, ancestor string) bool {
	relative, err := filepath.Rel(ancestor, path)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

// findShadowedContextFile identifies only the tracked context file duplicated
// by a linked worktree nested under its main checkout. Ordinary repositories,
// sibling worktrees, bare layouts, and submodules intentionally return none.
func findShadowedContextFile(ctx context.Context, cwd string, max int64) (string, error) {
	gitPaths, ok := findGitResourcePaths(cwd)
	if !ok {
		return "", nil
	}
	commonGitDir := canonicalResourcePath(gitPaths.commonGitDir)
	worktreeRoot := canonicalResourcePath(gitPaths.repoDir)
	mainRepoRoot := filepath.Dir(commonGitDir)
	if !strictDescendant(worktreeRoot, mainRepoRoot) {
		return "", nil
	}
	if canonicalResourcePath(filepath.Join(mainRepoRoot, ".git")) != commonGitDir {
		return "", nil
	}
	loaded, err := loadInstructions(ctx, worktreeRoot, ScopeProject, max, "")
	if err != nil {
		return "", err
	}
	if len(loaded) == 0 {
		return "", nil
	}
	return filepath.Join(mainRepoRoot, filepath.Base(loaded[0].Path)), nil
}

func discoverSystemPrompt(ctx context.Context, c Config, trusted bool) (string, error) {
	if c.SystemPromptSource != "" {
		return resolvePromptInput(ctx, c.SystemPromptSource, c.CWD, c.MaxFileBytes)
	}
	if trusted {
		project := filepath.Join(c.CWD, ".pi", "SYSTEM.md")
		if resourcePathExists(project) {
			return resolvePromptInput(ctx, project, c.CWD, c.MaxFileBytes)
		}
	}
	global := filepath.Join(c.AgentDir, "SYSTEM.md")
	if resourcePathExists(global) {
		return resolvePromptInput(ctx, global, c.CWD, c.MaxFileBytes)
	}
	return "", nil
}

func discoverAppendSystemPrompts(ctx context.Context, c Config, trusted bool) ([]string, error) {
	if c.AppendSystemPromptSources != nil {
		out := make([]string, 0, len(c.AppendSystemPromptSources))
		for _, source := range c.AppendSystemPromptSources {
			value, err := resolvePromptInput(ctx, source, c.CWD, c.MaxFileBytes)
			if err != nil {
				return nil, err
			}
			out = append(out, value)
		}
		return out, nil
	}
	path := ""
	if trusted {
		candidate := filepath.Join(c.CWD, ".pi", "APPEND_SYSTEM.md")
		if resourcePathExists(candidate) {
			path = candidate
		}
	}
	if path == "" {
		candidate := filepath.Join(c.AgentDir, "APPEND_SYSTEM.md")
		if resourcePathExists(candidate) {
			path = candidate
		}
	}
	if path == "" {
		return nil, nil
	}
	value, err := resolvePromptInput(ctx, path, c.CWD, c.MaxFileBytes)
	if err != nil {
		return nil, err
	}
	return []string{value}, nil
}

func resolvePromptInput(ctx context.Context, input, cwd string, max int64) (string, error) {
	path := input
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	if resourcePathExists(path) {
		value, exists, err := readResourceFile(ctx, path, max)
		if err != nil {
			return "", err
		}
		if exists {
			return value, nil
		}
	}
	return input, nil
}

func resourcePathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

type resourceRoot struct {
	path     string
	source   Source
	explicit bool
}

func defaultResourceSource(path string, scope Scope) Source {
	return Source{Path: path, Source: "local", Scope: scope, Origin: OriginTopLevel, BaseDir: resourceBaseDir(path)}
}

func resourceBaseDir(path string) string {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return filepath.Clean(path)
	}
	return filepath.Dir(filepath.Clean(path))
}

func resolveConfiguredSource(source Source, c Config) Source {
	source.Path = resolveResourcePath(source.Path, c.CWD, c.HomeDir)
	if source.Source == "" {
		source.Source = "local"
	}
	if source.Scope == "" {
		source.Scope = scopeForPath(source.Path, c)
	}
	if source.Origin == "" {
		source.Origin = OriginTopLevel
	}
	if source.BaseDir == "" {
		source.BaseDir = resourceBaseDir(source.Path)
	} else {
		source.BaseDir = resolveResourcePath(source.BaseDir, c.CWD, c.HomeDir)
	}
	return source
}

func sourceAtPath(source Source, path string) Source {
	source.Path = path
	if source.Source == "" {
		source.Source = "local"
	}
	if source.Scope == "" {
		source.Scope = ScopeTemporary
	}
	if source.Origin == "" {
		source.Origin = OriginTopLevel
	}
	if source.BaseDir == "" {
		source.BaseDir = resourceBaseDir(path)
	}
	return source
}

type canonicalResourceRootSet struct {
	seen         map[string]struct{}
	canonicalize func(string) string
}

func newCanonicalResourceRootSet() *canonicalResourceRootSet {
	return &canonicalResourceRootSet{seen: map[string]struct{}{}, canonicalize: canonicalPath}
}

func (s *canonicalResourceRootSet) add(path string) bool {
	canonical := s.canonicalize(path)
	if _, exists := s.seen[canonical]; exists {
		return false
	}
	s.seen[canonical] = struct{}{}
	return true
}

func appendResourceRoot(roots []resourceRoot, seen *canonicalResourceRootSet, candidate resourceRoot) []resourceRoot {
	if !seen.add(candidate.path) {
		return roots
	}
	return append(roots, candidate)
}

func optionalStringFrontmatter(values map[string]any, name string) (string, error) {
	value, exists := values[name]
	if !exists {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("frontmatter field %q must be a string", name)
	}
	return text, nil
}

func warningDiagnostic(resource string, source Source, message string) Diagnostic {
	return Diagnostic{Kind: "warning", Resource: resource, Path: source.Path, Source: source, Message: message}
}

func collisionDiagnostic(resource, name string, winner, loser Source) Diagnostic {
	return Diagnostic{
		Kind: "collision", Resource: resource, Name: name,
		WinnerPath: winner.Path, LoserPath: loser.Path, Path: loser.Path,
		Source: loser, WinnerSource: winner, LoserSource: loser,
		Message: fmt.Sprintf("name %q collision", name),
	}
}

func discoverTemplates(ctx context.Context, c Config, trusted bool, diagnostics []Diagnostic) ([]Template, []Diagnostic) {
	var candidates []Template
	var roots []resourceRoot
	rootSet := newCanonicalResourceRootSet()
	if !c.NoPromptTemplates {
		if trusted {
			path := filepath.Join(c.CWD, ".pi", "prompts")
			roots = appendResourceRoot(roots, rootSet, resourceRoot{path: path, source: defaultResourceSource(path, ScopeProject)})
		}
		path := filepath.Join(c.AgentDir, "prompts")
		roots = appendResourceRoot(roots, rootSet, resourceRoot{path: path, source: defaultResourceSource(path, ScopeUser)})
	}
	for _, configured := range c.PromptSources {
		source := resolveConfiguredSource(configured, c)
		roots = appendResourceRoot(roots, rootSet, resourceRoot{path: source.Path, source: source, explicit: true})
	}
	for _, rawPath := range c.PromptPaths {
		path := resolveResourcePath(rawPath, c.CWD, c.HomeDir)
		source := defaultResourceSource(path, scopeForPath(path, c))
		source.BaseDir = resourceBaseDir(path)
		roots = appendResourceRoot(roots, rootSet, resourceRoot{path: path, source: source, explicit: true})
	}
	for _, root := range roots {
		if _, err := os.Stat(root.path); err != nil && root.explicit {
			diagnostics = append(diagnostics, warningDiagnostic("template", sourceAtPath(root.source, root.path), "prompt template path does not exist"))
			continue
		}
		loaded, found := loadTemplatePath(ctx, root.path, root.source, c.MaxFileBytes)
		candidates = append(candidates, loaded...)
		diagnostics = append(diagnostics, found...)
	}
	seen := map[string]Template{}
	out := make([]Template, 0, len(candidates))
	for _, candidate := range candidates {
		if winner, ok := seen[candidate.Name]; ok {
			diagnostics = append(diagnostics, collisionDiagnostic("template", candidate.Name, winner.Source, candidate.Source))
			continue
		}
		seen[candidate.Name] = candidate
		out = append(out, candidate)
	}
	return out, diagnostics
}

func loadTemplatePath(ctx context.Context, path string, source Source, max int64) ([]Template, []Diagnostic) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil
	}
	if info.IsDir() {
		loaded, diagnostics, _ := loadTemplates(ctx, path, source, max, "")
		return loaded, diagnostics
	}
	if !info.Mode().IsRegular() || !strings.HasSuffix(path, ".md") {
		return nil, nil
	}
	value, diagnostics := loadTemplateFile(ctx, path, source, max)
	if value.Name == "" {
		return nil, diagnostics
	}
	return []Template{value}, diagnostics
}

func loadTemplateFile(ctx context.Context, path string, source Source, max int64) (Template, []Diagnostic) {
	source = sourceAtPath(source, path)
	raw, exists, err := readResourceFile(ctx, path, max)
	if err != nil || !exists {
		return Template{}, nil
	}
	front, body, err := parseFrontmatter(raw)
	if err != nil {
		return Template{}, []Diagnostic{warningDiagnostic("template", source, err.Error())}
	}
	if err := validateKnownFrontmatterTypes(front); err != nil {
		return Template{}, []Diagnostic{warningDiagnostic("template", source, err.Error())}
	}
	description, err := optionalStringFrontmatter(front, "description")
	if err != nil {
		return Template{}, []Diagnostic{warningDiagnostic("template", source, err.Error())}
	}
	if description == "" {
		for _, line := range strings.Split(body, "\n") {
			if strings.TrimFunc(line, isECMAScriptWhitespace) == "" {
				continue
			}
			description = truncateUTF16(line, 60)
			if utf16Length(line) > 60 {
				description += "..."
			}
			break
		}
	}
	argumentHint, err := optionalStringFrontmatter(front, "argument-hint")
	if err != nil {
		return Template{}, []Diagnostic{warningDiagnostic("template", source, err.Error())}
	}
	return Template{Source: source, Name: strings.TrimSuffix(filepath.Base(path), ".md"), Description: description, ArgumentHint: argumentHint, Content: body}, nil
}

func discoverSkills(ctx context.Context, c Config, trusted bool, diagnostics []Diagnostic) ([]Skill, []Diagnostic) {
	type skillRoot struct {
		path             string
		source           Source
		includeRootFiles bool
		explicit         bool
	}
	var roots []skillRoot
	rootSet := newCanonicalResourceRootSet()
	appendRoot := func(root skillRoot) {
		if !rootSet.add(root.path) {
			return
		}
		roots = append(roots, root)
	}
	if !c.NoSkills {
		if trusted {
			path := filepath.Join(c.CWD, ".pi", "skills")
			appendRoot(skillRoot{path: path, source: defaultResourceSource(path, ScopeProject), includeRootFiles: true})
			for _, directory := range ancestorAgentSkillDirectories(c.CWD) {
				if samePath(directory, userAgentSkillsDirectory(c.HomeDir)) {
					continue
				}
				appendRoot(skillRoot{path: directory, source: defaultResourceSource(directory, ScopeProject)})
			}
		}
		path := filepath.Join(c.AgentDir, "skills")
		appendRoot(skillRoot{path: path, source: defaultResourceSource(path, ScopeUser), includeRootFiles: true})
		path = userAgentSkillsDirectory(c.HomeDir)
		appendRoot(skillRoot{path: path, source: defaultResourceSource(path, ScopeUser)})
	}
	for _, configured := range c.SkillSources {
		source := resolveConfiguredSource(configured, c)
		appendRoot(skillRoot{path: source.Path, source: source, includeRootFiles: true, explicit: true})
	}
	for _, rawPath := range c.SkillPaths {
		path := resolveResourcePath(rawPath, c.CWD, c.HomeDir)
		source := defaultResourceSource(path, scopeForPath(path, c))
		source.BaseDir = resourceBaseDir(path)
		appendRoot(skillRoot{path: path, source: source, includeRootFiles: true, explicit: true})
	}

	var candidates []Skill
	for _, root := range roots {
		if _, err := os.Stat(root.path); err != nil && !root.explicit {
			continue
		}
		loaded, foundDiagnostics := loadSkillPath(ctx, root.path, root.source, root.includeRootFiles, c.MaxFileBytes)
		candidates = append(candidates, loaded...)
		diagnostics = append(diagnostics, foundDiagnostics...)
	}
	byName := map[string]Skill{}
	realFiles := map[string]struct{}{}
	out := make([]Skill, 0, len(candidates))
	for _, candidate := range candidates {
		real := canonicalPath(candidate.Path)
		if _, ok := realFiles[real]; ok {
			continue
		}
		if winner, ok := byName[candidate.Name]; ok {
			diagnostics = append(diagnostics, collisionDiagnostic("skill", candidate.Name, winner.Source, candidate.Source))
			continue
		}
		byName[candidate.Name] = candidate
		realFiles[real] = struct{}{}
		out = append(out, candidate)
	}
	return out, diagnostics
}

func loadSkillPath(ctx context.Context, path string, source Source, includeRootFiles bool, max int64) ([]Skill, []Diagnostic) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, []Diagnostic{warningDiagnostic("skill", sourceAtPath(source, path), "skill path does not exist")}
	}
	if info.Mode().IsRegular() {
		if !strings.HasSuffix(path, ".md") {
			return nil, []Diagnostic{warningDiagnostic("skill", sourceAtPath(source, path), "skill path is not a markdown file")}
		}
		skill, found := loadSkillFile(ctx, path, source, max)
		if skill.Name == "" {
			return nil, found
		}
		return []Skill{skill}, found
	}
	matcher := &resourceIgnoreMatcher{}
	visited := map[string]struct{}{}
	return scanSkillDirectory(ctx, path, path, source, includeRootFiles, max, matcher, visited)
}

func scanSkillDirectory(ctx context.Context, directory, root string, source Source, includeRootFiles bool, max int64, matcher *resourceIgnoreMatcher, visited map[string]struct{}) ([]Skill, []Diagnostic) {
	if err := check(ctx); err != nil {
		return nil, []Diagnostic{warningDiagnostic("skill", sourceAtPath(source, directory), err.Error())}
	}
	realDirectory := canonicalPath(directory)
	if _, ok := visited[realDirectory]; ok {
		return nil, nil
	}
	visited[realDirectory] = struct{}{}
	matcher.addDirectory(directory, root)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, nil
	}
	for _, entry := range entries {
		if entry.Name() != "SKILL.md" {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || matcher.ignored(path, false, root) {
			continue
		}
		skill, diagnostics := loadSkillFile(ctx, path, source, max)
		if skill.Name == "" {
			return nil, diagnostics
		}
		return []Skill{skill}, diagnostics
	}
	var skills []Skill
	var diagnostics []Diagnostic
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") || entry.Name() == "node_modules" {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		info, err := os.Stat(path)
		if err != nil || matcher.ignored(path, info.IsDir(), root) {
			continue
		}
		if info.IsDir() {
			loaded, found := scanSkillDirectory(ctx, path, root, source, false, max, matcher, visited)
			skills = append(skills, loaded...)
			diagnostics = append(diagnostics, found...)
			continue
		}
		if includeRootFiles && info.Mode().IsRegular() && strings.HasSuffix(entry.Name(), ".md") {
			skill, found := loadSkillFile(ctx, path, source, max)
			if skill.Name != "" {
				skills = append(skills, skill)
			}
			diagnostics = append(diagnostics, found...)
		}
	}
	return skills, diagnostics
}

func loadSkillFile(ctx context.Context, path string, source Source, max int64) (Skill, []Diagnostic) {
	source = sourceAtPath(source, path)
	raw, err := safeRead(ctx, path, max, "")
	if err != nil {
		return Skill{}, []Diagnostic{warningDiagnostic("skill", source, err.Error())}
	}
	front, _, err := parseFrontmatter(raw)
	if err != nil {
		return Skill{}, []Diagnostic{warningDiagnostic("skill", source, err.Error())}
	}
	if err := validateKnownFrontmatterTypes(front); err != nil {
		return Skill{}, []Diagnostic{warningDiagnostic("skill", source, err.Error())}
	}
	description, err := optionalStringFrontmatter(front, "description")
	if err != nil {
		return Skill{}, []Diagnostic{warningDiagnostic("skill", source, err.Error())}
	}
	name, err := optionalStringFrontmatter(front, "name")
	if err != nil {
		return Skill{}, []Diagnostic{warningDiagnostic("skill", source, err.Error())}
	}
	if name == "" {
		name = filepath.Base(filepath.Dir(path))
	}
	var diagnostics []Diagnostic
	if description == "" || strings.TrimFunc(description, isECMAScriptWhitespace) == "" {
		diagnostics = append(diagnostics, warningDiagnostic("skill", source, "description is required"))
		return Skill{}, diagnostics
	}
	if utf16Length(description) > 1024 {
		diagnostics = append(diagnostics, warningDiagnostic("skill", source, fmt.Sprintf("description exceeds 1024 characters (%d)", utf16Length(description))))
	}
	for _, message := range validateSkillName(name) {
		diagnostics = append(diagnostics, warningDiagnostic("skill", source, message))
	}
	disable := false
	if value, exists := front["disable-model-invocation"]; exists {
		var ok bool
		disable, ok = value.(bool)
		if !ok {
			return Skill{}, append(diagnostics, warningDiagnostic("skill", source, "frontmatter field \"disable-model-invocation\" must be a boolean"))
		}
	}
	return Skill{Source: source, Name: name, Description: description, BaseDir: filepath.Dir(path), DisableModelInvocation: disable}, diagnostics
}

func validateSkillName(name string) []string {
	var errors []string
	if utf16Length(name) > 64 {
		errors = append(errors, fmt.Sprintf("name exceeds 64 characters (%d)", utf16Length(name)))
	}
	valid := regexp.MustCompile(`^[a-z0-9-]+$`).MatchString(name)
	if !valid {
		errors = append(errors, "name contains invalid characters (must be lowercase a-z, 0-9, hyphens only)")
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		errors = append(errors, "name must not start or end with a hyphen")
	}
	if strings.Contains(name, "--") {
		errors = append(errors, "name must not contain consecutive hyphens")
	}
	return errors
}

func resolveResourcePath(path, cwd, home string) string {
	path = strings.TrimFunc(path, isECMAScriptWhitespace)
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, path[2:])
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	return filepath.Clean(path)
}

func scopeForPath(path string, c Config) Scope {
	if pathWithin(path, filepath.Join(c.CWD, ".pi")) || pathWithin(path, filepath.Join(c.CWD, ".agents")) {
		return ScopeProject
	}
	if pathWithin(path, c.AgentDir) || pathWithin(path, userAgentSkillsDirectory(c.HomeDir)) {
		return ScopeUser
	}
	return ScopeTemporary
}

func pathWithin(path, root string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func canonicalPath(path string) string {
	real, err := filepath.EvalSymlinks(path)
	if err == nil {
		return filepath.Clean(real)
	}
	absolute, err := filepath.Abs(path)
	if err == nil {
		return filepath.Clean(absolute)
	}
	return filepath.Clean(path)
}

func samePath(first, second string) bool { return canonicalPath(first) == canonicalPath(second) }

func userAgentSkillsDirectory(home string) string {
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return filepath.Join(home, ".agents", "skills")
}

func ancestorAgentSkillDirectories(cwd string) []string {
	root := ""
	for current := filepath.Clean(cwd); ; current = filepath.Dir(current) {
		if _, err := os.Stat(filepath.Join(current, ".git")); err == nil {
			root = current
			break
		}
		if parent := filepath.Dir(current); parent == current {
			break
		}
	}
	var out []string
	for current := filepath.Clean(cwd); ; current = filepath.Dir(current) {
		out = append(out, filepath.Join(current, ".agents", "skills"))
		if current == root || filepath.Dir(current) == current {
			break
		}
	}
	return out
}

func utf16Length(value string) int { return len(utf16.Encode([]rune(value))) }

func truncateUTF16(value string, limit int) string {
	if limit < 1 {
		return ""
	}
	units := 0
	for offset, r := range value {
		width := 1
		if r > 0xffff {
			width = 2
		}
		if units+width > limit {
			return value[:offset]
		}
		units += width
	}
	return value
}

type resourceIgnoreRule struct {
	base, pattern               string
	negate, directory, basename bool
	match                       func(string) bool
}

type resourceIgnoreMatcher struct{ rules []resourceIgnoreRule }

func (m *resourceIgnoreMatcher) addDirectory(directory, root string) {
	prefix, _ := filepath.Rel(root, directory)
	prefix = filepath.ToSlash(prefix)
	if prefix == "." {
		prefix = ""
	} else if prefix != "" {
		prefix += "/"
	}
	for _, name := range []string{".gitignore", ".ignore", ".fdignore"} {
		data, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			continue
		}
		for _, raw := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
			trimmed := strings.TrimFunc(raw, isECMAScriptWhitespace)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, `\#`) {
				continue
			}
			line := trimResourceIgnoreTrailingSpaces(raw)
			escapedNegation := strings.HasPrefix(line, `\!`)
			if strings.HasPrefix(line, `\#`) || escapedNegation {
				line = line[1:]
			}
			rule := resourceIgnoreRule{base: root, negate: !escapedNegation && strings.HasPrefix(line, "!")}
			if rule.negate {
				line = line[1:]
			}
			rule.directory = strings.HasSuffix(line, "/")
			line = strings.TrimSuffix(strings.TrimPrefix(line, "/"), "/")
			if line == "" {
				continue
			}
			rule.pattern = prefix + line
			rule.basename = prefix == "" && !strings.Contains(line, "/")
			rule.match = compileResourceGlob(rule.pattern)
			m.rules = append(m.rules, rule)
		}
	}
}

func (m *resourceIgnoreMatcher) ignored(path string, directory bool, root string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	relative = filepath.ToSlash(relative)
	ignored := false
	for _, rule := range m.rules {
		if rule.directory && !directory {
			continue
		}
		candidate := relative
		if rule.basename {
			candidate = filepath.Base(relative)
		}
		if rule.match(candidate) {
			ignored = !rule.negate
		}
	}
	return ignored
}

func compileResourceGlob(pattern string) func(string) bool {
	var expression strings.Builder
	expression.WriteByte('^')
	runes := []rune(pattern)
	for index := 0; index < len(runes); index++ {
		switch runes[index] {
		case '*':
			if index+1 < len(runes) && runes[index+1] == '*' {
				index++
				if index+1 < len(runes) && runes[index+1] == '/' {
					index++
					expression.WriteString("(?:.*/)?")
				} else {
					expression.WriteString(".*")
				}
			} else {
				expression.WriteString("[^/]*")
			}
		case '?':
			expression.WriteString("[^/]")
		case '[':
			end := index + 1
			for end < len(runes) && runes[end] != ']' {
				if runes[end] == '\\' && end+1 < len(runes) {
					end++
				}
				end++
			}
			if end == len(runes) || end == index+1 {
				return func(string) bool { return false }
			}
			class := string(runes[index+1 : end])
			if strings.HasPrefix(class, "!") || strings.HasPrefix(class, "^") {
				class = "^" + class[1:]
			}
			expression.WriteByte('[')
			expression.WriteString(class)
			expression.WriteByte(']')
			index = end
		case '\\':
			if index+1 < len(runes) {
				index++
				expression.WriteString(regexp.QuoteMeta(string(runes[index])))
			}
		default:
			expression.WriteString(regexp.QuoteMeta(string(runes[index])))
		}
	}
	expression.WriteByte('$')
	compiled, err := regexp.Compile(expression.String())
	if err != nil {
		return func(string) bool { return false }
	}
	return compiled.MatchString
}

func trimResourceIgnoreTrailingSpaces(line string) string {
	for len(line) > 0 && line[len(line)-1] == ' ' {
		backslashes := 0
		for index := len(line) - 2; index >= 0 && line[index] == '\\'; index-- {
			backslashes++
		}
		if backslashes%2 == 1 {
			break
		}
		line = line[:len(line)-1]
	}
	return line
}

func check(ctx context.Context) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	return nil
}
func safeRead(ctx context.Context, path string, max int64, anchor string) (string, error) {
	value, _, err := readResourceFile(ctx, path, max)
	return value, err
}

func readResourceFile(ctx context.Context, path string, max int64) (string, bool, error) {
	if err := check(ctx); err != nil {
		return "", false, err
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, nil
	}
	if !info.Mode().IsRegular() {
		return "", false, nil
	}
	if max > 0 && info.Size() > max {
		return "", true, fmt.Errorf("%w: %s", ErrTooLarge, filepath.Base(path))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, nil
	}
	if max > 0 && int64(len(data)) > max {
		return "", true, fmt.Errorf("%w: %s", ErrTooLarge, filepath.Base(path))
	}
	return strings.ToValidUTF8(string(data), "�"), true, nil
}
func optionalFile(ctx context.Context, path string, max int64, anchor string) (string, error) {
	return safeRead(ctx, path, max, anchor)
}
func directory(ctx context.Context, path string, anchor string) ([]os.DirEntry, error) {
	if err := check(ctx); err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, nil
	}
	if !info.IsDir() {
		return nil, nil
	}
	if afterDirectoryLstat != nil {
		afterDirectoryLstat(path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.IsDir() {
		return nil, nil
	}
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}
func loadInstructions(ctx context.Context, dir string, scope Scope, max int64, anchor string) ([]Instruction, error) {
	for _, name := range []string{"AGENTS.md", "AGENTS.MD", "CLAUDE.md", "CLAUDE.MD"} {
		path := filepath.Join(dir, name)
		value, exists, err := readResourceFile(ctx, path, max)
		if err != nil {
			return nil, err
		}
		if exists {
			source := defaultResourceSource(dir, scope)
			source.Path = path
			return []Instruction{{Source: source, Content: value}}, nil
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
func loadTemplates(ctx context.Context, dir string, source Source, max int64, anchor string) ([]Template, []Diagnostic, error) {
	entries, err := directory(ctx, dir, anchor)
	if err != nil {
		return nil, nil, err
	}
	var out []Template
	var diagnostics []Diagnostic
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		value, found := loadTemplateFile(ctx, path, source, max)
		diagnostics = append(diagnostics, found...)
		if value.Name != "" {
			out = append(out, value)
		}
	}
	return out, diagnostics, nil
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
