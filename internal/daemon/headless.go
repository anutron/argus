package daemon

import (
	"github.com/drn/argus/internal/agent"
	"github.com/drn/argus/internal/db"
	"github.com/drn/argus/internal/exedev"
	"github.com/drn/argus/internal/model"
)

// HeadlessCreateTask creates a task, its worktree, and starts an agent session
// without requiring a TUI. Used by the HTTP API and MCP server.
//
// Delegates to agent.CreateAndStart, which is fully transactional: any failure
// during worktree creation, DB insertion, or session start triggers LIFO
// cleanup of the preceding steps — so no orphan worktree, branch, or task row
// is left behind.
//
// autoName, when true, fires the post-creation Haiku rename. Callers should
// pass true iff name was synthesized from prompt (vs typed by a user or
// derived from a meaningful slug).
//
// BeforeStart/AfterStart hooks are intentionally nil — those are for the TUI's
// startGen tick-reconciliation counter, which has no analogue in headless mode.
func HeadlessCreateTask(database *db.DB, runner agent.SessionProvider, name, prompt, project, backend string, autoName bool) (*model.Task, error) {
	return HeadlessCreateTaskWithRuntime(database, runner, nil, HeadlessInput{
		Name:     name,
		Prompt:   prompt,
		Project:  project,
		Backend:  backend,
		AutoName: autoName,
	})
}

// HeadlessInput captures the optional runtime fields that the API/MCP layer
// pass through. The plain HeadlessCreateTask signature is preserved for
// backwards-compatible call sites — runtime defaults to local.
type HeadlessInput struct {
	Name       string
	Prompt     string
	Project    string
	Backend    string
	AutoName   bool
	Runtime    model.Runtime
	RemoteHost string
	BaseBranch string
}

// HeadlessCreateTaskWithRuntime is the runtime-aware sibling of
// HeadlessCreateTask. When the Runtime is exedev, it wires the daemon's
// exe.dev provider into Create/DestroyRemoteWorkspace so the same
// transactional semantics apply: a failed remote `git clone` rolls back
// the workspace just like a failed local `git worktree add`.
func HeadlessCreateTaskWithRuntime(database *db.DB, runner agent.SessionProvider, exedevProvider *exedev.Provider, in HeadlessInput) (*model.Task, error) {
	input := agent.CreateInput{
		Name:       in.Name,
		Prompt:     in.Prompt,
		Project:    in.Project,
		Backend:    in.Backend,
		AutoName:   in.AutoName,
		BaseBranch: in.BaseBranch,
		Runtime:    in.Runtime,
		RemoteHost: in.RemoteHost,
	}
	if in.Runtime == model.RuntimeExeDev {
		if exedevProvider == nil {
			return nil, agent.ErrNoRemoteProvider
		}
		client, err := exedevProvider.ClientFor(in.RemoteHost)
		if err != nil {
			return nil, err
		}
		// Resolve workspace root from the host config. Looked up here (not
		// captured in the closure) so a config edit between dial and
		// CreateAndStart picks up the new value.
		hostsCfg := database.Config().ExeDev.Hosts
		host := hostsCfg[in.RemoteHost]
		root := host.ResolvedWorkspaceRoot()

		input.CreateRemoteWorkspace = func(name, baseBranch string) (string, error) {
			// Repo cloning over SSH is a follow-up — for now the workspace
			// is a bare directory. The agent decides what to clone (if
			// anything) once it starts on the remote VM.
			return exedev.CreateWorkspace(client, root, name, "", baseBranch)
		}
		input.DestroyRemoteWorkspace = func(path string) error {
			return exedev.DestroyWorkspace(client, path)
		}
	}
	task, _, err := agent.CreateAndStart(database, runner, input)
	return task, err
}
