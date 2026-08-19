package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/session"
)

func (m *Model) openTreeSummarySelector(targetID string) tea.Cmd {
	m.treeTargetID = targetID
	_, focus := m.activateSelector(selectorTreeSummary, "Summarize abandoned branch?", "", false, false)
	m.selector.SetItems([]selectorItem{
		{Key: "none", Title: "No summary", Description: "Navigate without preserving the abandoned branch"},
		{Key: "summarize", Title: "Summarize", Description: "Generate a branch summary before navigating"},
		{Key: "custom", Title: "Summarize with custom instructions", Description: "Provide instructions for the branch summary"},
	}, "")
	return focus
}

func (m *Model) openTreeSummaryCustomSelector() tea.Cmd {
	_, focus := m.activateSelector(selectorTreeSummaryCustom, "Branch summary instructions", "", true, false)
	m.selector.input.Prompt = "Instructions: "
	m.selector.input.Placeholder = "what should the summary preserve?"
	m.selector.SetItems([]selectorItem{{
		Key: "apply", Title: "Generate summary", Description: "Press Enter to summarize with the instructions above",
	}}, "")
	return focus
}

func (m *Model) applyTreeSelectorSelection(kind selectorKind, selected selectorItem) tea.Cmd {
	switch kind {
	case selectorTree:
		if selected.Current {
			focus := m.closeSelector()
			m.setStatus("Already at this point", statusWarning)
			return focus
		}
		return m.openTreeSummarySelector(selected.Key)
	case selectorFork:
		focus := m.closeSelector()
		m.setStatus("Forking session…", statusInfo)
		return tea.Batch(focus, m.dispatchCommand(application.ForkCommand{
			EntryID: selected.Key, Position: agent.ForkBefore,
		}, "", nil))
	case selectorTreeSummary:
		targetID := m.treeTargetID
		if selected.Key == "custom" {
			return m.openTreeSummaryCustomSelector()
		}
		options := agent.NavigateTreeOptions{Summarize: selected.Key == "summarize"}
		focus := m.closeSelector()
		return tea.Batch(focus, m.dispatchTreeNavigation(targetID, options))
	case selectorTreeSummaryCustom:
		instructions := strings.TrimSpace(m.selector.Query())
		if instructions == "" {
			m.setStatus("Enter branch summary instructions", statusWarning)
			return nil
		}
		targetID := m.treeTargetID
		focus := m.closeSelector()
		return tea.Batch(focus, m.dispatchTreeNavigation(targetID, agent.NavigateTreeOptions{
			Summarize: true, CustomInstructions: &instructions,
		}))
	default:
		return nil
	}
}

func (m *Model) dispatchTreeNavigation(targetID string, options agent.NavigateTreeOptions) tea.Cmd {
	if strings.TrimSpace(targetID) == "" {
		m.setStatus("Tree target is unavailable", statusError)
		return nil
	}
	command := m.dispatchCommand(application.NavigateTreeCommand{TargetID: targetID, Options: options}, "", nil)
	if options.Summarize {
		m.branchSummaryRequest = m.commandRequest
		m.setStatus("Summarizing branch…", statusInfo)
	} else {
		m.setStatus("Navigating session tree…", statusInfo)
	}
	m.syncComposerState()
	return command
}

func (m *Model) cloneCurrentSession() tea.Cmd {
	if m.busy() {
		m.setStatus("Abort the active operation before cloning", statusWarning)
		return nil
	}
	if m.snapshotLeafID == nil || strings.TrimSpace(*m.snapshotLeafID) == "" {
		m.setStatus("Nothing to clone yet", statusWarning)
		return nil
	}
	m.setStatus("Cloning session…", statusInfo)
	return m.dispatchCommand(application.ForkCommand{
		EntryID: *m.snapshotLeafID, Position: agent.ForkAt,
	}, "", nil)
}

func treeSelectorItems(snapshot application.SessionSnapshot) []selectorItem {
	items := make([]selectorItem, 0, len(snapshot.Entries))
	var appendNodes func([]session.TreeNode, int)
	appendNodes = func(nodes []session.TreeNode, branchDepth int) {
		for _, node := range nodes {
			role, preview := treeEntryPreview(node.Entry)
			label := ""
			if node.Label != nil {
				label = strings.TrimSpace(*node.Label)
			}
			title := preview
			if label != "" {
				title = label + " — " + preview
			}
			if title == "" {
				title = node.Entry.Type()
			}
			// Session entries form a parent chain, so chronological depth can be
			// hundreds of nodes even when there is no branch. Indent only after a
			// real fork; otherwise a normal conversation is pushed off-screen.
			title = strings.Repeat("  ", min(branchDepth, 12)) + "• " + singleLine(title)
			description := fmt.Sprintf("%s • %s • %s", role, shortID(node.Entry.ID()), node.Entry.Timestamp().Local().Format("2006-01-02 15:04:05"))
			current := snapshot.LeafID != nil && node.Entry.ID() == *snapshot.LeafID
			items = append(items, selectorItem{
				Key: node.Entry.ID(), Title: title, Badge: role, Description: description,
				Keywords: node.Entry.ID() + " " + role + " " + label + " " + preview, Current: current,
			})
			childDepth := branchDepth
			if len(node.Children) > 1 {
				childDepth++
			}
			appendNodes(node.Children, childDepth)
		}
	}
	appendNodes(snapshot.Tree, 0)
	return items
}

func forkSelectorItems(snapshot application.SessionSnapshot) []selectorItem {
	items := make([]selectorItem, 0)
	for _, entry := range snapshot.Entries {
		role, ok := entry.MessageRole()
		if !ok || role != "user" {
			continue
		}
		_, preview := treeEntryPreview(entry)
		preview = strings.TrimSpace(preview)
		if preview == "" {
			continue
		}
		items = append(items, selectorItem{
			Key: entry.ID(), Title: singleLine(preview), Badge: "user",
			Description: fmt.Sprintf("%s • %s", shortID(entry.ID()), entry.Timestamp().Local().Format("2006-01-02 15:04:05")),
			Keywords:    entry.ID() + " " + preview,
		})
	}
	return items
}

func treeEntryPreview(entry session.Entry) (string, string) {
	role := entry.Type()
	if messageRole, ok := entry.MessageRole(); ok && messageRole != "" {
		role = messageRole
	}
	item, ok := contentItemFromEntry(entry)
	if !ok {
		return role, entry.Type()
	}
	parts := make([]string, 0, len(item.Blocks))
	for _, block := range item.Blocks {
		if text := strings.TrimSpace(block.Text); text != "" {
			parts = append(parts, text)
		}
	}
	preview := strings.TrimSpace(strings.Join(parts, " "))
	if preview == "" {
		preview = strings.TrimSpace(item.Title)
	}
	return role, preview
}
