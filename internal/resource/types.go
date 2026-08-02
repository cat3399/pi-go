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
	ErrUnavailable   = errors.New("resource snapshot unavailable")
)

const (
	DefaultMaxFileBytes   int64 = 128 << 10
	DefaultMaxPromptBytes int64 = 512 << 10
)

type Scope string

const (
	ScopeGlobal  Scope = "global"
	ScopeProject Scope = "project"
)

type Source struct {
	Path  string
	Scope Scope
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
type Diagnostic struct{ Kind, Resource, Name, WinnerPath, LoserPath string }

// Snapshot has no mutable maps or slices exposed by Service. Callers receive
// a copy, so a completed reload cannot be changed by a later caller.
type Snapshot struct {
	Trusted      bool
	SystemPrompt string
	AppendSystem []string
	Instructions []Instruction
	Templates    []Template
	Skills       []Skill
	Diagnostics  []Diagnostic
}

func (s Snapshot) clone() Snapshot {
	s.AppendSystem = append([]string(nil), s.AppendSystem...)
	s.Instructions = append([]Instruction(nil), s.Instructions...)
	s.Templates = append([]Template(nil), s.Templates...)
	s.Skills = append([]Skill(nil), s.Skills...)
	s.Diagnostics = append([]Diagnostic(nil), s.Diagnostics...)
	return s
}

type Tool struct{ Name, Snippet string }
type Config struct {
	CWD, AgentDir                string
	Tools                        []Tool
	MaxFileBytes, MaxPromptBytes int64
}

func validateConfig(c Config) (Config, error) {
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
	if c.MaxFileBytes == 0 {
		c.MaxFileBytes = DefaultMaxFileBytes
	}
	if c.MaxPromptBytes == 0 {
		c.MaxPromptBytes = DefaultMaxPromptBytes
	}
	if c.MaxFileBytes < 1 || c.MaxPromptBytes < c.MaxFileBytes {
		return Config{}, fmt.Errorf("%w: size limits are invalid", ErrInvalidConfig)
	}
	for _, tool := range c.Tools {
		if tool.Name == "" || !utf8.ValidString(tool.Name) || !utf8.ValidString(tool.Snippet) {
			return Config{}, fmt.Errorf("%w: tool declaration is invalid", ErrInvalidConfig)
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
	name, rest, found := strings.Cut(strings.TrimPrefix(text, "/"), " ")
	if !found {
		rest = ""
	}
	for _, template := range templates {
		if template.Name == name {
			return substitute(template.Content, parseArgs(rest))
		}
	}
	return text
}
