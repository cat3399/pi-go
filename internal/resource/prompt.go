package resource

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

func frontmatter(raw string) (map[string]string, string, error) {
	values, body, err := parseFrontmatter(raw)
	if err != nil {
		return nil, "", err
	}
	if err := validateKnownFrontmatterTypes(values); err != nil {
		return nil, "", err
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		if text, ok := value.(string); ok {
			out[key] = text
		}
	}
	return out, body, nil
}

func validateKnownFrontmatterTypes(values map[string]any) error {
	for _, name := range []string{"name", "description", "argument-hint"} {
		if value, exists := values[name]; exists {
			if _, ok := value.(string); !ok {
				return fmt.Errorf("frontmatter field %q must be a string", name)
			}
		}
	}
	if value, exists := values["disable-model-invocation"]; exists {
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("frontmatter field %q must be a boolean", "disable-model-invocation")
		}
	}
	return nil
}

// parseFrontmatter mirrors coding-agent's delimiter/body behavior while
// keeping metadata declarative. Scalar aliases are supported, but YAML merge
// keys are rejected rather than expanded into implicit fields.
func parseFrontmatter(raw string) (map[string]any, string, error) {
	normalized := strings.ReplaceAll(strings.ReplaceAll(raw, "\r\n", "\n"), "\r", "\n")
	if !strings.HasPrefix(normalized, "---") {
		return map[string]any{}, normalized, nil
	}
	end := strings.Index(normalized[3:], "\n---")
	if end < 0 {
		return map[string]any{}, normalized, nil
	}
	end += 3
	yamlText := normalized[4:end]
	body := strings.TrimFunc(normalized[end+4:], isECMAScriptWhitespace)
	if yamlText == "" {
		return map[string]any{}, body, nil
	}
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(yamlText), &document); err != nil {
		return nil, "", err
	}
	if len(document.Content) == 0 {
		return map[string]any{}, body, nil
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, "", fmt.Errorf("frontmatter root must be a mapping")
	}
	value, err := decodeFrontmatterMapping(root, map[*yaml.Node]bool{})
	if err != nil {
		return nil, "", err
	}
	return value, body, nil
}

func decodeFrontmatterMapping(node *yaml.Node, resolving map[*yaml.Node]bool) (map[string]any, error) {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("frontmatter mapping is invalid")
	}
	values := make(map[string]any, len(node.Content)/2)
	for index := 0; index+1 < len(node.Content); index += 2 {
		key := node.Content[index]
		if key.Kind == yaml.ScalarNode && key.Tag == "!!merge" {
			return nil, fmt.Errorf("YAML merge keys are not supported in frontmatter")
		}
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" && key.Tag != "" {
			return nil, fmt.Errorf("frontmatter keys must be strings")
		}
		if _, exists := values[key.Value]; exists {
			return nil, fmt.Errorf("duplicate frontmatter field %q", key.Value)
		}
		value, err := decodeFrontmatterValue(node.Content[index+1], resolving)
		if err != nil {
			return nil, fmt.Errorf("frontmatter field %q: %w", key.Value, err)
		}
		values[key.Value] = value
	}
	return values, nil
}

func decodeFrontmatterValue(node *yaml.Node, resolving map[*yaml.Node]bool) (any, error) {
	if node == nil {
		return nil, nil
	}
	if resolving[node] {
		return nil, fmt.Errorf("cyclic YAML alias")
	}
	switch node.Kind {
	case yaml.AliasNode:
		if node.Alias == nil || node.Alias.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("only scalar YAML aliases are supported")
		}
		resolving[node] = true
		value, err := decodeFrontmatterValue(node.Alias, resolving)
		delete(resolving, node)
		return value, err
	case yaml.ScalarNode:
		var value any
		if err := node.Decode(&value); err != nil {
			return nil, err
		}
		return value, nil
	case yaml.SequenceNode:
		values := make([]any, 0, len(node.Content))
		for _, child := range node.Content {
			value, err := decodeFrontmatterValue(child, resolving)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		return values, nil
	case yaml.MappingNode:
		return decodeFrontmatterMapping(node, resolving)
	default:
		return nil, fmt.Errorf("unsupported YAML node")
	}
}

func assemble(c Config, snapshot Snapshot) (string, error) {
	selected := c.SelectedTools
	if selected == nil && c.Tools != nil {
		selected = make([]string, 0, len(c.Tools))
		for _, candidate := range c.Tools {
			selected = append(selected, candidate.Name)
		}
	}
	snippets := make(map[string]string, len(c.Tools))
	var guidelines []string
	active := make(map[string]struct{}, len(selected))
	for _, name := range selected {
		active[name] = struct{}{}
	}
	for _, candidate := range c.Tools {
		snippets[candidate.Name] = candidate.Snippet
		if _, ok := active[candidate.Name]; ok {
			guidelines = append(guidelines, candidate.PromptGuidelines...)
		}
	}
	var custom *string
	basePrompt := snapshot.BaseSystemPrompt
	if basePrompt == "" {
		basePrompt = snapshot.SystemPrompt
	}
	if basePrompt != "" {
		value := basePrompt
		custom = &value
	}
	prompt := BuildSystemPrompt(BuildSystemPromptOptions{
		CustomPrompt: custom, SelectedTools: selected, ToolSnippets: snippets, PromptGuidelines: guidelines,
		AppendSystemPrompt: strings.Join(snapshot.AppendSystem, "\n\n"), CWD: c.CWD,
		ContextFiles: snapshot.Instructions, Skills: snapshot.Skills,
		ReadmePath: c.ReadmePath, DocsPath: c.DocsPath, ExamplesPath: c.ExamplesPath,
	})
	if c.MaxPromptBytes > 0 && int64(len(prompt)) > c.MaxPromptBytes {
		return "", fmt.Errorf("%w: assembled system prompt", ErrTooLarge)
	}
	return prompt, nil
}

type BuildSystemPromptOptions struct {
	CustomPrompt       *string
	SelectedTools      []string
	ToolSnippets       map[string]string
	PromptGuidelines   []string
	AppendSystemPrompt string
	CWD                string
	ContextFiles       []Instruction
	Skills             []Skill
	ReadmePath         string
	DocsPath           string
	ExamplesPath       string
}

// BuildSystemPrompt is a behavioral port of coding-agent's system prompt
// builder. Prompt templates deliberately are not injected into this prompt.
func BuildSystemPrompt(options BuildSystemPromptOptions) string {
	promptCWD := strings.ReplaceAll(options.CWD, "\\", "/")
	appendSection := ""
	if options.AppendSystemPrompt != "" {
		appendSection = "\n\n" + options.AppendSystemPrompt
	}
	tools := options.SelectedTools
	if tools == nil {
		tools = []string{"read", "bash", "edit", "write"}
	}
	hasRead := containsString(tools, "read")
	if options.CustomPrompt != nil && *options.CustomPrompt != "" {
		prompt := *options.CustomPrompt + appendSection + formatContextFiles(options.ContextFiles)
		if hasRead {
			prompt += formatSkillsForPrompt(options.Skills)
		}
		return prompt + "\nCurrent working directory: " + promptCWD
	}

	visibleTools := make([]string, 0, len(tools))
	for _, name := range tools {
		if snippet := options.ToolSnippets[name]; snippet != "" {
			visibleTools = append(visibleTools, "- "+name+": "+snippet)
		}
	}
	toolsList := "(none)"
	if len(visibleTools) > 0 {
		toolsList = strings.Join(visibleTools, "\n")
	}

	guidelines := make([]string, 0, len(options.PromptGuidelines)+3)
	seen := map[string]struct{}{}
	addGuideline := func(value string) {
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		guidelines = append(guidelines, value)
	}
	if containsString(tools, "bash") && !containsString(tools, "grep") && !containsString(tools, "find") && !containsString(tools, "ls") {
		addGuideline("Use bash for file operations like ls, rg, find")
	}
	for _, guideline := range options.PromptGuidelines {
		if normalized := strings.TrimFunc(guideline, isECMAScriptWhitespace); normalized != "" {
			addGuideline(normalized)
		}
	}
	addGuideline("Be concise in your responses")
	addGuideline("Show file paths clearly when working with files")
	for index := range guidelines {
		guidelines[index] = "- " + guidelines[index]
	}

	readmePath, docsPath, examplesPath := options.ReadmePath, options.DocsPath, options.ExamplesPath
	if readmePath == "" {
		readmePath = filepath.Join(defaultPiPackageRoot(), "README.md")
	}
	if docsPath == "" {
		docsPath = filepath.Join(filepath.Dir(readmePath), "docs")
	}
	if examplesPath == "" {
		examplesPath = filepath.Join(filepath.Dir(readmePath), "examples")
	}
	prompt := fmt.Sprintf(`You are an expert coding assistant operating inside pi, a coding agent harness. You help users by reading files, executing commands, editing code, and writing new files.

Available tools:
%s

In addition to the tools above, you may have access to other custom tools depending on the project.

Guidelines:
%s

Pi documentation (read only when the user asks about pi itself, its SDK, extensions, themes, skills, or TUI):
- Main documentation: %s
- Additional docs: %s
- Examples: %s (extensions, custom tools, SDK)
- When reading pi docs or examples, resolve docs/... under Additional docs and examples/... under Examples, not the current working directory
- When asked about: extensions (docs/extensions.md, examples/extensions/), themes (docs/themes.md), skills (docs/skills.md), prompt templates (docs/prompt-templates.md), TUI components (docs/tui.md), keybindings (docs/keybindings.md), SDK integrations (docs/sdk.md), custom providers (docs/custom-provider.md), adding models (docs/models.md), pi packages (docs/packages.md), environment variables (docs/environment-variables.md)
- When working on pi topics, read the docs and examples, and follow .md cross-references before implementing
- Always read pi .md files completely and follow links to related docs (e.g., tui.md for TUI API details)`, toolsList, strings.Join(guidelines, "\n"), readmePath, docsPath, examplesPath)
	prompt += appendSection + formatContextFiles(options.ContextFiles)
	if hasRead {
		prompt += formatSkillsForPrompt(options.Skills)
	}
	return prompt + "\nCurrent working directory: " + promptCWD
}

func defaultPiPackageRoot() string {
	if _, source, _, ok := runtime.Caller(0); ok {
		root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
		if _, err := os.Stat(filepath.Join(root, "README.md")); err == nil {
			return root
		}
	}
	if executable, err := os.Executable(); err == nil {
		return filepath.Dir(executable)
	}
	return "."
}

func formatContextFiles(files []Instruction) string {
	if len(files) == 0 {
		return ""
	}
	var out strings.Builder
	out.WriteString("\n\n<project_context>\n\nProject-specific instructions and guidelines:\n\n")
	for _, file := range files {
		out.WriteString(`<project_instructions path="`)
		out.WriteString(file.Path)
		out.WriteString("\">\n")
		out.WriteString(file.Content)
		out.WriteString("\n</project_instructions>\n\n")
	}
	out.WriteString("</project_context>\n")
	return out.String()
}

func formatSkillsForPrompt(skills []Skill) string {
	visible := make([]Skill, 0, len(skills))
	for _, skill := range skills {
		if !skill.DisableModelInvocation {
			visible = append(visible, skill)
		}
	}
	if len(visible) == 0 {
		return ""
	}
	lines := []string{
		"\n\nThe following skills provide specialized instructions for specific tasks.",
		"Use the read tool to load a skill's file when the task matches its description.",
		"When a skill file references a relative path, resolve it against the skill directory (parent of SKILL.md / dirname of the path) and use that absolute path in tool commands.",
		"", "<available_skills>",
	}
	for _, skill := range visible {
		lines = append(lines, "  <skill>", "    <name>"+escape(skill.Name)+"</name>", "    <description>"+escape(skill.Description)+"</description>", "    <location>"+escape(skill.Path)+"</location>", "  </skill>")
	}
	return strings.Join(append(lines, "</available_skills>"), "\n")
}

// ExpandSkillCommand ports AgentSession._expandSkillCommand. Skill files are
// read at invocation time so an edited SKILL.md takes effect without a resource
// reload, exactly as in pi. Unknown or unreadable skills pass through unchanged.
func ExpandSkillCommand(text string, skills []Skill) string {
	if !strings.HasPrefix(text, "/skill:") {
		return text
	}
	spaceIndex := strings.IndexByte(text, ' ')
	name := ""
	arguments := ""
	if spaceIndex < 0 {
		name = text[7:]
	} else {
		name = text[7:spaceIndex]
		arguments = strings.TrimFunc(text[spaceIndex+1:], isECMAScriptWhitespace)
	}
	var selected *Skill
	for index := range skills {
		if skills[index].Name == name {
			selected = &skills[index]
			break
		}
	}
	if selected == nil {
		return text
	}
	data, err := os.ReadFile(selected.Path)
	if err != nil {
		return text
	}
	_, body, err := parseFrontmatter(strings.ToValidUTF8(string(data), "�"))
	if err != nil {
		return text
	}
	body = strings.TrimFunc(body, isECMAScriptWhitespace)
	skillBlock := fmt.Sprintf(
		"<skill name=\"%s\" location=\"%s\">\nReferences are relative to %s.\n\n%s\n</skill>",
		selected.Name, selected.Path, selected.BaseDir, body,
	)
	if arguments == "" {
		return skillBlock
	}
	return skillBlock + "\n\n" + arguments
}

// ExpandPromptInput preserves pi's expansion order: explicit skill commands
// are expanded first, then ordinary prompt templates are considered.
func ExpandPromptInput(text string, snapshot Snapshot) string {
	return ExpandTemplate(ExpandSkillCommand(text, snapshot.Skills), snapshot.Templates)
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
func escape(value string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;").Replace(value)
}

func parseArgs(value string) []string {
	var out []string
	var current strings.Builder
	var quote rune
	for _, r := range value {
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				current.WriteRune(r)
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			continue
		}
		if isECMAScriptWhitespace(r) {
			if current.Len() > 0 {
				out = append(out, current.String())
				current.Reset()
			}
			continue
		}
		current.WriteRune(r)
	}
	if current.Len() > 0 {
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
