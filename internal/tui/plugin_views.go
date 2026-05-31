package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/drn/argus/internal/db"
	"github.com/drn/argus/internal/tui/terminalpane"
	"github.com/drn/argus/internal/tui/views"
	"github.com/drn/argus/internal/tui/widget"
	"github.com/drn/argus/internal/uxlog"
)

// HotkeyItem is one entry in a plugin's pushed hotkey dictionary. Stage 5
// (bottom bar) and Stage 6 (help overlay) consume the stored slice; this
// stage only decodes and stores it.
type HotkeyItem struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Bar   bool   `json:"bar"`
}

// pluginControlEnvelope is the typed decode for plugin → argus control
// frames. Only Type is needed to dispatch release/help; Items carries the
// hotkey dictionary for the hotkeys envelope. Unknown types and malformed
// JSON decode to a zero/garbage value that dispatchPluginControl ignores.
type pluginControlEnvelope struct {
	Type  string       `json:"type"`
	Items []HotkeyItem `json:"items,omitempty"`
}

// pluginConnector is the minimum surface the App uses from a real
// views.Connector. Abstracted so smoke tests can replace the real WebSocket
// dial with an in-process no-op.
type pluginConnector interface {
	Dial(ctx context.Context) error
	SendResize(cols, rows int) error
	SendFocus() error
	SendBlur() error
	Close() error
}

// pluginConnectorFactory builds a Connector wired to the given URL and byte
// sinks. The default factory returns a real views.Connector. Tests assign a
// factory that returns a stub so they can observe the lifecycle without
// touching the network.
type pluginConnectorFactory func(url string, onBytes func([]byte), onControl func([]byte), in <-chan []byte) pluginConnector

func defaultPluginConnectorFactory(url string, onBytes func([]byte), onControl func([]byte), in <-chan []byte) pluginConnector {
	return views.NewConnector(url, onBytes, onControl, in)
}

// pluginViewMount is one registered plugin view mounted as a tview.Page. Each
// mount carries its own bytes pipes; the actual WebSocket connector is
// created lazily on hotkey activation and torn down on Esc.
type pluginViewMount struct {
	view     *views.View
	pane     *terminalpane.TerminalPane
	pageName string
	hotkey   tcell.Key

	bytesIn chan []byte // ANSI from plugin → terminalpane source
	keysOut chan []byte // keystrokes from pane → plugin

	conn pluginConnector // nil when the view is not active

	// hotkeys is the latest dictionary the plugin pushed via a hotkeys
	// control frame. Stage 5 renders the bar:true subset in the bottom bar;
	// Stage 6 renders the full set in the help overlay. Set on dispatch, only
	// read on the tview goroutine. Cleared implicitly when the mount is torn
	// down (a fresh activation starts with the prior dictionary, so Stage 5
	// will reset it on deactivate to avoid bleed between plugins).
	hotkeys []HotkeyItem
}

// loadPluginViews reads the registry and mounts every registered view as a
// tview.Page. Idempotent — calling twice is safe; later calls rebuild the
// list and replace any prior mounts. Called from buildUI; the app's tick
// loop does not refresh dynamically because plugin registrations are rare
// and a new view requires a TUI restart to surface today.
func (a *App) loadPluginViews() {
	if a.pluginConnFactory == nil {
		a.pluginConnFactory = defaultPluginConnectorFactory
	}
	// Remote-TUI mode is a.db = *apistore.Store, which the views.Registry
	// cannot use directly. The remote-TUI flow will mount plugin views over
	// the REST API in a follow-up; for now, no-op cleanly.
	local, ok := a.db.(*db.DB)
	if !ok {
		return
	}

	reg := views.New(local)
	all := reg.List()

	// Tear down any previous mounts before rebuilding (defensive — buildUI
	// only calls this once but the test harness rebuilds the App in place).
	for _, m := range a.pluginMounts {
		a.pages.RemovePage(m.pageName)
		close(m.bytesIn)
		close(m.keysOut)
	}
	a.pluginMounts = a.pluginMounts[:0]
	a.pluginHotkeys = make(map[tcell.Key]*pluginViewMount)

	for _, v := range all {
		key, ok := views.ParseHotkey(v.Hotkey)
		if !ok {
			uxlog.Log("[plugin-view] skipped %q: invalid hotkey %q", v.Title, v.Hotkey)
			continue
		}
		bytesIn := make(chan []byte, 64)
		keysOut := make(chan []byte, 64)
		pane := terminalpane.New(bytesIn)
		pane.SetTitle(v.Title)
		pane.SetInputBack(keysOut)
		pane.OnNeedRedraw = func() {
			if a.tapp != nil {
				a.tapp.QueueUpdateDraw(func() {})
			}
		}
		pageName := fmt.Sprintf("plugin-view:%d", v.ID)
		a.pages.AddPage(pageName, pane, true, false)

		m := &pluginViewMount{
			view:     v,
			pane:     pane,
			pageName: pageName,
			hotkey:   tcell.Key(key),
			bytesIn:  bytesIn,
			keysOut:  keysOut,
		}
		a.pluginMounts = append(a.pluginMounts, m)
		a.pluginHotkeys[tcell.Key(key)] = m
	}
}

// activatePluginView opens a plugin view: switches Pages, dials the WS,
// emits resize+focus envelopes. Idempotent — re-activating an already-active
// view re-sends the resize envelope so a stale plugin can recover.
func (a *App) activatePluginView(m *pluginViewMount) {
	if a.activePlugin != nil && a.activePlugin == m {
		// Re-send resize as a recovery hint and bail.
		if a.activePlugin.conn != nil {
			cols, rows := a.pluginViewportSize()
			_ = a.activePlugin.conn.SendResize(cols, rows)
		}
		return
	}
	if a.activePlugin != nil {
		a.deactivatePluginView()
	}

	a.activePlugin = m
	a.mode = modePluginView
	// Reset the failsafe timestamp so a stale Ctrl+Q from a prior view can't
	// trip the double-tap failsafe on the first press in this one.
	a.lastCtrlQ = time.Time{}
	uxlog.Log("[plugin-view] surrender: %q has the keyboard (full surrender, ^Q^Q failsafe)", m.view.Title)
	a.pages.SwitchToPage(m.pageName)
	a.tapp.SetFocus(m.pane)

	// Context-sensitive bottom bar: surface the plugin's bar:true hotkeys (if
	// any were pushed before activation) plus the reserved exit hint.
	a.statusbar.SetPluginMode(true, m.view.Title, barHints(m.hotkeys))

	conn := a.pluginConnFactory(m.view.CallbackURL, func(b []byte) {
		// Forward plugin → streampane source. Non-blocking — drop on
		// backpressure to match the rest of argus's PTY plumbing.
		select {
		case m.bytesIn <- b:
		default:
		}
	}, func(b []byte) {
		// onControl runs on the connector's read-pump goroutine — it must NOT
		// touch tview or App state directly. dispatchPluginControl decodes
		// defensively and routes every tview interaction through
		// QueueUpdateDraw.
		a.dispatchPluginControl(m, b)
	}, m.keysOut)
	m.conn = conn

	go func(c pluginConnector) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.Dial(ctx); err != nil {
			uxlog.Log("[plugin-view] dial %q failed: %v", m.view.Title, err)
			return
		}
		cols, rows := a.pluginViewportSize()
		if err := c.SendResize(cols, rows); err != nil {
			uxlog.Log("[plugin-view] send resize failed: %v", err)
		}
		if err := c.SendFocus(); err != nil {
			uxlog.Log("[plugin-view] send focus failed: %v", err)
		}
	}(conn)
}

// deactivatePluginView closes the active plugin view: sends blur, closes the
// WS, switches back to the tasks page.
func (a *App) deactivatePluginView() {
	if a.activePlugin == nil {
		return
	}
	m := a.activePlugin
	a.activePlugin = nil
	uxlog.Log("[plugin-view] release: %q gave back the keyboard", m.view.Title)

	if m.conn != nil {
		_ = m.conn.SendBlur()
		_ = m.conn.Close()
		m.conn = nil
	}

	// Clear all plugin-view bar state so nothing bleeds into the next plugin:
	// drop the bottom-bar plugin mode (argus's own tab hints return — the bar
	// already tracks the active tab via SetTab), forget the pushed dictionary,
	// and reset the help-requested seam.
	a.statusbar.SetPluginMode(false, "", nil)
	m.hotkeys = nil
	a.pluginHelpRequested = false

	a.mode = modeTaskList
	a.pages.SwitchToPage("tasks")
	a.tapp.SetFocus(a.tasklist)
}

// barHints converts a plugin's pushed hotkey dictionary into the widget-local
// PluginHint slice the bottom bar renders, filtering to the bar:true subset.
// Defined on the app side so the widget package never imports tui (cycle).
func barHints(items []HotkeyItem) []widget.PluginHint {
	var out []widget.PluginHint
	for _, it := range items {
		if it.Bar {
			out = append(out, widget.PluginHint{Key: it.Key, Label: it.Label})
		}
	}
	return out
}

// dispatchPluginControl decodes a raw plugin → argus control frame and
// routes it to the matching handler. Runs on the connector's read-pump
// goroutine, so every handler that touches tview or App state defers to
// a.tapp.QueueUpdateDraw. Malformed JSON and unknown types are logged and
// ignored — never panics, never disturbs the binary ANSI stream.
//
// mount is the mount the control frame arrived on. Handlers re-check that it
// is still the active plugin under QueueUpdateDraw, because a release/failsafe
// could have fired between the read and the queued closure running.
func (a *App) dispatchPluginControl(mount *pluginViewMount, raw []byte) {
	uxlog.Log("[plugin-view] control frame received (%d bytes)", len(raw))
	var env pluginControlEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		uxlog.Log("[plugin-view] ignored malformed control frame: %v", err)
		return
	}
	switch env.Type {
	case "release":
		uxlog.Log("[plugin-view] dispatch: release")
		if a.tapp != nil {
			a.tapp.QueueUpdateDraw(func() { a.deactivatePluginView() })
		}
	case "hotkeys":
		uxlog.Log("[plugin-view] dispatch: hotkeys (%d items)", len(env.Items))
		items := env.Items
		if a.tapp != nil {
			a.tapp.QueueUpdateDraw(func() {
				// Guard against a stale/nil mount: only store if this mount is
				// still the active plugin (a release could have raced in).
				if a.activePlugin == nil || a.activePlugin != mount {
					uxlog.Log("[plugin-view] hotkeys dropped: mount no longer active")
					return
				}
				mount.hotkeys = items
				// Refresh the bottom bar live with the bar:true subset.
				a.statusbar.SetPluginMode(true, mount.view.Title, barHints(items))
			})
		}
	case "help":
		uxlog.Log("[plugin-view] dispatch: help")
		if a.tapp != nil {
			a.tapp.QueueUpdateDraw(func() { a.requestPluginHelp(mount) })
		}
	default:
		uxlog.Log("[plugin-view] ignored unknown control type %q", env.Type)
	}
}

// requestPluginHelp is the Stage 6 seam for the plugin-triggered help overlay.
// For now it only records that help was requested (observable via
// pluginHelpRequested) and logs; the overlay rendering lands in Stage 6.
// Runs on the tview goroutine (called from a QueueUpdateDraw closure).
//
// Stage 6: render mount.hotkeys (the full dictionary, ignoring the bar flag)
// in argus's help modal, styled like argus help, showing only the plugin's
// hotkeys.
func (a *App) requestPluginHelp(mount *pluginViewMount) {
	if a.activePlugin == nil || a.activePlugin != mount {
		uxlog.Log("[plugin-view] help dropped: mount no longer active")
		return
	}
	a.pluginHelpRequested = true
	uxlog.Log("[plugin-view] help requested for %q (overlay is Stage 6)", mount.view.Title)
}

// pluginViewportSize returns the cols/rows the active plugin view should
// render into. Falls back to the screen size when the streampane hasn't been
// drawn yet.
func (a *App) pluginViewportSize() (int, int) {
	if a.activePlugin != nil {
		x, y, w, h := a.activePlugin.pane.GetRect()
		_ = x
		_ = y
		if w > 2 && h > 2 {
			return w - 2, h - 2 // subtract border
		}
	}
	if a.screen != nil {
		w, h := a.screen.Size()
		return w, h
	}
	return 80, 24
}

// resizePluginViewIfActive forwards a resize envelope to the active plugin
// view (if any). Called from afterDraw when the terminal dimensions change.
func (a *App) resizePluginViewIfActive() {
	if a.activePlugin == nil || a.activePlugin.conn == nil {
		return
	}
	cols, rows := a.pluginViewportSize()
	_ = a.activePlugin.conn.SendResize(cols, rows)
}
