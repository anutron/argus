package exedev

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/drn/argus/internal/config"
	"github.com/drn/argus/internal/model"
	"github.com/drn/argus/internal/testutil"
	"golang.org/x/crypto/ssh"
)

// providerWithFakeSSH builds a Provider whose dial path goes through the
// in-process fake SSH server. The trick: we point dialFn at a custom dialer
// that ignores the host key file entirely so we don't have to plumb a
// known_hosts or identity file through the test.
func providerWithFakeSSH(t *testing.T, srv *fakeSSHServer, onFinish func(string, error, bool, []byte)) *Provider {
	t.Helper()

	prevDial := dialFn
	prevHK := HostKeyCallback
	t.Cleanup(func() {
		dialFn = prevDial
		HostKeyCallback = prevHK
	})

	HostKeyCallback = func() (ssh.HostKeyCallback, error) {
		return ssh.InsecureIgnoreHostKey(), nil //nolint:gosec // test
	}
	dialFn = func(network, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
		return ssh.Dial(network, srv.Addr(), cfg)
	}

	// Provider needs an identity file. Write a temp one and point the host
	// config at it. Any private key works because the fake server accepts
	// every public key.
	keyPath := writeTempEd25519Key(t)

	hostsFn := func() map[string]config.ExeDevHost {
		return map[string]config.ExeDevHost{
			"primary": {
				Host:         "ignored",
				User:         "darren",
				IdentityFile: keyPath,
			},
		}
	}
	p := NewProvider(hostsFn, onFinish)
	t.Cleanup(p.Close)
	return p
}

func TestProvider_StartGetStop(t *testing.T) {
	srv := newFakeSSHServer(t)

	exited := make(chan struct{})
	var lastTask atomic.Value
	p := providerWithFakeSSH(t, srv, func(taskID string, _ error, _ bool, _ []byte) {
		lastTask.Store(taskID)
		close(exited)
	})

	task := &model.Task{
		ID:         "t1",
		Name:       "echo task",
		Runtime:    model.RuntimeExeDev,
		RemoteHost: "primary",
		Worktree:   "/tmp",
		Backend:    "claude",
	}
	cfg := config.DefaultConfig()
	cfg.Backends["claude"] = config.Backend{Command: "cat"}

	handle, err := p.Start(task, cfg, 24, 80, false)
	testutil.NoError(t, err)
	if handle == nil {
		t.Fatal("nil handle")
	}

	if !p.HasSession("t1") {
		t.Fatal("HasSession(t1) = false after Start")
	}
	if got := p.Get("t1"); got == nil {
		t.Fatal("Get(t1) = nil after Start")
	}

	running := p.Running()
	testutil.Equal(t, len(running), 1)
	testutil.Equal(t, running[0], "t1")

	if err := p.Stop("t1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("onFinish never fired")
	}

	if got := lastTask.Load(); got != "t1" {
		t.Fatalf("onFinish task = %v, want t1", got)
	}
	// After exit, the session is removed from the map.
	if p.HasSession("t1") {
		t.Fatal("session should be gone after exit")
	}
}

func TestProvider_StartRejectsLocalRuntime(t *testing.T) {
	srv := newFakeSSHServer(t)
	p := providerWithFakeSSH(t, srv, nil)
	task := &model.Task{ID: "tx", Runtime: model.RuntimeLocal}
	_, err := p.Start(task, config.DefaultConfig(), 24, 80, false)
	if err == nil {
		t.Fatal("expected error for local runtime")
	}
}

func TestProvider_StartRejectsUnknownHost(t *testing.T) {
	srv := newFakeSSHServer(t)
	p := providerWithFakeSSH(t, srv, nil)
	task := &model.Task{
		ID:         "ty",
		Runtime:    model.RuntimeExeDev,
		RemoteHost: "no-such-host",
		Worktree:   "/tmp",
	}
	_, err := p.Start(task, config.DefaultConfig(), 24, 80, false)
	if err == nil {
		t.Fatal("expected error for unknown host")
	}
}

func TestProvider_DuplicateStartFails(t *testing.T) {
	srv := newFakeSSHServer(t)
	p := providerWithFakeSSH(t, srv, nil)
	task := &model.Task{
		ID:         "td",
		Runtime:    model.RuntimeExeDev,
		RemoteHost: "primary",
		Worktree:   "/tmp",
		Backend:    "claude",
	}
	cfg := config.DefaultConfig()
	cfg.Backends["claude"] = config.Backend{Command: "cat"}

	if _, err := p.Start(task, cfg, 24, 80, false); err != nil {
		t.Fatal(err)
	}
	defer p.Stop("td") //nolint:errcheck

	if _, err := p.Start(task, cfg, 24, 80, false); err == nil {
		t.Fatal("expected duplicate Start to fail")
	}
}

func TestProvider_StopAllAndClose(t *testing.T) {
	srv := newFakeSSHServer(t)
	p := providerWithFakeSSH(t, srv, nil)

	cfg := config.DefaultConfig()
	cfg.Backends["claude"] = config.Backend{Command: "cat"}
	for _, id := range []string{"a", "b", "c"} {
		task := &model.Task{
			ID:         id,
			Runtime:    model.RuntimeExeDev,
			RemoteHost: "primary",
			Worktree:   "/tmp",
			Backend:    "claude",
		}
		if _, err := p.Start(task, cfg, 24, 80, false); err != nil {
			t.Fatalf("start %s: %v", id, err)
		}
	}
	if got := len(p.Running()); got != 3 {
		t.Fatalf("Running()=%d, want 3", got)
	}
	p.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(p.Running()) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := len(p.Running()); got != 0 {
		t.Fatalf("Running()=%d after Close, want 0", got)
	}
}
