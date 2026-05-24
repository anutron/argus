package modal

import (
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/drn/argus/internal/tui/theme"
	"github.com/drn/argus/internal/tui/widget"
)

// HelpModal shows a centered overlay listing keybindings grouped by context.
// Esc, Ctrl+Q, or `?` closes it.
type HelpModal struct {
	*tview.Box
	closed bool
}

// HelpBinding is a single key/action pair.
type HelpBinding struct {
	Key    string
	Action string
}

// HelpSection groups bindings under a heading.
type HelpSection struct {
	Title    string
	Bindings []HelpBinding
}

// HelpSections is the canonical inventory rendered by the modal. Source of
// truth for the help overlay — mirror updates here when bindings change.
var HelpSections = []HelpSection{
	{
		Title: "Task List",
		Bindings: []HelpBinding{
			{"n", "new task"},
			{"Enter", "open agent view"},
			{"j / k", "navigate up/down"},
			{"s / S", "advance / revert status"},
			{"a", "toggle archive"},
			{"P", "toggle pin"},
			{"r", "rename"},
			{"c", "copy prompt"},
			{"/", "filter"},
			{"ctrl+f", "fork task"},
			{"ctrl+d", "destroy task"},
			{"ctrl+r", "prune completed"},
			{"ctrl+p", "open PR"},
			{"ctrl+o", "open repo"},
			{"ctrl+l", "refresh screen"},
			{"1 / 2 / 3", "switch tab (tasks / DAG / settings)"},
			{"q", "quit"},
		},
	},
	{
		Title: "Agent View",
		Bindings: []HelpBinding{
			{"ctrl+q / Esc", "back (diff → files → list)"},
			{"Cmd+← / Cmd+→", "switch panels"},
			{"Cmd+↑ / Cmd+↓", "navigate tasks"},
			{"Shift+↑ / Shift+↓", "scroll terminal"},
			{"ctrl+l", "link picker"},
			{"ctrl+p", "open PR"},
			{"ctrl+y", "copy staged text"},
		},
	},
	{
		Title: "File Panel",
		Bindings: []HelpBinding{
			{"Enter", "open diff"},
			{"s", "toggle split/unified"},
			{"o", "reveal in Finder"},
			{"e", "open in editor"},
			{"t", "open terminal in worktree"},
		},
	},
	{
		Title: "Settings",
		Bindings: []HelpBinding{
			{"j / k", "navigate rows"},
			{"n", "new project / backend / schedule"},
			{"e", "edit"},
			{"d", "delete / set default"},
			{"t", "toggle schedule enabled"},
			{"r", "run schedule now"},
			{"i", "quick add projects"},
			{"Enter / ◀ / ▶", "toggle / cycle settings"},
		},
	},
	{
		Title: "Modals & Forms",
		Bindings: []HelpBinding{
			{"Esc / ctrl+q", "close / cancel"},
			{"Enter", "confirm / submit"},
			{"Tab / Shift+Tab", "navigate fields"},
		},
	},
}

// NewHelpModal creates a help overlay.
func NewHelpModal() *HelpModal {
	return &HelpModal{Box: tview.NewBox()}
}

// Closed reports whether the user dismissed the modal.
func (m *HelpModal) Closed() bool { return m.closed }

// InputHandler closes the modal on Esc, Ctrl+Q, or `?`.
func (m *HelpModal) InputHandler() func(event *tcell.EventKey, setFocus func(p tview.Primitive)) {
	return m.WrapInputHandler(func(event *tcell.EventKey, setFocus func(p tview.Primitive)) {
		switch event.Key() {
		case tcell.KeyEscape, tcell.KeyCtrlQ:
			m.closed = true
			return
		}
		if event.Key() == tcell.KeyRune && event.Rune() == '?' {
			m.closed = true
		}
	})
}

// helpModalSize returns the modal's width and total drawn height based on
// the available rect.
func helpModalSize(width, height int) (w, h int) {
	w = min(72, width-4)
	if w < 24 {
		w = width
	}
	h = 4 // border (2) + title gap (1) + hint (1)
	for _, sec := range HelpSections {
		h += 1 + len(sec.Bindings) + 1 // heading + rows + spacer
	}
	if h > height {
		h = height
	}
	return w, h
}

// Draw renders the help modal centered in the available rect.
func (m *HelpModal) Draw(screen tcell.Screen) {
	m.Box.DrawForSubclass(screen, m)
	x, y, width, height := m.GetInnerRect()
	if width <= 0 || height <= 0 {
		return
	}

	formW, formH := helpModalSize(width, height)
	if formW < 8 || formH < 4 {
		return
	}
	formX := x + (width-formW)/2
	formY := y + (height-formH)/2
	if formY < y {
		formY = y
	}

	widget.FillArea(screen, formX, formY, formW, formH, ' ', tcell.StyleDefault)
	inner := widget.DrawBorderedPanel(screen, formX, formY, formW, formH, "Keybindings", theme.StyleFocusedBorder)
	if inner.W <= 0 || inner.H <= 0 {
		return
	}

	// Column layout: key column ~18 chars, action takes the rest.
	keyCol := 18
	if keyCol > inner.W/2 {
		keyCol = inner.W / 2
	}

	row := inner.Y
	maxRow := inner.Y + inner.H - 2 // reserve a row for the hint
	for _, sec := range HelpSections {
		if row >= maxRow {
			break
		}
		widget.DrawText(screen, inner.X, row, inner.W, sec.Title, theme.StyleTitle)
		row++
		for _, b := range sec.Bindings {
			if row >= maxRow {
				break
			}
			widget.DrawText(screen, inner.X+2, row, keyCol, b.Key, theme.StyleNormal)
			widget.DrawText(screen, inner.X+2+keyCol, row, inner.W-keyCol-2, b.Action, theme.StyleDimmed)
			row++
		}
		row++ // section spacer
	}

	// Footer hint pinned to the last inner row.
	widget.DrawText(screen, inner.X, inner.Y+inner.H-1, inner.W, "[esc / ?] close", theme.StyleDimmed)
}
