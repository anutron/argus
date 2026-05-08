package exedev

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/drn/argus/internal/agent"
	"golang.org/x/crypto/ssh"
)

// defaultBufSize mirrors agent.defaultBufSize so the local replay window for
// a remote session matches the local-runtime experience.
const defaultBufSize = 256 * 1024

// idleThreshold mirrors agent.idleThreshold. Kept as a separate constant so
// the two packages don't import each other's privates.
const idleThreshold = 3 * time.Second

// ErrSessionClosed is returned by WriteInput / Resize after the SSH channel
// has been torn down by waitLoop or Stop.
var ErrSessionClosed = errors.New("exedev: session closed")

// RemoteSession is an agent.SessionHandle whose PTY lives on a remote
// exe.dev VM. The byte stream is multiplexed through an SSH session; locally
// it looks identical to an agent.Session (ring buffer, writers, idle timer,
// done channel) so callers depend only on the SessionHandle interface.
type RemoteSession struct {
	taskID  string
	client  *ssh.Client
	session *ssh.Session
	stdin   io.WriteCloser
	workdir string

	mu         sync.Mutex
	buf        *agent.RingBuffer
	writers    []io.Writer
	done       chan struct{}
	err        error
	ptyCols    uint16
	ptyRows    uint16
	initCols   uint16
	initRows   uint16
	lastOutput time.Time
	lastInput  time.Time
	closed     bool
}

// startInput captures everything StartRemoteSession needs to know about
// what to run. Kept package-private so callers go through StartRemoteSession.
type startInput struct {
	taskID  string
	command string
	workdir string
	rows    uint16
	cols    uint16
	envVars map[string]string
}

// StartRemoteSession dials a new SSH session on client, requests a PTY,
// runs `cd <workdir> && <command>` (with env injected via export), and
// begins streaming output into a local ring buffer.
//
// The returned session reports Done when the remote process exits. Any
// failure during PTY request or command start tears down the SSH session
// and returns the error directly — no compensating cleanup needed in the
// caller because no ring buffer or goroutines are live yet.
func StartRemoteSession(client *ssh.Client, taskID, command, workdir string, rows, cols uint16, env map[string]string) (*RemoteSession, error) {
	return startRemoteSession(client, startInput{
		taskID:  taskID,
		command: command,
		workdir: workdir,
		rows:    rows,
		cols:    cols,
		envVars: env,
	})
}

func startRemoteSession(client *ssh.Client, in startInput) (*RemoteSession, error) {
	if in.rows == 0 {
		in.rows = agent.DefaultTermRows
	}
	if in.cols == 0 {
		in.cols = agent.DefaultTermCols
	}

	sess, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("exedev: new session: %w", err)
	}

	// Request a real PTY. Without this, agents that probe isatty (Claude,
	// Codex) fall back to dumb-terminal mode and never paint a status bar —
	// the remote is the only place this matters because `creack/pty` always
	// allocates a real PTY locally.
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := sess.RequestPty("xterm-256color", int(in.rows), int(in.cols), modes); err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("exedev: request pty: %w", err)
	}

	stdin, err := sess.StdinPipe()
	if err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("exedev: stdin pipe: %w", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("exedev: stdout pipe: %w", err)
	}

	// Build the wrapper command. We execute through `bash -lc` so that the
	// remote login shell rc files run (PATH, nvm, asdf, …) — the exe.dev
	// VMs are vanilla Linux and almost everyone keeps tool paths in
	// .bashrc/.profile. Without -l, claude/codex/etc. won't be on PATH.
	wrapped := buildRemoteCommand(in.workdir, in.command, in.envVars)
	if err := sess.Start(wrapped); err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("exedev: start: %w", err)
	}

	rs := &RemoteSession{
		taskID:   in.taskID,
		client:   client,
		session:  sess,
		stdin:    stdin,
		workdir:  in.workdir,
		buf:      agent.NewRingBuffer(defaultBufSize),
		done:     make(chan struct{}),
		ptyCols:  in.cols,
		ptyRows:  in.rows,
		initCols: in.cols,
		initRows: in.rows,
	}

	go rs.readLoop(stdout)
	go rs.waitLoop()
	return rs, nil
}

// buildRemoteCommand assembles the wrapper executed by `bash -lc`. Env is
// injected via `export` so values containing spaces or quotes are passed
// through `shellQuote` exactly once.
func buildRemoteCommand(workdir, command string, env map[string]string) string {
	var b strings.Builder
	for k, v := range env {
		b.WriteString("export ")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(shellQuote(v))
		b.WriteString("; ")
	}
	if workdir != "" {
		b.WriteString("cd ")
		b.WriteString(shellQuote(workdir))
		b.WriteString(" && ")
	}
	b.WriteString(command)
	// `bash -lc` runs the script as a login shell so .bashrc/.profile
	// populate PATH. We can't use -ic — ssh PTY is not a login terminal
	// in the bash sense, and -i would print job-control banners.
	return "bash -lc " + shellQuote(b.String())
}

// readLoop is the sole reader of the SSH stdout. Mirrors agent.Session's
// readLoop: write to ring buffer, tee to attached writers, drop writers
// that return errors.
func (s *RemoteSession) readLoop(stdout io.Reader) {
	tmp := make([]byte, 4096)
	for {
		n, err := stdout.Read(tmp)
		if n > 0 {
			data := tmp[:n]
			s.mu.Lock()
			s.buf.Write(data)
			s.lastOutput = time.Now()
			var ws []io.Writer
			if len(s.writers) > 0 {
				ws = make([]io.Writer, len(s.writers))
				copy(ws, s.writers)
			}
			s.mu.Unlock()
			var failed []io.Writer
			for _, w := range ws {
				if _, werr := w.Write(data); werr != nil {
					failed = append(failed, w)
				}
			}
			if len(failed) > 0 {
				s.mu.Lock()
				for _, f := range failed {
					s.removeWriterLocked(f)
				}
				s.mu.Unlock()
			}
		}
		if err != nil {
			return
		}
	}
}

// waitLoop blocks until the remote process exits, then marks the session
// closed and signals Done.
func (s *RemoteSession) waitLoop() {
	err := s.session.Wait()
	s.mu.Lock()
	s.err = err
	s.closed = true
	s.mu.Unlock()
	_ = s.session.Close()
	close(s.done)
}

// PID returns 0 — SSH does not expose the remote child PID. Callers that
// only use PID for display tolerate 0 (it shows as "—" in the TUI).
func (s *RemoteSession) PID() int { return 0 }

// WriteInput writes raw bytes to the remote stdin. Records lastInput on
// success only, matching agent.Session semantics.
func (s *RemoteSession) WriteInput(p []byte) (int, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, ErrSessionClosed
	}
	s.mu.Unlock()
	n, err := s.stdin.Write(p)
	if err == nil {
		s.mu.Lock()
		s.lastInput = time.Now()
		s.mu.Unlock()
	}
	return n, err
}

// Resize changes the remote PTY window size. SSH calls this WindowChange.
func (s *RemoteSession) Resize(rows, cols uint16) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.ptyCols = cols
	s.ptyRows = rows
	s.mu.Unlock()
	return s.session.WindowChange(int(rows), int(cols))
}

func (s *RemoteSession) RecentOutput() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Bytes()
}

func (s *RemoteSession) RecentOutputTail(n int) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Tail(n)
}

func (s *RemoteSession) RecentOutputTailWithTotal(n int) (tail []byte, total uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Tail(n), s.buf.TotalWritten()
}

func (s *RemoteSession) TotalWritten() uint64 { return s.buf.TotalWritten() }

func (s *RemoteSession) IsIdle() bool {
	if !s.Alive() {
		return false
	}
	s.mu.Lock()
	last := s.lastOutput
	s.mu.Unlock()
	if last.IsZero() {
		return false
	}
	return time.Since(last) >= idleThreshold
}

func (s *RemoteSession) LastInput() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastInput
}

func (s *RemoteSession) Alive() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

func (s *RemoteSession) PTYSize() (cols, rows int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int(s.ptyCols), int(s.ptyRows)
}

func (s *RemoteSession) InitialPTYSize() (cols, rows int) {
	return int(s.initCols), int(s.initRows)
}

func (s *RemoteSession) Done() <-chan struct{} { return s.done }

func (s *RemoteSession) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// WorkDir returns the remote workspace path. Callers that interpret this
// must check for the "exedev://" scheme before treating it as a local path.
func (s *RemoteSession) WorkDir() string { return s.workdir }

// Stop sends SIGTERM via SSH "signal" request. exe.dev's sshd honors this
// for the foreground process group; if not, we fall back to closing stdin
// which the agent CLIs read as EOF and exit cleanly.
func (s *RemoteSession) Stop() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	if err := s.session.Signal(ssh.SIGTERM); err == nil {
		return nil
	}
	// Some sshd builds reject signals over a Session channel. Closing stdin
	// lets the agent CLIs see EOF and exit; if even that fails, close the
	// whole session as the last resort.
	if err := s.stdin.Close(); err == nil {
		return nil
	}
	return s.session.Close()
}

func (s *RemoteSession) AddWriter(w io.Writer) {
	s.mu.Lock()
	replay := s.buf.Bytes()
	s.mu.Unlock()
	if len(replay) > 0 {
		w.Write(replay) //nolint:errcheck
	}
	s.mu.Lock()
	s.writers = append(s.writers, w)
	s.mu.Unlock()
}

func (s *RemoteSession) AddWriterFrom(w io.Writer, offset uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	currentTotal := s.buf.TotalWritten()
	if offset < currentTotal {
		gap := currentTotal - offset
		var replay []byte
		if gap > uint64(s.buf.Len()) { //nolint:gosec
			replay = s.buf.Bytes()
		} else {
			replay = s.buf.Tail(int(gap)) //nolint:gosec
		}
		if len(replay) > 0 {
			if _, err := w.Write(replay); err != nil {
				return
			}
		}
	}
	s.writers = append(s.writers, w)
}

func (s *RemoteSession) RemoveWriter(w io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeWriterLocked(w)
}

func (s *RemoteSession) removeWriterLocked(w io.Writer) {
	for i, existing := range s.writers {
		if existing == w {
			s.writers = append(s.writers[:i], s.writers[i+1:]...)
			return
		}
	}
}

// Compile-time interface check. Because exedev imports agent, the assertion
// proves the parallel surface stays in lockstep with future SessionHandle
// changes.
var _ agent.SessionHandle = (*RemoteSession)(nil)
