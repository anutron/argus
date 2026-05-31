package tui

import (
	"context"
	"fmt"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/drn/argus/internal/db"
	"github.com/drn/argus/internal/tui/terminalpane"
	"github.com/drn/argus/internal/tui/views"
	"github.com/drn/argus/internal/uxlog"
)

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
type pluginConnectorFactory func(url string, onBytes func([]byte), in <-chan []byte) pluginConnector

func defaultPluginConnectorFactory(url string, onBytes func([]byte), in <-chan []byte) pluginConnector {
	return views.NewConnector(url, onBytes, in)
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

	conn := a.pluginConnFactory(m.view.CallbackURL, func(b []byte) {
		// Forward plugin → streampane source. Non-blocking — drop on
		// backpressure to match the rest of argus's PTY plumbing.
		select {
		case m.bytesIn <- b:
		default:
		}
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

	a.mode = modeTaskList
	a.pages.SwitchToPage("tasks")
	a.tapp.SetFocus(a.tasklist)
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
