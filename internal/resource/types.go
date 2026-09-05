// Package resource owns trusted local prompt assets. It deliberately has no
// extension runtime or remote loader: a resource can influence a model prompt,
// so discovering it is part of the product security boundary.
package resource

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

var (
	ErrInvalidConfig = errors.New("invalid resource configuration")
	ErrMalformed     = errors.New("malformed resource")
	ErrTooLarge      = errors.New("resource exceeds size limit")
	ErrUnsafePath    = errors.New("unsafe resource path")
	ErrTrustStore    = errors.New("project trust store error")
	// ErrCommitUnknown means a replacement was renamed but directory durability
	// could not be confirmed. Callers must reopen/reconcile rather than assume
	// either the old or new decision survived a crash.
	ErrCommitUnknown = errors.New("trust decision commit durability is unknown")
	ErrUnavailable   = errors.New("resource snapshot unavailable")
	ErrStaleReload   = errors.New("resource reload superseded")
)

const (
	DefaultMaxFileBytes   int64 = 128 << 10
	DefaultMaxPromptBytes int64 = 512 << 10
)

type Scope string

const (
	ScopeUser      Scope = "user"
	ScopeProject   Scope = "project"
	ScopeTemporary Scope = "temporary"
	// ScopeGlobal is retained as a source-compatible alias. Pi calls this
	// persisted user scope rather than global scope.
	ScopeGlobal = ScopeUser
)

type Origin string

const (
	OriginPackage  Origin = "package"
	OriginTopLevel Origin = "top-level"
)

type Source struct {
	Path    string
	Source  string
	Scope   Scope
	Origin  Origin
	BaseDir string
}

type Instruction struct {
	Source
	Content string
}
type Template struct {
	Source
	Name, Description, ArgumentHint, Content string
}
type Skill struct {
	Source
	Name, Description, BaseDir string
	DisableModelInvocation     bool
}
type Diagnostic struct {
	Kind, Resource, Name, WinnerPath, LoserPath string
	Message, Path                               string
	Source                                      Source
	WinnerSource, LoserSource                   Source
}

// Snapshot has no mutable maps or slices exposed by Service. Callers receive
// a copy, so a completed reload cannot be changed by a later caller.
type Snapshot struct {
	Trusted          bool
	BaseSystemPrompt string
	SystemPrompt     string
	AppendSystem     []string
	Instructions     []Instruction
	Templates        []Template
	Skills           []Skill
	Diagnostics      []Diagnostic
}

func (s Snapshot) clone() Snapshot {
	s.AppendSystem = append([]string(nil), s.AppendSystem...)
	s.Instructions = append([]Instruction(nil), s.Instructions...)
	s.Templates = append([]Template(nil), s.Templates...)
	s.Skills = append([]Skill(nil), s.Skills...)
	s.Diagnostics = append([]Diagnostic(nil), s.Diagnostics...)
	return s
}

type Tool struct {
	Name             string
	Snippet          string
	PromptGuidelines []string
}
type Config struct {
	CWD, AgentDir string
	Tools         []Tool
	// A nil SelectedTools uses pi's default read/bash/edit/write set. An
	// explicitly empty slice advertises no tools in the system prompt.
	SelectedTools []string
	// SkillSources and PromptSources carry provenance from an upstream package
	// resolver. Their order is retained. SkillPaths and PromptPaths are CLI-like
	// additional paths and are appended after defaults/resolved sources.
	SkillSources, PromptSources                 []Source
	SkillPaths, PromptPaths                     []string
	NoContextFiles, NoSkills, NoPromptTemplates bool
	SystemPromptSource                          string
	AppendSystemPromptSources                   []string
	ReadmePath, DocsPath                        string
	HomeDir                                     string
	MaxFileBytes, MaxPromptBytes                int64
}

func cloneBuildSystemPromptOptions(options BuildSystemPromptOptions) BuildSystemPromptOptions {
	options.CustomPrompt = cloneString(options.CustomPrompt)
	if options.SelectedTools != nil {
		options.SelectedTools = append([]string{}, options.SelectedTools...)
	}
	if options.ToolSnippets != nil {
		cloned := make(map[string]string, len(options.ToolSnippets))
		for name, snippet := range options.ToolSnippets {
			cloned[name] = snippet
		}
		options.ToolSnippets = cloned
	}
	if options.PromptGuidelines != nil {
		options.PromptGuidelines = append([]string{}, options.PromptGuidelines...)
	}
	if options.ContextFiles != nil {
		options.ContextFiles = append([]Instruction{}, options.ContextFiles...)
	}
	if options.Skills != nil {
		options.Skills = append([]Skill{}, options.Skills...)
	}
	return options
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func validateConfig(c Config) (Config, error) {
	if c.Tools != nil {
		tools := make([]Tool, len(c.Tools))
		copy(tools, c.Tools)
		c.Tools = tools
	}
	for index := range c.Tools {
		if c.Tools[index].PromptGuidelines != nil {
			c.Tools[index].PromptGuidelines = append([]string{}, c.Tools[index].PromptGuidelines...)
		}
	}
	if c.SelectedTools != nil {
		c.SelectedTools = append([]string{}, c.SelectedTools...)
	}
	c.SkillPaths = append([]string(nil), c.SkillPaths...)
	c.PromptPaths = append([]string(nil), c.PromptPaths...)
	c.SkillSources = append([]Source(nil), c.SkillSources...)
	c.PromptSources = append([]Source(nil), c.PromptSources...)
	if c.AppendSystemPromptSources != nil {
		c.AppendSystemPromptSources = append([]string{}, c.AppendSystemPromptSources...)
	}
	for _, item := range []struct{ label, value string }{{"cwd", c.CWD}, {"agent directory", c.AgentDir}} {
		if item.value == "" || !utf8.ValidString(item.value) || strings.IndexByte(item.value, 0) >= 0 {
			return Config{}, fmt.Errorf("%w: %s is invalid", ErrInvalidConfig, item.label)
		}
		absolute, err := filepath.Abs(item.value)
		if err != nil {
			return Config{}, fmt.Errorf("%w: resolve %s: %w", ErrInvalidConfig, item.label, err)
		}
		if item.label == "cwd" {
			c.CWD = filepath.Clean(absolute)
		} else {
			c.AgentDir = filepath.Clean(absolute)
		}
	}
	for _, item := range []struct{ label, value string }{
		{"home directory", c.HomeDir}, {"readme path", c.ReadmePath}, {"docs path", c.DocsPath},
	} {
		if item.value != "" && (!utf8.ValidString(item.value) || strings.IndexByte(item.value, 0) >= 0) {
			return Config{}, fmt.Errorf("%w: %s is invalid", ErrInvalidConfig, item.label)
		}
	}
	if c.MaxFileBytes < 0 || c.MaxPromptBytes < 0 || c.MaxPromptBytes > 0 && c.MaxFileBytes > c.MaxPromptBytes {
		return Config{}, fmt.Errorf("%w: size limits are invalid", ErrInvalidConfig)
	}
	for _, tool := range c.Tools {
		if tool.Name == "" || !utf8.ValidString(tool.Name) || !utf8.ValidString(tool.Snippet) {
			return Config{}, fmt.Errorf("%w: tool declaration is invalid", ErrInvalidConfig)
		}
		for _, guideline := range tool.PromptGuidelines {
			if !utf8.ValidString(guideline) {
				return Config{}, fmt.Errorf("%w: tool guideline is invalid", ErrInvalidConfig)
			}
		}
	}
	for _, name := range c.SelectedTools {
		if name == "" || !utf8.ValidString(name) {
			return Config{}, fmt.Errorf("%w: selected tool is invalid", ErrInvalidConfig)
		}
	}
	for _, item := range append(append([]string(nil), c.SkillPaths...), c.PromptPaths...) {
		if !utf8.ValidString(item) || strings.IndexByte(item, 0) >= 0 {
			return Config{}, fmt.Errorf("%w: additional resource path is invalid", ErrInvalidConfig)
		}
	}
	for _, source := range append(append([]Source(nil), c.SkillSources...), c.PromptSources...) {
		if source.Path == "" || !utf8.ValidString(source.Path) || strings.IndexByte(source.Path, 0) >= 0 ||
			!utf8.ValidString(source.Source) || strings.IndexByte(source.Source, 0) >= 0 ||
			!utf8.ValidString(source.BaseDir) || strings.IndexByte(source.BaseDir, 0) >= 0 {
			return Config{}, fmt.Errorf("%w: resource source is invalid", ErrInvalidConfig)
		}
		if source.Scope != "" && source.Scope != ScopeUser && source.Scope != ScopeProject && source.Scope != ScopeTemporary {
			return Config{}, fmt.Errorf("%w: resource source scope is invalid", ErrInvalidConfig)
		}
		if source.Origin != "" && source.Origin != OriginPackage && source.Origin != OriginTopLevel {
			return Config{}, fmt.Errorf("%w: resource source origin is invalid", ErrInvalidConfig)
		}
	}
	return c, nil
}

// ExpandTemplate applies the upstream positional grammar without recursive
// expansion; arguments are data, never template syntax.
func ExpandTemplate(text string, templates []Template) string {
	if !strings.HasPrefix(text, "/") {
		return text
	}
	command := strings.TrimPrefix(text, "/")
	name := command
	rest := ""
	for offset, r := range command {
		if isECMAScriptWhitespace(r) {
			name = command[:offset]
			rest = strings.TrimLeftFunc(command[offset:], isECMAScriptWhitespace)
			break
		}
	}
	if name == "" {
		return text
	}
	for _, template := range templates {
		if template.Name == name {
			return substitute(template.Content, parseArgs(rest))
		}
	}
	return text
}
