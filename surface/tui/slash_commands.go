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
	matches        []slashCommand
	selected       int
	value          string
	dismissedValue string
}

func builtinSlashCommands() []slashCommand {
	return []slashCommand{
		{name: "help", description: "Show commands and keyboard shortcuts", source: "builtin"},
		{name: "new", description: "Start a new session", source: "builtin"},
		{name: "resume", description: "Open another session", argumentHint: "<session-id>", source: "builtin"},
		{name: "model", description: "Switch the active model", argumentHint: "<provider/model>", source: "builtin"},
		{name: "thinking", description: "Set the reasoning level", argumentHint: "<level>", source: "builtin"},
		{name: "compact", description: "Compact the current context", argumentHint: "[instructions]", source: "builtin"},
		{name: "abort", description: "Abort the active operation", source: "builtin"},
		{name: "clear-queue", description: "Clear queued steering and follow-up messages", source: "builtin"},
		{name: "reload", description: "Reload resources and dynamic configuration", source: "builtin"},
		{name: "name", description: "Set the session display name", argumentHint: "<name>", source: "builtin"},
		{name: "stats", description: "Show session statistics", source: "builtin"},
		{name: "copy", description: "Copy the last assistant reply", source: "builtin"},
		{name: "tools", description: "Show available tool state", source: "builtin"},
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
			name: name, description: strings.TrimSpace(command.Description), source: string(command.Source),
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
	if value != p.dismissedValue {
		p.dismissedValue = ""
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
	p.dismissedValue = value
	p.value = value
	p.matches = nil
	p.selected = 0
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
		if command.argumentHint != "" {
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
