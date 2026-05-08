package agent

import (
	"github.com/drn/argus/internal/config"
	"github.com/drn/argus/internal/model"
)

// RuntimeRouter dispatches SessionProvider calls to the right backing
// provider based on each task's Runtime. The local runner stays the only
// path for `RuntimeLocal` tasks; the remote provider is consulted only when
// a task explicitly opts into `RuntimeExeDev`.
//
// The router itself implements SessionProvider, so daemon and HTTP-API code
// holds it behind the same interface — no surface change for callers.
//
// remote may be nil. In that case any call that would touch a remote
// runtime returns an error, and the router behaves identically to the local
// runner. This is the configuration used when no exe.dev hosts are
// configured, keeping local-only deployments byte-identical to before.
type RuntimeRouter struct {
	local  SessionProvider
	remote SessionProvider
}

// NewRuntimeRouter builds a router. local is required; remote may be nil.
func NewRuntimeRouter(local, remote SessionProvider) *RuntimeRouter {
	return &RuntimeRouter{local: local, remote: remote}
}

// providerForTask picks the provider based on the task's runtime. Returns
// nil for RuntimeExeDev when the router has no remote provider configured —
// callers translate that to a clear "configure exe.dev first" error.
func (r *RuntimeRouter) providerForTask(t *model.Task) SessionProvider {
	if t == nil || t.Runtime == model.RuntimeLocal {
		return r.local
	}
	return r.remote
}

// providerForTaskID is the post-Start lookup variant. Each provider tracks
// its own session set; we query both to find which one owns the task.
// Local-runtime tasks should always live in the local runner, but the dual
// lookup defends against config drift (e.g., a task created with one
// runtime and Start-failed, with no remote provider configured).
func (r *RuntimeRouter) providerForTaskID(taskID string) SessionProvider {
	if r.local.HasSession(taskID) {
		return r.local
	}
	if r.remote != nil && r.remote.HasSession(taskID) {
		return r.remote
	}
	return nil
}

func (r *RuntimeRouter) Start(task *model.Task, cfg config.Config, rows, cols uint16, resume bool) (SessionHandle, error) {
	p := r.providerForTask(task)
	if p == nil {
		return nil, ErrNoRemoteProvider
	}
	return p.Start(task, cfg, rows, cols, resume)
}

func (r *RuntimeRouter) Stop(taskID string) error {
	if p := r.providerForTaskID(taskID); p != nil {
		return p.Stop(taskID)
	}
	return ErrSessionNotFound
}

func (r *RuntimeRouter) StopAll() {
	r.local.StopAll()
	if r.remote != nil {
		r.remote.StopAll()
	}
}

func (r *RuntimeRouter) Get(taskID string) SessionHandle {
	if p := r.providerForTaskID(taskID); p != nil {
		return p.Get(taskID)
	}
	return nil
}

func (r *RuntimeRouter) Running() []string {
	out := r.local.Running()
	if r.remote != nil {
		out = append(out, r.remote.Running()...)
	}
	return out
}

func (r *RuntimeRouter) Idle() []string {
	out := r.local.Idle()
	if r.remote != nil {
		out = append(out, r.remote.Idle()...)
	}
	return out
}

func (r *RuntimeRouter) RunningAndIdle() (running, idle []string) {
	running, idle = r.local.RunningAndIdle()
	if r.remote != nil {
		rr, ri := r.remote.RunningAndIdle()
		running = append(running, rr...)
		idle = append(idle, ri...)
	}
	return running, idle
}

func (r *RuntimeRouter) HasSession(taskID string) bool {
	return r.providerForTaskID(taskID) != nil
}

func (r *RuntimeRouter) WorkDir(taskID string) string {
	if p := r.providerForTaskID(taskID); p != nil {
		return p.WorkDir(taskID)
	}
	return ""
}

var _ SessionProvider = (*RuntimeRouter)(nil)
