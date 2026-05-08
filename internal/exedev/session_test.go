package exedev

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/drn/argus/internal/agent"
	"github.com/drn/argus/internal/testutil"
)

// TestRemoteSession_EchoLoop is the core plumbing test: open a session
// against the fake SSH server, send bytes via WriteInput, expect them to
// come back through the ring buffer and any attached writer.
func TestRemoteSession_EchoLoop(t *testing.T) {
	srv := newFakeSSHServer(t)
	client := dialFakeServer(t, srv)

	rs, err := StartRemoteSession(client, "task-1", "cat", "/tmp", 24, 80, nil)
	testutil.NoError(t, err)
	defer rs.Stop() //nolint:errcheck

	tap := &syncBuf{}
	rs.AddWriter(tap)

	if _, err := rs.WriteInput([]byte("hello\n")); err != nil {
		t.Fatalf("WriteInput: %v", err)
	}
	waitFor(t, time.Second, func() bool {
		return strings.Contains(tap.String(), "hello")
	})

	got := rs.RecentOutput()
	if !bytes.Contains(got, []byte("hello")) {
		t.Fatalf("RecentOutput=%q want substring 'hello'", got)
	}

	cols, rows := rs.PTYSize()
	testutil.Equal(t, cols, 80)
	testutil.Equal(t, rows, 24)

	icols, irows := rs.InitialPTYSize()
	testutil.Equal(t, icols, 80)
	testutil.Equal(t, irows, 24)

	if !rs.Alive() {
		t.Fatal("session should be alive after WriteInput")
	}
}

func TestRemoteSession_ResizeForwarded(t *testing.T) {
	srv := newFakeSSHServer(t)
	client := dialFakeServer(t, srv)

	rs, err := StartRemoteSession(client, "task-r", "cat", "", 24, 80, nil)
	testutil.NoError(t, err)
	defer rs.Stop() //nolint:errcheck

	if err := rs.Resize(40, 120); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	cols, rows := rs.PTYSize()
	testutil.Equal(t, cols, 120)
	testutil.Equal(t, rows, 40)

	// Initial PTY size is immutable.
	icols, irows := rs.InitialPTYSize()
	testutil.Equal(t, icols, 80)
	testutil.Equal(t, irows, 24)
}

func TestRemoteSession_StopMarksDone(t *testing.T) {
	srv := newFakeSSHServer(t)
	client := dialFakeServer(t, srv)

	rs, err := StartRemoteSession(client, "task-s", "cat", "", 24, 80, nil)
	testutil.NoError(t, err)

	if err := rs.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-rs.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done never closed after Stop")
	}
	if rs.Alive() {
		t.Fatal("Alive should be false after Stop")
	}
}

func TestRemoteSession_AddWriterReplay(t *testing.T) {
	srv := newFakeSSHServer(t)
	client := dialFakeServer(t, srv)

	rs, err := StartRemoteSession(client, "task-w", "cat", "", 24, 80, nil)
	testutil.NoError(t, err)
	defer rs.Stop() //nolint:errcheck

	rs.WriteInput([]byte("seed\n")) //nolint:errcheck
	waitFor(t, time.Second, func() bool {
		return bytes.Contains(rs.RecentOutput(), []byte("seed"))
	})

	tap := &syncBuf{}
	rs.AddWriter(tap)
	if !bytes.Contains(tap.Bytes(), []byte("seed")) {
		t.Fatalf("AddWriter did not replay buffered output: %q", tap.String())
	}
}

func TestBuildRemoteCommand_QuotesAndChain(t *testing.T) {
	got := buildRemoteCommand("/home/x/y", "claude --foo", map[string]string{
		"FOO": "value with space",
	})
	if !strings.HasPrefix(got, "bash -lc ") {
		t.Fatalf("expected bash -lc prefix, got %q", got)
	}
	// The whole inner script is quoted as one bash -lc arg, so the
	// per-token single quotes get escaped with the '\\'' escape sequence.
	// Sanity-check that the value, workdir, and command all show up
	// somewhere in the script — exact escape pattern is a function of
	// how many layers deep we are.
	for _, want := range []string{"value with space", "/home/x/y", "claude --foo", "cd ", "&&"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}

func TestBuildRemoteCommand_NoWorkdir(t *testing.T) {
	got := buildRemoteCommand("", "echo hi", nil)
	if strings.Contains(got, "cd '") {
		t.Fatalf("no cd expected when workdir empty, got %q", got)
	}
	if !strings.Contains(got, "echo hi") {
		t.Fatalf("command missing: %q", got)
	}
}

// syncBuf is a goroutine-safe bytes.Buffer for capturing writer output.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *syncBuf) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]byte, s.buf.Len())
	copy(out, s.buf.Bytes())
	return out
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("waitFor: condition never satisfied")
}

// Compile-time assertion that RemoteSession satisfies SessionHandle, mirrored
// here so tests fail fast if the interface drifts.
var _ agent.SessionHandle = (*RemoteSession)(nil)
