package tui

import (
	"fmt"

	"charm.land/bubbletea/v2"
	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/application"
)

const (
	settingAutoCompaction  = "auto-compaction"
	settingAutoRetry       = "auto-retry"
	settingSteeringMode    = "steering-mode"
	settingFollowUpMode    = "follow-up-mode"
	settingThinkingLevel   = "thinking-level"
	settingThinkingVisible = "thinking-visible"
)

func (m *Model) settingsSelectorItems() []selectorItem {
	return []selectorItem{
		{
			Key: settingAutoCompaction, Title: "Automatic compaction",
			Badge:       enabledLabel(m.state.AutoCompactionEnabled),
			Description: "Compact context automatically when it approaches the model limit",
		},
		{
			Key: settingAutoRetry, Title: "Automatic retry",
			Badge:       enabledLabel(m.state.AutoRetryEnabled),
			Description: "Retry transient provider failures using the configured retry policy",
		},
		{
			Key: settingSteeringMode, Title: "Steering queue mode",
			Badge:       queueModeLabel(m.state.SteeringMode),
			Description: "Choose whether a drain point consumes one or all steering messages",
		},
		{
			Key: settingFollowUpMode, Title: "Follow-up queue mode",
			Badge:       queueModeLabel(m.state.FollowUpMode),
			Description: "Choose whether a drain point consumes one or all follow-up messages",
		},
		{
			Key: settingThinkingLevel, Title: "Thinking level",
			Badge:       string(m.state.ThinkingLevel),
			Description: "Select a reasoning effort supported by the active model",
		},
		{
			Key: settingThinkingVisible, Title: "Show thinking content",
			Badge:       enabledLabel(m.renderer != nil && m.renderer.thinkingVisible),
			Description: "Show or hide reasoning blocks in the transcript",
		},
	}
}

func (m *Model) applySettingsSelection(selected selectorItem) tea.Cmd {
	if m == nil {
		return nil
	}
	var command application.Command
	switch selected.Key {
	case settingAutoCompaction:
		command = application.SetAutoCompactionCommand{Enabled: !m.state.AutoCompactionEnabled}
	case settingAutoRetry:
		command = application.SetAutoRetryCommand{Enabled: !m.state.AutoRetryEnabled}
	case settingSteeringMode:
		command = application.SetSteeringModeCommand{Mode: toggledQueueMode(m.state.SteeringMode)}
	case settingFollowUpMode:
		command = application.SetFollowUpModeCommand{Mode: toggledQueueMode(m.state.FollowUpMode)}
	case settingThinkingLevel:
		return tea.Batch(m.closeSelector(), m.openThinkingSelector())
	case settingThinkingVisible:
		m.renderer.SetThinkingVisible(!m.renderer.thinkingVisible)
		m.selector.SetItems(m.settingsSelectorItems(), "Changes are saved immediately")
		if m.renderer.thinkingVisible {
			m.setStatus("Thinking content shown", statusSuccess)
		} else {
			m.setStatus("Thinking content hidden", statusSuccess)
		}
		return nil
	default:
		m.setStatus("Unknown setting: "+selected.Key, statusError)
		return nil
	}
	m.setStatus("Updating "+selected.Title+"…", statusInfo)
	return m.dispatchCommand(command, "", nil)
}

func enabledLabel(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}

func queueModeLabel(mode agent.QueueMode) string {
	if mode == agent.QueueOneAtATime || mode == agent.QueueAll {
		return mode.String()
	}
	return "one-at-a-time"
}

func toggledQueueMode(mode agent.QueueMode) agent.QueueMode {
	if mode == agent.QueueOneAtATime {
		return agent.QueueAll
	}
	return agent.QueueOneAtATime
}

func settingUpdatedStatus(name string, value any) string {
	return fmt.Sprintf("%s: %v", name, value)
}

func (m *Model) refreshOpenSettings() {
	if m != nil && m.selector != nil && m.selector.kind == selectorSettings {
		m.selector.SetItems(m.settingsSelectorItems(), "Changes are saved immediately")
	}
}
