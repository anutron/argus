package exedev

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/drn/argus/internal/agent"
	"github.com/drn/argus/internal/config"
	"github.com/drn/argus/internal/model"
	"golang.org/x/crypto/ssh"
)

// ErrNoHostConfigured is returned by Provider methods when a task asks for an
// exe.dev runtime but its RemoteHost is empty or unknown to the config.
var ErrNoHostConfigured = errors.New("exedev: no remote host configured")

// ErrSessionExists mirrors agent.Runner's "session already exists" semantics
// so callers can switch on identical errors regardless of runtime.
var ErrSessionExists = errors.New("exedev: session already exists for task")

// Provider implements agent.SessionProvider against exe.dev VMs over SSH.
//
// It owns:
//   - a small SSH client cache keyed by host name (one connection per host),
//     so successive task starts on the same VM reuse a single TCP/SSH handshake
//   - the live remote sessions keyed by task ID
//
// SSH clients are dialed lazily on first use and reused until torn down on
// Close. Connections never auto-reconnect — if the SSH transport drops mid-
// session, the affected RemoteSession marks itself Done with an error and
// the task surfaces the same way a local crash does.
type Provider struct {
	hostsFn  func() map[string]config.ExeDevHost
	onFinish func(taskID string, err error, stopped bool, lastOutput []byte)

	mu       sync.Mutex
	clients  map[string]*ssh.Client     // hostName → SSH client
	sessions map[string]*RemoteSession  // taskID → remote session
	stopped  map[string]bool            // taskIDs explicitly Stop()ed
	hostFor  map[string]string          // taskID → hostName (for Workspace destroy after exit)
}

// NewProvider builds a Provider. hostsFn is called on each Start so a config
// reload at runtime is picked up without tearing the Provider down.
//
// onFinish parallels agent.Runner's onFinish: fired in a goroutine when any
// remote session exits, with the captured ring buffer and "was-stopped" flag.
// Callers (the daemon) use this to flip the persisted task status.
func NewProvider(hostsFn func() map[string]config.ExeDevHost, onFinish func(taskID string, err error, stopped bool, lastOutput []byte)) *Provider {
	return &Provider{
		hostsFn:  hostsFn,
		onFinish: onFinish,
		clients:  make(map[string]*ssh.Client),
		sessions: make(map[string]*RemoteSession),
		stopped:  make(map[string]bool),
		hostFor:  make(map[string]string),
	}
}

// Start launches a remote agent session for task. The task's Runtime must be
// RuntimeExeDev and its RemoteHost must reference a host in the current
// config; otherwise ErrNoHostConfigured is returned.
func (p *Provider) Start(task *model.Task, cfg config.Config, rows, cols uint16, resume bool) (agent.SessionHandle, error) {
	if task.Runtime != model.RuntimeExeDev {
		return nil, fmt.Errorf("exedev: task %s is not an exe.dev runtime", task.ID)
	}
	host, ok := p.hostsFn()[task.RemoteHost]
	if !ok || host.Host == "" {
		return nil, fmt.Errorf("%w: %q", ErrNoHostConfigured, task.RemoteHost)
	}

	p.mu.Lock()
	if _, exists := p.sessions[task.ID]; exists {
		p.mu.Unlock()
		return nil, fmt.Errorf("%w %s", ErrSessionExists, task.ID)
	}
	p.sessions[task.ID] = nil // reservation
	p.mu.Unlock()

	cleanup := func() {
		p.mu.Lock()
		if p.sessions[task.ID] == nil {
			delete(p.sessions, task.ID)
		}
		p.mu.Unlock()
	}

	client, err := p.dialOrReuse(task.RemoteHost, host)
	if err != nil {
		cleanup()
		return nil, err
	}

	cmdStr, err := buildAgentCommand(task, cfg, host, resume)
	if err != nil {
		cleanup()
		return nil, err
	}

	slog.Info("exedev.Start", "task", task.ID, "host", task.RemoteHost, "workdir", task.Worktree, "resume", resume)

	rs, err := StartRemoteSession(client, task.ID, cmdStr, task.Worktree, rows, cols, nil)
	if err != nil {
		cleanup()
		return nil, err
	}

	p.mu.Lock()
	p.sessions[task.ID] = rs
	p.hostFor[task.ID] = task.RemoteHost
	p.mu.Unlock()

	go p.watchExit(task.ID, rs)
	return rs, nil
}

// dialOrReuse returns the cached client for hostName, dialing on first use.
// The same lock that guards `clients` also guards the dial — without it, two
// concurrent Starts on the same host would each open a fresh SSH connection
// and leak one of them when this method races to set the map entry.
func (p *Provider) dialOrReuse(hostName string, host config.ExeDevHost) (*ssh.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[hostName]; ok && c != nil {
		return c, nil
	}
	c, err := Dial(host)
	if err != nil {
		return nil, err
	}
	p.clients[hostName] = c
	return c, nil
}

// watchExit fires onFinish when the remote session is done.
func (p *Provider) watchExit(taskID string, rs *RemoteSession) {
	<-rs.Done()
	lastOutput := rs.RecentOutput()
	exitErr := rs.Err()

	p.mu.Lock()
	wasStopped := p.stopped[taskID]
	delete(p.stopped, taskID)
	p.mu.Unlock()

	slog.Info("exedev: session exited", "task", taskID, "err", exitErr, "stopped", wasStopped, "lastOutputBytes", len(lastOutput))

	if p.onFinish != nil {
		p.onFinish(taskID, exitErr, wasStopped, lastOutput)
	}

	p.mu.Lock()
	delete(p.sessions, taskID)
	delete(p.hostFor, taskID)
	p.mu.Unlock()
}

func (p *Provider) Stop(taskID string) error {
	p.mu.Lock()
	rs := p.sessions[taskID]
	if rs == nil {
		p.mu.Unlock()
		return agent.ErrSessionNotFound
	}
	p.stopped[taskID] = true
	p.mu.Unlock()
	return rs.Stop()
}

func (p *Provider) StopAll() {
	p.mu.Lock()
	ids := make([]string, 0, len(p.sessions))
	for id := range p.sessions {
		ids = append(ids, id)
	}
	p.mu.Unlock()
	for _, id := range ids {
		_ = p.Stop(id)
	}
}

func (p *Provider) Get(taskID string) agent.SessionHandle {
	p.mu.Lock()
	defer p.mu.Unlock()
	rs := p.sessions[taskID]
	if rs == nil {
		return nil
	}
	return rs
}

func (p *Provider) Running() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make([]string, 0, len(p.sessions))
	for id, rs := range p.sessions {
		if rs != nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func (p *Provider) Idle() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var ids []string
	for id, rs := range p.sessions {
		if rs != nil && rs.IsIdle() {
			ids = append(ids, id)
		}
	}
	return ids
}

func (p *Provider) RunningAndIdle() (running, idle []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	running = make([]string, 0, len(p.sessions))
	for id, rs := range p.sessions {
		if rs == nil {
			continue
		}
		running = append(running, id)
		if rs.IsIdle() {
			idle = append(idle, id)
		}
	}
	return running, idle
}

func (p *Provider) HasSession(taskID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.sessions[taskID]
	return ok
}

func (p *Provider) WorkDir(taskID string) string {
	if rs := p.Get(taskID); rs != nil {
		return rs.WorkDir()
	}
	return ""
}

// Close tears down all SSH clients and remote sessions. Used by the daemon
// on shutdown so connections aren't leaked.
func (p *Provider) Close() {
	p.StopAll()
	p.mu.Lock()
	clients := p.clients
	p.clients = map[string]*ssh.Client{}
	p.mu.Unlock()
	for name, c := range clients {
		if err := c.Close(); err != nil {
			slog.Warn("exedev: client close", "host", name, "err", err)
		}
	}
}

// ClientFor returns the cached SSH client for hostName (dialing if needed).
// Exposed for callers that need to bootstrap a workspace before Start —
// CreateWorkspace runs over the same client, but hostsFn is internal to the
// Provider, so callers go through this instead of Dial directly.
func (p *Provider) ClientFor(hostName string) (*ssh.Client, error) {
	host, ok := p.hostsFn()[hostName]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNoHostConfigured, hostName)
	}
	return p.dialOrReuse(hostName, host)
}

var _ agent.SessionProvider = (*Provider)(nil)
