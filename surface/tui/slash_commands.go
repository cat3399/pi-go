package tui

import (
	"sort"
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/cat3399/pi-go/internal/application"
)

type slashCommand struct {
	name         string
	description  string
	argumentHint string
	source       string
}

type slashPaletteModel struct {
	commands       []slashCommand
	arguments      map[string][]slashCommand
	matches        []slashCommand
	selected       int
	value          string
	dismissedValue string
	argumentMode   bool
	argumentPrefix string
}

func builtinSlashCommands() []slashCommand {
	return []slashCommand{
		{name: "help", description: "Show commands and keyboard shortcuts", source: "builtin"},
		{name: "new", description: "Start a new session", source: "builtin"},
		{name: "resume", description: "Open the session selector", argumentHint: "[session-id]", source: "builtin"},
		{name: "tree", description: "Navigate the current session tree", source: "builtin"},
		{name: "fork", description: "Fork from an earlier user message", source: "builtin"},
		{name: "clone", description: "Clone the session at its current position", source: "builtin"},
		{name: "model", description: "Open the model selector", argumentHint: "[provider/model or search]", source: "builtin"},
		{name: "thinking", description: "Choose the reasoning level", argumentHint: "[level]", source: "builtin"},
		{name: "settings", description: "Configure runtime and display settings", source: "builtin"},
		{name: "export", description: "Export this session as HTML or JSONL", argumentHint: "[path]", source: "builtin"},
		{name: "import", description: "Replace this session from a JSONL file", argumentHint: "<path.jsonl>", source: "builtin"},
		{name: "trust", description: "Trust project-local resources in this working directory", source: "builtin"},
		{name: "login", description: "Authenticate a model provider", argumentHint: "[provider]", source: "builtin"},
		{name: "logout", description: "Remove a stored provider credential", argumentHint: "[provider]", source: "builtin"},
		{name: "compact", description: "Compact the current context", argumentHint: "[instructions]", source: "builtin"},
		{name: "abort", description: "Abort the active operation", source: "builtin"},
		{name: "clear-queue", description: "Clear queued steering and follow-up messages", source: "builtin"},
		{name: "reload", description: "Reload resources and dynamic configuration", source: "builtin"},
		{name: "name", description: "Set the session display name", argumentHint: "<name>", source: "builtin"},
		{name: "stats", description: "Show session statistics", source: "builtin"},
		{name: "copy", description: "Copy the last assistant reply", source: "builtin"},
		{name: "tools", description: "Configure available tools", source: "builtin"},
		{name: "quit", description: "Exit pi-go", source: "builtin"},
	}
}

func mergeSlashCommands(dynamic []application.SlashCommandInfo) []slashCommand {
	commands := builtinSlashCommands()
	seen := make(map[string]struct{}, len(commands)+len(dynamic))
	for _, command := range commands {
		seen[command.name] = struct{}{}
	}
	for _, command := range dynamic {
		name := strings.TrimSpace(command.Name)
		if name == "" || strings.IndexFunc(name, unicode.IsSpace) >= 0 {
			continue
		}
		key := strings.ToLower(name)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		commands = append(commands, slashCommand{
			name: name, description: strings.TrimSpace(command.Description),
			argumentHint: strings.TrimSpace(command.ArgumentHint), source: string(command.Source),
		})
	}
	return commands
}

func (p *slashPaletteModel) SetCommands(commands []slashCommand) {
	if p == nil {
		return
	}
	p.commands = append([]slashCommand(nil), commands...)
	p.Update(p.value)
}

func (p *slashPaletteModel) SetArgumentCompletions(arguments map[string][]slashCommand) {
	if p == nil {
		return
	}
	p.arguments = make(map[string][]slashCommand, len(arguments))
	for name, values := range arguments {
		p.arguments[strings.ToLower(strings.TrimSpace(name))] = append([]slashCommand(nil), values...)
	}
}

func (p *slashPaletteModel) Update(value string) {
	if p == nil {
		return
	}
	previous := ""
	if p.selected >= 0 && p.selected < len(p.matches) {
		previous = p.matches[p.selected].name
	}
	p.value = value
	p.matches = nil
	p.selected = 0
	p.argumentMode = false
	p.argumentPrefix = ""
	if value != p.dismissedValue {
		p.dismissedValue = ""
	}
	if command, query, prefix, argumentMode := slashArgumentQuery(value); argumentMode {
		candidates := p.arguments[command]
		if len(candidates) == 0 || p.dismissedValue == value {
			return
		}
		p.argumentMode = true
		p.argumentPrefix = prefix
		query = strings.ToLower(strings.TrimSpace(query))
		for _, candidate := range candidates {
			if query == "" || slashCommandRank(candidate, query) < 3 ||
				strings.Contains(strings.ToLower(candidate.description), query) {
				p.matches = append(p.matches, candidate)
			}
		}
		sort.SliceStable(p.matches, func(left, right int) bool {
			leftRank := slashCommandRank(p.matches[left], query)
			rightRank := slashCommandRank(p.matches[right], query)
			if leftRank != rightRank {
				return leftRank < rightRank
			}
			return strings.ToLower(p.matches[left].name) < strings.ToLower(p.matches[right].name)
		})
		return
	}
	query, ok := slashCommandQuery(value)
	if !ok || p.dismissedValue == value {
		return
	}
	query = strings.ToLower(query)
	for _, command := range p.commands {
		name := strings.ToLower(command.name)
		description := strings.ToLower(command.description)
		if strings.Contains(name, query) || strings.Contains(description, query) {
			p.matches = append(p.matches, command)
		}
	}
	sort.SliceStable(p.matches, func(left, right int) bool {
		leftRank := slashCommandRank(p.matches[left], query)
		rightRank := slashCommandRank(p.matches[right], query)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		if sourceRank(p.matches[left].source) != sourceRank(p.matches[right].source) {
			return sourceRank(p.matches[left].source) < sourceRank(p.matches[right].source)
		}
		return strings.ToLower(p.matches[left].name) < strings.ToLower(p.matches[right].name)
	})
	for index, command := range p.matches {
		if command.name == previous {
			p.selected = index
			break
		}
	}
}

func slashArgumentQuery(value string) (command, query, prefix string, ok bool) {
	if !strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\r\n") {
		return "", "", "", false
	}
	space := strings.IndexFunc(value, unicode.IsSpace)
	if space <= 1 {
		return "", "", "", false
	}
	command = strings.ToLower(strings.TrimSpace(value[1:space]))
	if command == "" {
		return "", "", "", false
	}
	argumentStart := space
	for argumentStart < len(value) && unicode.IsSpace(rune(value[argumentStart])) {
		argumentStart++
	}
	prefix = value[:argumentStart]
	if prefix == value {
		prefix = value
	}
	query = value[argumentStart:]
	return command, query, prefix, true
}

func slashCommandQuery(value string) (string, bool) {
	if !strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\r\n") {
		return "", false
	}
	query := strings.TrimPrefix(value, "/")
	if strings.IndexFunc(query, unicode.IsSpace) >= 0 {
		return "", false
	}
	return query, true
}

func slashCommandRank(command slashCommand, query string) int {
	name := strings.ToLower(command.name)
	switch {
	case name == query:
		return 0
	case strings.HasPrefix(name, query):
		return 1
	case strings.Contains(name, query):
		return 2
	default:
		return 3
	}
}

func sourceRank(source string) int {
	switch source {
	case "builtin":
		return 0
	case string(application.CommandSourceExtension):
		return 1
	case string(application.CommandSourcePrompt):
		return 2
	case string(application.CommandSourceSkill):
		return 3
	default:
		return 4
	}
}

func (p *slashPaletteModel) Visible() bool { return p != nil && len(p.matches) != 0 }

func (p *slashPaletteModel) Move(delta int) {
	if p == nil || len(p.matches) == 0 {
		return
	}
	p.selected = (p.selected + delta + len(p.matches)) % len(p.matches)
}

func (p *slashPaletteModel) Accept() (string, bool) {
	if p == nil || p.selected < 0 || p.selected >= len(p.matches) {
		return "", false
	}
	value := "/" + p.matches[p.selected].name + " "
	if p.argumentMode {
		value = p.argumentPrefix + p.matches[p.selected].name
	}
	p.dismissedValue = value
	p.value = value
	p.matches = nil
	p.selected = 0
	p.argumentMode = false
	p.argumentPrefix = ""
	return value, true
}

func (p *slashPaletteModel) Dismiss() {
	if p == nil {
		return
	}
	p.dismissedValue = p.value
	p.matches = nil
	p.selected = 0
}

func (p *slashPaletteModel) Hide(value string) {
	if p == nil {
		return
	}
	p.value = value
	p.matches = nil
	p.selected = 0
}

func (m *Model) renderSlashPalette(width, maxLines int) []string {
	if maxLines <= 0 || !m.slashPalette.Visible() {
		return nil
	}
	count := min(maxLines, len(m.slashPalette.matches))
	start := 0
	if m.slashPalette.selected >= count {
		start = m.slashPalette.selected - count + 1
	}
	lines := make([]string, 0, count)
	for index := start; index < start+count; index++ {
		command := m.slashPalette.matches[index]
		invocation := "/" + sanitizeDisplayText(command.name)
		if m.slashPalette.argumentMode {
			invocation = sanitizeDisplayText(command.name)
		} else if command.argumentHint != "" {
			invocation += " " + sanitizeDisplayText(command.argumentHint)
		}
		description := sanitizeDisplayText(command.description)
		if command.source != "" && command.source != "builtin" {
			description = "[" + sanitizeDisplayText(command.source) + "] " + description
		}
		line := "  " + invocation
		if description != "" {
			line += " — " + description
		}
		style := m.theme.subtleStyle()
		if index == m.slashPalette.selected {
			line = "› " + strings.TrimPrefix(line, "  ")
			style = lipgloss.NewStyle().Bold(true).Foreground(m.theme.color(m.theme.Primary))
		}
		lines = append(lines, Truncate(style.Render(line), width, "…", false))
	}
	return lines
}
