package modal

import (
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/drn/argus/internal/testutil"
)

func TestHelpModal_Defaults(t *testing.T) {
	m := NewHelpModal()
	testutil.False(t, m.Closed())
}

func TestHelpModal_InputHandler(t *testing.T) {
	for _, tc := range []struct {
		name       string
		key        tcell.Key
		rune       rune
		wantClosed bool
	}{
		{"esc closes", tcell.KeyEscape, 0, true},
		{"ctrl+q closes", tcell.KeyCtrlQ, 0, true},
		{"? closes", tcell.KeyRune, '?', true},
		{"unrelated rune no-op", tcell.KeyRune, 'x', false},
		{"enter no-op", tcell.KeyEnter, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewHelpModal()
			ev := tcell.NewEventKey(tc.key, tc.rune, tcell.ModNone)
			m.InputHandler()(ev, nil)
			testutil.Equal(t, m.Closed(), tc.wantClosed)
		})
	}
}

func TestHelpModal_Draw(t *testing.T) {
	sim := drawAt(t, 100, 40)
	m := NewHelpModal()
	m.SetRect(0, 0, 100, 40)
	m.Draw(sim)
	sim.Sync()

	body := screenString(sim)
	testutil.Contains(t, body, "Keybindings")
	testutil.Contains(t, body, "Task List")
	testutil.Contains(t, body, "Agent View")
	testutil.Contains(t, body, "File Panel")
	testutil.Contains(t, body, "Settings")
	testutil.Contains(t, body, "[esc / ?] close")
	// Sample a few bindings to catch regressions in the section list.
	testutil.Contains(t, body, "new task")
	testutil.Contains(t, body, "fork task")
}

func TestHelpModal_DrawZeroSizeNoOp(t *testing.T) {
	sim := drawAt(t, 80, 24)
	m := NewHelpModal()
	m.SetRect(0, 0, 0, 0)
	m.Draw(sim) // must not panic
}

func TestHelpModal_DrawTinyArea(t *testing.T) {
	sim := drawAt(t, 80, 24)
	m := NewHelpModal()
	m.SetRect(0, 0, 6, 2) // below the 8x4 minimum — should short-circuit
	m.Draw(sim)
}

func TestHelpModal_DrawClampsToAvailableHeight(t *testing.T) {
	sim := drawAt(t, 80, 24)
	m := NewHelpModal()
	m.SetRect(0, 0, 80, 8) // height clamped; only the first sections fit
	m.Draw(sim)
	sim.Sync()
	body := screenString(sim)
	testutil.Contains(t, body, "Keybindings")
	// Hint must still render on the last inner row.
	testutil.Contains(t, body, "close")
}

func TestHelpSections_NonEmpty(t *testing.T) {
	testutil.True(t, len(HelpSections) > 0)
	for _, sec := range HelpSections {
		t.Run(sec.Title, func(t *testing.T) {
			testutil.True(t, sec.Title != "")
			testutil.True(t, len(sec.Bindings) > 0)
			for _, b := range sec.Bindings {
				testutil.True(t, b.Key != "")
				testutil.True(t, b.Action != "")
			}
		})
	}
}
