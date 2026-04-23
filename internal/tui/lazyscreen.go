package tui

import "github.com/gdamore/tcell/v2"

// lazyScreen wraps a tcell.Screen. It exists as a passthrough wrapper so
// `app.screen` has a stable type the rest of the package can pin to (and
// so that smoke tests can inject a SimulationScreen through the same
// indirection production uses).
//
// A prior incarnation of this wrapper skipped tview's screen-wide Clear()
// for PTY-forwarded keystrokes, shaving ~10K cell writes per keystroke.
// That optimization was removed because it leaked stale cells whenever the
// layout shifted between frames (resize, page swap, agent-header toggle),
// producing ghost status bars and persistent render tearing. tcell's Show()
// diffs cells against their last-emitted state, so a full Clear followed
// by widgets restoring the same content emits nothing to the terminal —
// the optimization was saving in-process cell writes, not terminal I/O.
type lazyScreen struct {
	tcell.Screen
}
