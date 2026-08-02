package resource

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type scalarKind uint8

const (
	scalarPlain scalarKind = iota
	scalarQuoted
)

// frontmatter is deliberately a small strict subset. Resource metadata is
// declarative; accepting YAML tags, aliases, or arbitrary nesting would add a
// second interpreter to the trust boundary. Quoted scalars and folded lines
// cover the upstream skill/template metadata used by this milestone.
func frontmatter(raw string) (map[string]string, string, error) {
	values, body, _, err := frontmatterDetailed(raw)
	return values, body, err
}

func frontmatterDetailed(raw string) (map[string]string, string, map[string]scalarKind, error) {
	if !strings.HasPrefix(raw, "---\n") && !strings.HasPrefix(raw, "---\r\n") {
		return map[string]string{}, raw, map[string]scalarKind{}, nil
	}
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	end := -1
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, "", nil, fmt.Errorf("unterminated frontmatter")
	}
	out := map[string]string{}
	kinds := map[string]scalarKind{}
	for _, line := range lines[1:end] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, "", nil, fmt.Errorf("invalid frontmatter")
		}
		key = strings.TrimSpace(key)
		if _, exists := out[key]; exists {
			return nil, "", nil, fmt.Errorf("duplicate frontmatter field")
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"')) {
			kinds[key] = scalarQuoted
			value = value[1 : len(value)-1]
		}
		if value == "|" || value == ">" {
			return nil, "", nil, fmt.Errorf("multiline frontmatter is unsupported")
		}
		out[key] = value
	}
	return out, strings.Join(lines[end+1:], "\n"), kinds, nil
}

func assemble(c Config, snapshot Snapshot) (string, error) {
	var out strings.Builder
	if snapshot.SystemPrompt != "" {
		out.WriteString(snapshot.SystemPrompt)
	} else {
		out.WriteString("You are pi-go, a coding agent. Work carefully and report material failures.")
	}
	if len(c.Tools) > 0 {
		out.WriteString("\n\n<available_tools>\n")
		tools := append([]Tool(nil), c.Tools...)
		for i := 0; i < len(tools); i++ {
			for j := i + 1; j < len(tools); j++ {
				if tools[j].Name < tools[i].Name {
					tools[i], tools[j] = tools[j], tools[i]
				}
			}
		}
		for _, tool := range tools {
			out.WriteString("- ")
			out.WriteString(escape(tool.Name))
			if tool.Snippet != "" {
				out.WriteString(": ")
				out.WriteString(escape(tool.Snippet))
			}
			out.WriteByte('\n')
		}
		out.WriteString("</available_tools>")
	}
	for _, appendix := range snapshot.AppendSystem {
		out.WriteString("\n\n")
		out.WriteString(appendix)
	}
	if len(snapshot.Instructions) > 0 {
		out.WriteString("\n\n<project_context>\n")
		for _, item := range snapshot.Instructions {
			out.WriteString("<project_instructions path=\"")
			out.WriteString(escape(item.Path))
			out.WriteString("\">\n")
			out.WriteString(item.Content)
			out.WriteString("\n</project_instructions>\n")
		}
		out.WriteString("</project_context>")
	}
	if len(snapshot.Templates) > 0 {
		out.WriteString("\n\n<available_prompt_templates>\n")
		for _, item := range snapshot.Templates {
			out.WriteString("<template name=\"")
			out.WriteString(escape(item.Name))
			out.WriteString("\" description=\"")
			out.WriteString(escape(item.Description))
			out.WriteString("\" location=\"")
			out.WriteString(escape(item.Path))
			out.WriteString("\"/>\n")
		}
		out.WriteString("</available_prompt_templates>")
	}
	visible := false
	for _, skill := range snapshot.Skills {
		if !skill.DisableModelInvocation {
			visible = true
			break
		}
	}
	if visible {
		out.WriteString("\n\n<available_skills>\n")
		for _, skill := range snapshot.Skills {
			if skill.DisableModelInvocation {
				continue
			}
			out.WriteString("<skill name=\"")
			out.WriteString(escape(skill.Name))
			out.WriteString("\" description=\"")
			out.WriteString(escape(skill.Description))
			out.WriteString("\" location=\"")
			out.WriteString(escape(skill.Path))
			out.WriteString("\"/>\n")
		}
		out.WriteString("</available_skills>")
	}
	out.WriteString("\n\nCurrent working directory: ")
	out.WriteString(strings.ReplaceAll(c.CWD, "\\", "/"))
	if int64(out.Len()) > c.MaxPromptBytes {
		return "", fmt.Errorf("%w: assembled system prompt", ErrTooLarge)
	}
	return out.String(), nil
}
func escape(value string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;").Replace(value)
}

func parseArgs(value string) []string {
	var out []string
	var current strings.Builder
	var quote rune
	hasToken := false
	for _, r := range value {
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				current.WriteRune(r)
			}
			hasToken = true
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			hasToken = true
			continue
		}
		if isECMAScriptWhitespace(r) {
			if hasToken {
				out = append(out, current.String())
				current.Reset()
				hasToken = false
			}
			continue
		}
		current.WriteRune(r)
		hasToken = true
	}
	if hasToken {
		out = append(out, current.String())
	}
	return out
}

// isECMAScriptWhitespace implements the fixed ECMAScript WhiteSpace and
// LineTerminator sets used by upstream JavaScript regexp \s. In particular it
// includes BOM/FEFF and excludes Unicode NEL/U+0085.
func isECMAScriptWhitespace(r rune) bool {
	switch r {
	case '\u0009', '\u000a', '\u000b', '\u000c', '\u000d', '\u0020',
		'\u00a0', '\u1680', '\u2028', '\u2029', '\u202f', '\u205f',
		'\u3000', '\ufeff':
		return true
	default:
		return r >= '\u2000' && r <= '\u200a'
	}
}

var placeholder = regexp.MustCompile(`\$\{(\d+|ARGUMENTS|@):-([^}]*)\}|\$\{@:(\d+)(?::(\d+))?\}|\$(ARGUMENTS|@|\d+)`)

func substitute(content string, args []string) string {
	all := strings.Join(args, " ")
	return placeholder.ReplaceAllStringFunc(content, func(match string) string {
		parts := placeholder.FindStringSubmatch(match)
		if parts[1] != "" {
			if parts[1] == "@" || parts[1] == "ARGUMENTS" {
				if all != "" {
					return all
				}
			} else {
				n, err := strconv.Atoi(parts[1])
				if err != nil {
					return parts[2]
				}
				if n > 0 && n <= len(args) && args[n-1] != "" {
					return args[n-1]
				}
			}
			return parts[2]
		}
		if parts[3] != "" {
			start, err := strconv.Atoi(parts[3])
			if err != nil {
				return ""
			}
			if start < 1 {
				start = 1
			}
			start--
			if start >= len(args) {
				return ""
			}
			if parts[4] != "" {
				length, err := strconv.Atoi(parts[4])
				if err != nil {
					return ""
				}
				end := start + length
				if end < start { // overflow is never a valid slice.
					return ""
				}
				if end > len(args) {
					end = len(args)
				}
				return strings.Join(args[start:end], " ")
			}
			return strings.Join(args[start:], " ")
		}
		if parts[5] == "@" || parts[5] == "ARGUMENTS" {
			return all
		}
		n, err := strconv.Atoi(parts[5])
		if err != nil {
			return ""
		}
		if n > 0 && n <= len(args) {
			return args[n-1]
		}
		return ""
	})
}
