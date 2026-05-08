package exedev

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// fakeSSHServer is a minimal in-process SSH server used for unit tests.
// It accepts any client key, runs `bash -c <cmd>` for exec requests, and
// hooks up the channel pipes to either the bash process's stdio or to a
// pty (when the client requested one).
//
// It deliberately does NOT spawn a real PTY (that would require creack/pty
// in tests too). Instead, when a PTY is requested, we run `cat` and pipe
// stdin↔stdout — enough to verify byte plumbing through the SSH layer
// without depending on bash being installed.
type fakeSSHServer struct {
	t        *testing.T
	listener net.Listener
	hostKey  ssh.Signer

	wg        sync.WaitGroup
	closing   chan struct{}
	closeOnce sync.Once

	// commandFn returns the os/exec.Cmd to run for a given exec request. If
	// nil (default), `cat` is used so PTY sessions echo back stdin.
	commandFn func(req string) *exec.Cmd
}

func newFakeSSHServer(t *testing.T) *fakeSSHServer {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	s := &fakeSSHServer{
		t:        t,
		listener: ln,
		hostKey:  signer,
		closing:  make(chan struct{}),
	}
	t.Cleanup(s.Close)
	s.wg.Add(1)
	go s.acceptLoop()
	return s
}

func (s *fakeSSHServer) Addr() string { return s.listener.Addr().String() }

func (s *fakeSSHServer) HostKey() ssh.PublicKey { return s.hostKey.PublicKey() }

func (s *fakeSSHServer) Close() {
	s.closeOnce.Do(func() {
		close(s.closing)
		_ = s.listener.Close()
	})
	s.wg.Wait()
}

func (s *fakeSSHServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.closing:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.t.Logf("fakeSSHServer accept: %v", err)
			return
		}
		s.wg.Add(1)
		go s.handle(conn)
	}
}

func (s *fakeSSHServer) handle(nConn net.Conn) {
	defer s.wg.Done()
	defer nConn.Close()

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(s.hostKey)

	_, chans, reqs, err := ssh.NewServerConn(nConn, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)

	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		ch, in, err := newCh.Accept()
		if err != nil {
			continue
		}
		s.wg.Add(1)
		go s.serveSession(ch, in)
	}
}

func (s *fakeSSHServer) serveSession(ch ssh.Channel, in <-chan *ssh.Request) {
	defer s.wg.Done()

	hasPty := false
	stopCh := make(chan struct{})
	stopOnce := sync.Once{}
	stop := func() {
		stopOnce.Do(func() { close(stopCh) })
	}
	for req := range in {
		switch req.Type {
		case "pty-req":
			hasPty = true
			_ = req.Reply(true, nil)
		case "window-change":
			_ = req.Reply(true, nil)
		case "shell":
			_ = req.Reply(true, nil)
		case "exec":
			cmdStr := parsePayloadString(req.Payload)
			_ = req.Reply(true, nil)
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.runExec(ch, cmdStr, hasPty, stopCh)
			}()
		case "signal":
			_ = req.Reply(true, nil)
			stop()
		default:
			_ = req.Reply(false, nil)
		}
	}
	stop()
}

// runExec runs the requested command. We don't actually invoke a shell —
// instead, the test server interprets the command string for the handful of
// patterns RemoteSession uses (bash -lc '<inner>') and synthesizes the right
// behavior with cat / printf / true. This keeps the tests hermetic against
// the host environment.
//
// stopCh fires when the client sends a signal (e.g. SIGTERM via Stop()) or
// closes the request channel. The loopback exits cleanly so tests waiting
// on Done() don't time out.
func (s *fakeSSHServer) runExec(ch ssh.Channel, cmd string, hasPty bool, stopCh <-chan struct{}) {
	defer func() { _ = ch.Close() }()
	defer func() {
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
	}()

	if s.commandFn != nil {
		c := s.commandFn(cmd)
		if c != nil {
			s.runOSCmd(ch, c, stopCh)
			return
		}
	}

	if hasPty {
		// Loopback: read stdin → write stdout. Bail when the client sends
		// a signal request (Stop) by closing the channel from another
		// goroutine — the io.Copy unblocks on EOF/closed channel.
		done := make(chan struct{})
		go func() {
			_, _ = io.Copy(ch, ch)
			close(done)
		}()
		select {
		case <-done:
		case <-stopCh:
			ch.CloseWrite() //nolint:errcheck
		}
		return
	}
	_, _ = ch.Write([]byte("ok\n"))
}

func (s *fakeSSHServer) runOSCmd(ch ssh.Channel, c *exec.Cmd, stopCh <-chan struct{}) {
	c.Stdin = ch
	c.Stdout = ch
	c.Stderr = ch.Stderr()
	if err := c.Start(); err != nil {
		ch.Write([]byte(err.Error())) //nolint:errcheck
		return
	}
	doneCh := make(chan struct{})
	go func() {
		c.Wait() //nolint:errcheck
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-stopCh:
		_ = c.Process.Kill()
		<-doneCh
	}
}

// parsePayloadString extracts the command string from an SSH exec / subsystem
// request payload (4-byte length-prefixed string). Returns the payload as-is
// if it's too short to parse (best-effort for tests).
func parsePayloadString(b []byte) string {
	if len(b) < 4 {
		return string(b)
	}
	n := int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3])
	if 4+n > len(b) {
		return string(b)
	}
	return string(b[4 : 4+n])
}

// writeTempEd25519Key writes a freshly-generated ed25519 private key to a
// temp file (PEM-encoded, OpenSSH format unsupported) and returns the path.
// Used by Provider tests that exercise the full Dial path with a host
// config pointing at a real on-disk identity file.
func writeTempEd25519Key(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	pemEncoded := pem.EncodeToMemory(pemBytes)
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pemEncoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// dialFakeServer returns an SSH client connected to s, ignoring host key
// verification (the server's key is trusted by virtue of being in-process).
func dialFakeServer(t *testing.T, s *fakeSSHServer) *ssh.Client {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ClientConfig{
		User:            "darren",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // in-process test server
		Timeout:         5 * time.Second,
	}
	c, err := ssh.Dial("tcp", s.Addr(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
