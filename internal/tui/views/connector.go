package views

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Plugin-view WebSocket protocol:
//
//   - Plugin → argus: binary frames carry ANSI bytes for streampane.
//   - Argus → plugin: binary frames carry keystrokes; text frames carry JSON
//     control envelopes (resize/focus/blur).
//   - Argus reserves Esc for "exit plugin view back to argus" — Esc is
//     intercepted before the keystroke pump runs, so it never reaches the
//     plugin.
type envelopeType string

const (
	envelopeResize envelopeType = "resize"
	envelopeFocus  envelopeType = "focus"
	envelopeBlur   envelopeType = "blur"
)

type controlEnvelope struct {
	Type envelopeType `json:"type"`
	Cols int          `json:"cols,omitempty"`
	Rows int          `json:"rows,omitempty"`
}

// Connector dials a plugin's WebSocket and shuttles bytes both ways. The
// caller wires the byte sinks at construction; Dial spins up read+write pumps
// that run until Close or the remote disconnects.
type Connector struct {
	url string

	// onBytes is called with each binary frame the plugin emits. The
	// streampane's source channel is the natural sink. Always called from
	// a single goroutine, so receivers don't need their own mutex.
	onBytes func([]byte)
	// inBytes carries keystrokes from the TUI → plugin as binary frames.
	// Closing the channel signals the write pump to exit cleanly.
	inBytes <-chan []byte

	mu      sync.Mutex
	conn    *websocket.Conn
	closeCh chan struct{}
	once    sync.Once

	// dialer is the function used by Dial to open the WebSocket. The default
	// is websocket.Dial; tests can override to inject a controlled handshake.
	dialer func(ctx context.Context, url string) (*websocket.Conn, *websocketResp, error)
}

// websocketResp mirrors the http.Response that websocket.Dial returns. Kept
// internal so callers don't depend on the net/http import here.
type websocketResp struct{}

// NewConnector constructs a Connector wired to the given URL and byte sinks.
//
// onBytes is invoked from the read pump for each binary frame from the
// plugin. inBytes is consumed by the write pump as binary frames sent to the
// plugin; closing inBytes triggers a clean shutdown of the write pump.
func NewConnector(url string, onBytes func([]byte), inBytes <-chan []byte) *Connector {
	return &Connector{
		url:     url,
		onBytes: onBytes,
		inBytes: inBytes,
		closeCh: make(chan struct{}),
		dialer: func(ctx context.Context, url string) (*websocket.Conn, *websocketResp, error) {
			c, _, err := websocket.Dial(ctx, url, nil)
			if err != nil {
				return nil, nil, err
			}
			return c, &websocketResp{}, nil
		},
	}
}

// Dial opens the WebSocket and starts the read/write pumps. Returns the dial
// error untouched on failure. On success, the pumps run until Close is
// invoked or the remote disconnects.
func (c *Connector) Dial(ctx context.Context) error {
	conn, _, err := c.dialer(ctx, c.url)
	if err != nil {
		return err
	}
	// Plugin views ship full-screen ANSI frames that routinely exceed the
	// 32 KiB default coder/websocket read limit (a 316x69 surface with
	// cursor positioning + SGR colors per cell lands in the 32–50 KiB
	// range). The connector is loopback-only — argus and the plugin daemon
	// both bind 127.0.0.1 — so disabling the per-message cap is safe.
	conn.SetReadLimit(-1)
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	// G118 false positive: the pumps run for the lifetime of the WebSocket,
	// which outlives the Dial(ctx) request — tying them to ctx would cancel
	// the pumps as soon as Dial returns. Close() drives shutdown via closeCh.
	go c.readPump()  //nolint:gosec // G118
	go c.writePump() //nolint:gosec // G118
	return nil
}

// SendResize emits a {"type":"resize","cols":N,"rows":M} envelope as a TEXT
// frame. Sent on initial connect + every terminal resize while the plugin
// view is active.
func (c *Connector) SendResize(cols, rows int) error {
	return c.sendEnvelope(controlEnvelope{Type: envelopeResize, Cols: cols, Rows: rows})
}

// SendFocus emits the {"type":"focus"} envelope.
func (c *Connector) SendFocus() error {
	return c.sendEnvelope(controlEnvelope{Type: envelopeFocus})
}

// SendBlur emits the {"type":"blur"} envelope. Sent just before Close.
func (c *Connector) SendBlur() error {
	return c.sendEnvelope(controlEnvelope{Type: envelopeBlur})
}

// Close terminates the WebSocket and stops the pumps. Idempotent.
func (c *Connector) Close() error {
	var err error
	c.once.Do(func() {
		close(c.closeCh)
		c.mu.Lock()
		conn := c.conn
		c.mu.Unlock()
		if conn != nil {
			err = conn.Close(websocket.StatusNormalClosure, "client closing")
		}
	})
	return err
}

func (c *Connector) sendEnvelope(env controlEnvelope) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return errors.New("connector not dialed")
	}
	body, err := json.Marshal(env)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, body)
}

func (c *Connector) readPump() {
	for {
		select {
		case <-c.closeCh:
			return
		default:
		}
		c.mu.Lock()
		conn := c.conn
		c.mu.Unlock()
		if conn == nil {
			return
		}
		typ, data, err := conn.Read(context.Background())
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			return
		}
		if typ != websocket.MessageBinary {
			// Control text frames flow plugin→argus too, but the current
			// protocol reserves them for argus→plugin. Drop unknown text.
			continue
		}
		if c.onBytes != nil && len(data) > 0 {
			c.onBytes(data)
		}
	}
}

func (c *Connector) writePump() {
	for {
		select {
		case <-c.closeCh:
			return
		case b, ok := <-c.inBytes:
			if !ok {
				return
			}
			if len(b) == 0 {
				continue
			}
			c.mu.Lock()
			conn := c.conn
			c.mu.Unlock()
			if conn == nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := conn.Write(ctx, websocket.MessageBinary, b)
			cancel()
			if err != nil {
				return
			}
		}
	}
}
