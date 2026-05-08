package agent

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/drn/argus/internal/config"
	"github.com/drn/argus/internal/model"
)

// stubProvider records call sites for routing assertions. It implements
// SessionProvider with the bare minimum surface — no real PTY, no goroutines.
type stubProvider struct {
	name     string
	starts   []string // taskIDs that asked Start
	stops    []string
	sessions map[string]*stubSession
}

func newStubProvider(name string) *stubProvider {
	return &stubProvider{name: name, sessions: map[string]*stubSession{}}
}

func (p *stubProvider) Start(task *model.Task, _ config.Config, _, _ uint16, _ bool) (SessionHandle, error) {
	p.starts = append(p.starts, task.ID)
	h := &stubSession{taskID: task.ID, done: make(chan struct{})}
	p.sessions[task.ID] = h
	return h, nil
}
func (p *stubProvider) Stop(taskID string) error {
	p.stops = append(p.stops, taskID)
	if h, ok := p.sessions[taskID]; ok {
		close(h.done)
		delete(p.sessions, taskID)
		return nil
	}
	return ErrSessionNotFound
}
func (p *stubProvider) StopAll() {
	for id := range p.sessions {
		_ = p.Stop(id)
	}
}
func (p *stubProvider) Get(taskID string) SessionHandle {
	if h, ok := p.sessions[taskID]; ok {
		return h
	}
	return nil
}
func (p *stubProvider) Running() []string {
	out := make([]string, 0, len(p.sessions))
	for id := range p.sessions {
		out = append(out, id)
	}
	return out
}
func (p *stubProvider) Idle() []string                    { return nil }
func (p *stubProvider) RunningAndIdle() ([]string, []string) {
	return p.Running(), nil
}
func (p *stubProvider) HasSession(taskID string) bool {
	_, ok := p.sessions[taskID]
	return ok
}
func (p *stubProvider) WorkDir(taskID string) string {
	if h, ok := p.sessions[taskID]; ok {
		return "/wd/" + h.taskID
	}
	return ""
}

type stubSession struct {
	taskID string
	done   chan struct{}
}

func (s *stubSession) PID() int                                       { return 0 }
func (s *stubSession) WriteInput([]byte) (int, error)                  { return 0, errors.New("nope") }
func (s *stubSession) Resize(uint16, uint16) error                     { return nil }
func (s *stubSession) RecentOutput() []byte                            { return nil }
func (s *stubSession) RecentOutputTail(int) []byte                     { return nil }
func (s *stubSession) RecentOutputTailWithTotal(int) ([]byte, uint64)  { return nil, 0 }
func (s *stubSession) TotalWritten() uint64                            { return 0 }
func (s *stubSession) IsIdle() bool                                    { return false }
func (s *stubSession) LastInput() time.Time                            { return time.Time{} }
func (s *stubSession) Alive() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}
func (s *stubSession) PTYSize() (int, int)                  { return 80, 24 }
func (s *stubSession) InitialPTYSize() (int, int)           { return 80, 24 }
func (s *stubSession) Done() <-chan struct{}                { return s.done }
func (s *stubSession) Err() error                           { return nil }
func (s *stubSession) WorkDir() string                      { return "/wd/" + s.taskID }
func (s *stubSession) Stop() error                          { close(s.done); return nil }
func (s *stubSession) AddWriter(io.Writer)                  {}
func (s *stubSession) AddWriterFrom(io.Writer, uint64)      {}
func (s *stubSession) RemoveWriter(io.Writer)               {}

func TestRouter_LocalTaskGoesToLocal(t *testing.T) {
	local := newStubProvider("local")
	remote := newStubProvider("remote")
	r := NewRuntimeRouter(local, remote)

	task := &model.Task{ID: "t1", Runtime: model.RuntimeLocal}
	if _, err := r.Start(task, config.Config{}, 24, 80, false); err != nil {
		t.Fatal(err)
	}
	if len(local.starts) != 1 || local.starts[0] != "t1" {
		t.Fatalf("local.starts=%v want [t1]", local.starts)
	}
	if len(remote.starts) != 0 {
		t.Fatalf("remote should not have been touched: %v", remote.starts)
	}
}

func TestRouter_RemoteTaskGoesToRemote(t *testing.T) {
	local := newStubProvider("local")
	remote := newStubProvider("remote")
	r := NewRuntimeRouter(local, remote)

	task := &model.Task{ID: "t2", Runtime: model.RuntimeExeDev, RemoteHost: "primary"}
	if _, err := r.Start(task, config.Config{}, 24, 80, false); err != nil {
		t.Fatal(err)
	}
	if len(remote.starts) != 1 || remote.starts[0] != "t2" {
		t.Fatalf("remote.starts=%v want [t2]", remote.starts)
	}
	if len(local.starts) != 0 {
		t.Fatalf("local should not have been touched: %v", local.starts)
	}
}

func TestRouter_RemoteTaskWithoutProvider(t *testing.T) {
	local := newStubProvider("local")
	r := NewRuntimeRouter(local, nil)

	task := &model.Task{ID: "tx", Runtime: model.RuntimeExeDev}
	_, err := r.Start(task, config.Config{}, 24, 80, false)
	if err == nil {
		t.Fatal("expected error when remote provider is nil")
	}
	if !errors.Is(err, ErrNoRemoteProvider) {
		t.Fatalf("expected ErrNoRemoteProvider, got %v", err)
	}
}

func TestRouter_StopRoutesByOwnership(t *testing.T) {
	local := newStubProvider("local")
	remote := newStubProvider("remote")
	r := NewRuntimeRouter(local, remote)

	r.Start(&model.Task{ID: "L", Runtime: model.RuntimeLocal}, config.Config{}, 24, 80, false)         //nolint:errcheck
	r.Start(&model.Task{ID: "R", Runtime: model.RuntimeExeDev}, config.Config{}, 24, 80, false) //nolint:errcheck

	if err := r.Stop("L"); err != nil {
		t.Fatal(err)
	}
	if err := r.Stop("R"); err != nil {
		t.Fatal(err)
	}
	if len(local.stops) != 1 || local.stops[0] != "L" {
		t.Fatalf("local.stops=%v", local.stops)
	}
	if len(remote.stops) != 1 || remote.stops[0] != "R" {
		t.Fatalf("remote.stops=%v", remote.stops)
	}
}

func TestRouter_RunningAggregates(t *testing.T) {
	local := newStubProvider("local")
	remote := newStubProvider("remote")
	r := NewRuntimeRouter(local, remote)

	r.Start(&model.Task{ID: "L1", Runtime: model.RuntimeLocal}, config.Config{}, 24, 80, false)         //nolint:errcheck
	r.Start(&model.Task{ID: "L2", Runtime: model.RuntimeLocal}, config.Config{}, 24, 80, false)         //nolint:errcheck
	r.Start(&model.Task{ID: "R1", Runtime: model.RuntimeExeDev}, config.Config{}, 24, 80, false) //nolint:errcheck

	got := r.Running()
	if len(got) != 3 {
		t.Fatalf("Running()=%v, want 3 items", got)
	}
}

func TestRouter_StopAllStopsBoth(t *testing.T) {
	local := newStubProvider("local")
	remote := newStubProvider("remote")
	r := NewRuntimeRouter(local, remote)

	r.Start(&model.Task{ID: "L1", Runtime: model.RuntimeLocal}, config.Config{}, 24, 80, false)         //nolint:errcheck
	r.Start(&model.Task{ID: "R1", Runtime: model.RuntimeExeDev}, config.Config{}, 24, 80, false) //nolint:errcheck

	r.StopAll()
	if len(local.stops) != 1 {
		t.Fatalf("local.stops=%v", local.stops)
	}
	if len(remote.stops) != 1 {
		t.Fatalf("remote.stops=%v", remote.stops)
	}
}
