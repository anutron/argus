# Cloud Runtime via exe.dev

**Date:** 2026-05-08
**Status:** In progress
**Goal:** Allow Argus tasks to run in the cloud (on a persistent exe.dev VM via SSH) instead of always shelling a local agent against a local git worktree.

## Why

`exe.dev` provides persistent Linux VMs accessible over SSH ($20/mo, 8GB RAM, 25GB disk, up to 50 VMs). The cleanest cloud target is "SSH to a known host and run the agent there." That maps 1:1 onto the existing PTY model — `golang.org/x/crypto/ssh` exposes `Session.RequestPty`, `Session.WindowChange`, stdin `Writer`, stdout `Reader` — exactly the surface `internal/agent/Session` already uses against `creack/pty`. So the integration is "swap the transport, keep the abstraction."

## Decisions (from clarifying questions)

| Question | Answer |
|---|---|
| Where does the runtime choice live? | **Per-task toggle**. New-task form has a Local / exe.dev radio. |
| Worktree model | **Remote-only**. No local `git worktree add` for cloud tasks. The repo lives in `~/argus/<task>` on the VM. |
| Terminal pane | **Full PTY over SSH**. SSH gives us PTY, resize, stdin write, raw stdout — same surface as local. |

## Architecture

```
                              ┌──────────────────────────┐
TUI / API / MCP ─────────────▶│  RuntimeRouter           │
                              │  (SessionProvider)       │
                              └────┬───────────┬─────────┘
                                   │           │
                  task.Runtime == "local"   task.Runtime == "exedev"
                                   │           │
                                   ▼           ▼
                       ┌──────────────┐    ┌──────────────────────┐
                       │ agent.Runner │    │ exedev.Provider      │
                       │ (PTY local)  │    │ (SSH PTY remote)     │
                       └──────────────┘    └──────────────────────┘
```

- `model.Runtime` enum (`local` | `exedev`), persisted on `Task`.
- `internal/exedev/` ships:
  - `Provider` — implements `agent.SessionProvider`. Owns the SSH client pool keyed by host.
  - `RemoteSession` — implements `agent.SessionHandle`. Wraps an SSH session with a remote PTY, populates a local `agent.RingBuffer` from the SSH stdout pipe.
  - `Workspace` — bootstrap a remote working directory: `mkdir -p <root>/<task>` and (if a repo URL is supplied) `git clone --branch …`. Returns a workspace ID = the directory path on the VM.
- `internal/agent/router.go` — `RuntimeRouter` dispatches `SessionProvider` calls per-task by `Runtime`. Daemon owns one router instead of a bare `Runner`.
- `agent.CreateAndStart` branches on `task.Runtime`: local path stays untouched; exedev path skips `CreateWorktree` and calls `exedev.Provider.CreateWorkspace` instead, with a compensating `DestroyWorkspace` cleanup registered on the same LIFO stack.
- `config.ExeDevConfig`: list of remote hosts with `User`, `Host`, `IdentityFile` (default `~/.ssh/id_ed25519`), `WorkspaceRoot` (default `~/argus`). Persisted in DB `config` table as JSON. Editable from Settings — minimal UI for now (read-only display + path to JSON in DB), with TODO follow-up for full CRUD.
- New-task form (TUI) gets a "Runtime" row; PWA form gets a runtime select. Default = `local`. If exe.dev is selected, an exe.dev host must exist in config.

## Out of scope for this PR (follow-ups)

- **Remote git status / diff / file panels** — `internal/gitutil` currently shells locally. For exedev tasks the panels show "(remote — open in agent shell)" with a hint. Wire-up of remote git providers is its own PR.
- **Remote attachments / fork-context files** — no `OnWorktreeCreated` invocation for exedev tasks; the prompt does not get attachment paths appended (returns an error if attachments are supplied with exedev runtime).
- **Settings UI for exe.dev hosts** — config goes in the DB; the Settings tab gets a stub section.
- **PWA web-tests** — toggle is added but new-task form spec coverage is minimal.

## Implementation order

1. **Plan doc** (this file).
2. **Runtime model + DB migration** — add `runtime` and `remote_host` columns; default `local`.
3. **Config** — `ExeDevConfig` struct, DB persistence, default-on-empty.
4. **`internal/exedev/`** — package scaffold:
   - `client.go` — SSH dial with key-based auth.
   - `workspace.go` — Create/Destroy.
   - `session.go` — `RemoteSession` implementing `SessionHandle` (PTY over SSH, ring buffer, reader goroutine).
   - `provider.go` — `Provider` implementing `SessionProvider`.
   - In-process tests via `golang.org/x/crypto/ssh` server-side.
5. **`agent.RuntimeRouter`** — wraps `Runner` + `exedev.Provider`. Tests prove local path is untouched.
6. **Branch `CreateAndStart`** — skip worktree, call exedev workspace. LIFO cleanup. Tests.
7. **TUI new-task form toggle** — radio. PWA form gets a select.
8. **README + gotchas** — add `context/knowledge/gotchas/exedev.md`.
9. **`make test` + `make vet` + `make lint-pr`** — green before PR.
10. **`/pr`**.

## Non-obvious invariants (will land in `gotchas/exedev.md`)

- **SSH session = PTY, not channel**. Always `RequestPty` + `Shell` (or `Start` of the agent command). A bare exec channel doesn't deliver `WindowChange` → no agent resize.
- **stdin is a `Writer`, not a file**. Don't try to `Setsize` the SSH session — call `Session.WindowChange(rows, cols)`.
- **Exit detection is async**. SSH `Session.Wait` returns when remote exits; mirror the local `waitLoop` shape.
- **Workspace path = `task.Worktree`**, scheme `exedev://<host>/<path>`. Code that string-prefixes paths must check the scheme.
- **Reconnection is the daemon's problem, not the session's**. If SSH drops mid-task, the session goes Done with an error — same as a local crash. A reconnection layer is a follow-up.
- **Auth = key files only**. Password prompts have no place in the daemon. If `IdentityFile` doesn't exist or is wrong, the task fails to start with a clear error.
