package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

type literalItemRenderer struct{}

func (literalItemRenderer) CacheKey() string { return "literal" }
func (literalItemRenderer) Render(item contentItem, _ int) []string {
	return strings.Split(item.Title, "\n")
}

func TestTranscriptMapsVisibleRowsAndCopiesWideSelections(t *testing.T) {
	transcript := newTranscriptModel()
	transcript.SetItems([]contentItem{
		{ID: "one", Revision: 1, Title: "alpha界"},
		{ID: "two", Revision: 1, Title: "omega"},
	})
	if got := StripTerminalSequences(transcript.View(20, 3, literalItemRenderer{})); got != "alpha界\n\nomega" {
		t.Fatalf("transcript view = %q", got)
	}
	anchor, _, ok := transcript.CellAt(0, 6, false)
	if !ok {
		t.Fatal("wide-character anchor was not mapped")
	}
	focus, _, ok := transcript.CellAt(2, 1, false)
	if !ok {
		t.Fatal("focus was not mapped")
	}
	if got := transcript.SelectedText(anchor, focus); got != "界\n\nom" {
		t.Fatalf("selected text = %q", got)
	}
	if got := transcript.SelectedText(focus, anchor); got != "界\n\nom" {
		t.Fatalf("reverse selected text = %q", got)
	}

	_ = transcript.View(20, 5, literalItemRenderer{})
	if _, _, ok := transcript.CellAt(0, 0, false); ok {
		t.Fatal("tail padding was exposed as transcript content")
	}
	cell, row, ok := transcript.CellAt(0, 0, true)
	if !ok || row.position != (transcriptPosition{item: 0, line: 0}) || cell.column != 0 {
		t.Fatalf("clamped padded cell = %#v, row=%#v, ok=%t", cell, row, ok)
	}
}

func TestOSC8LinkAtColumnTracksGraphemeCellsAndClosures(t *testing.T) {
	const url = "https://example.test/a?x=1&y=2"
	line := "a\x1b]8;id=42;" + url + "\a界x\x1b]8;;\az"
	for _, column := range []int{1, 2, 3} {
		if got := osc8LinkAtColumn(line, column); got != url {
			t.Fatalf("column %d link = %q", column, got)
		}
	}
	for _, column := range []int{0, 4, 5} {
		if got := osc8LinkAtColumn(line, column); got != "" {
			t.Fatalf("column %d unexpectedly linked to %q", column, got)
		}
	}
	if got := osc8LinkAtColumn("\x1b]8;;https://st.test\x1b\\go\x1b]8;;\x1b\\", 1); got != "https://st.test" {
		t.Fatalf("ST-terminated link = %q", got)
	}
}

func TestModelMouseDragCopiesTranscriptSelection(t *testing.T) {
	model := newModelForTest(t)
	model.transcript.SetItems([]contentItem{{ID: "one", Revision: 1, Title: "abcdef"}})
	_ = model.transcript.View(20, 1, literalItemRenderer{})
	copied := ""
	model.setClipboard = func(value string) tea.Cmd {
		copied = value
		return func() tea.Msg { return nil }
	}
	_, _ = model.Update(tea.MouseClickMsg{X: 1, Y: 0, Button: tea.MouseLeft})
	_, _ = model.Update(tea.MouseMotionMsg{X: 3, Y: 0, Button: tea.MouseLeft})
	_, _ = model.Update(tea.MouseReleaseMsg{X: 3, Y: 0, Button: tea.MouseLeft})
	if copied != "bcd" {
		t.Fatalf("clipboard = %q", copied)
	}
	if model.status.text != "Copied!" || model.status.level != statusSuccess {
		t.Fatalf("copy status = %#v", model.status)
	}
	if model.mouseSelection == nil || model.mouseSelection.active {
		t.Fatalf("final selection = %#v", model.mouseSelection)
	}
}

func TestModelMouseDragCopiesVisibleOverlayScreen(t *testing.T) {
	model := newModelForTest(t)
	model.width = 20
	model.lastScreen = []string{"alpha界", "", "omega"}
	model.selector = newSelectorModel(model.theme, selectorThinking, "Choose", "", false, false)
	copied := ""
	model.setClipboard = func(value string) tea.Cmd {
		copied = value
		return func() tea.Msg { return nil }
	}
	_, _ = model.Update(tea.MouseClickMsg{X: 6, Y: 0, Button: tea.MouseLeft})
	_, _ = model.Update(tea.MouseMotionMsg{X: 1, Y: 2, Button: tea.MouseLeft})
	_, _ = model.Update(tea.MouseReleaseMsg{X: 1, Y: 2, Button: tea.MouseLeft})
	if copied != "界\n\nom" {
		t.Fatalf("overlay clipboard = %q", copied)
	}
	if model.mouseSelection == nil || !model.mouseSelection.screen || model.mouseSelection.active {
		t.Fatalf("overlay selection = %#v", model.mouseSelection)
	}
	rendered := model.prepareScreen(strings.Join(model.lastScreen, "\n"))
	if !strings.Contains(rendered, "\x1b[7m") || StripTerminalSequences(rendered) != "alpha界\n\nomega" {
		t.Fatalf("overlay selection rendering = %q", rendered)
	}
}

func TestModelBlurCancelsActiveMouseSelection(t *testing.T) {
	model := newModelForTest(t)
	model.lastScreen = []string{"select me"}
	model.selector = newSelectorModel(model.theme, selectorThinking, "Choose", "", false, false)
	_, _ = model.Update(tea.MouseClickMsg{X: 1, Y: 0, Button: tea.MouseLeft})
	if model.mouseSelection == nil || !model.mouseSelection.active {
		t.Fatal("mouse press did not start selection")
	}
	_, _ = model.Update(tea.BlurMsg{})
	if model.mouseSelection != nil || model.mouseAutoDirection != 0 {
		t.Fatalf("blur retained mouse state: selection=%#v direction=%d", model.mouseSelection, model.mouseAutoDirection)
	}
}

func TestModelMouseClickOpensOSC8LinkWithoutShell(t *testing.T) {
	model := newModelForTest(t)
	const url = "https://example.test/?a=1&b=2"
	model.transcript.SetItems([]contentItem{{
		ID: "one", Revision: 1,
		Title: "x\x1b]8;;" + url + "\aopen\x1b]8;;\ay",
	}})
	_ = model.transcript.View(80, 1, literalItemRenderer{})
	opened := ""
	model.openURL = func(value string) error {
		opened = value
		return nil
	}
	_, _ = model.Update(tea.MouseClickMsg{X: 2, Y: 0, Button: tea.MouseLeft})
	_, command := model.Update(tea.MouseReleaseMsg{X: 2, Y: 0, Button: tea.MouseLeft})
	if command == nil {
		t.Fatal("link click did not return an opener command")
	}
	message, ok := command().(urlOpenedMsg)
	if !ok || message.err != nil || message.url != url || opened != url {
		t.Fatalf("opened message = %#v, target=%q", message, opened)
	}
	_, _ = model.Update(message)
	if model.mouseSelection != nil {
		t.Fatalf("link click retained selection: %#v", model.mouseSelection)
	}
}

func TestModelRoutesWheelToSelectorOrTranscriptByPointerRegion(t *testing.T) {
	model := newModelForTest(t)
	items := make([]contentItem, 5)
	for index := range items {
		items[index] = contentItem{ID: string(rune('a' + index)), Revision: 1, Title: string(rune('a' + index))}
	}
	model.transcript.SetItems(items)
	_ = model.transcript.View(20, 2, literalItemRenderer{})
	model.selector = newSelectorModel(model.theme, selectorThinking, "Choose", "", false, false)
	model.selector.SetItems([]selectorItem{{Key: "a"}, {Key: "b"}, {Key: "c"}}, "")

	_, _ = model.Update(tea.MouseWheelMsg{X: 1, Y: 3, Button: tea.MouseWheelUp})
	if model.selector.selected != 2 {
		t.Fatalf("selector wheel selected = %d", model.selector.selected)
	}
	if !model.transcript.follow {
		t.Fatal("selector wheel also scrolled transcript")
	}

	_, _ = model.Update(tea.MouseWheelMsg{X: 1, Y: 0, Button: tea.MouseWheelUp})
	if model.transcript.follow || model.transcript.anchor.id != "d" || model.transcript.anchor.line != 1 {
		t.Fatalf("one-line transcript wheel anchor = %#v, follow=%t", model.transcript.anchor, model.transcript.follow)
	}
}

func TestOpenLinkFailureIsVisible(t *testing.T) {
	model := newModelForTest(t)
	_, _ = model.Update(urlOpenedMsg{url: "https://example.test", err: errors.New("launcher missing")})
	if model.status.level != statusError || !strings.Contains(model.status.text, "launcher missing") {
		t.Fatalf("open failure status = %#v", model.status)
	}
}

func TestReverseSelectionRestoresReverseAfterSGR(t *testing.T) {
	rendered := reverseSelection("\x1b[31mred\x1b[0m")
	if got := StripTerminalSequences(rendered); got != "red" {
		t.Fatalf("selected rendering text = %q", got)
	}
	if strings.Count(rendered, "\x1b[7m") < 3 || !strings.HasSuffix(rendered, "\x1b[27m") {
		t.Fatalf("selected rendering did not preserve reverse state: %q", rendered)
	}
}
