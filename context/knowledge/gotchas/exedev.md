# exe.dev runtime gotchas

Non-obvious invariants for the cloud runtime in `internal/exedev/`. The local
runtime is the source of truth; everything here describes how the remote
counterpart deviates and where that drift can bite.

## Runtime + worktree

- **`task.Runtime` is the discriminator, not `task.Worktree`'s prefix.**
  Remote tasks store the absolute remote path (e.g. `/home/darren/argus/foo-1`)
  on `Worktree` so the existing display surface (detail view, modal banners)
  shows it untouched. Code that operates on the local filesystem must gate
  on `task.IsRemote()` BEFORE statting / removing / globbing — the same path
  may exist locally by coincidence.
- **`Sandboxed` is always false for remote tasks.** `CreateAndStart` skips
  `IsTaskSandboxed` for remote runtime; the VM is the sandbox. A `Sandboxed:
true` row with `Runtime: exedev` is a corruption indicator.
- **Branch column is empty for remote tasks.** The remote workspace owns its
  own branch state and we don't track it locally. Anything that depends on
  `task.Branch` must check IsRemote() and skip or substitute.

## SSH session lifecycle

- **PTY first, then `Start`.** `Session.RequestPty` must precede
  `Session.Start`. Reversing the order silently produces a non-PTY session
  and `claude`/`codex` fall back to dumb-terminal mode (no status bar, no
  ANSI coloring).
- **`WindowChange`, not `Setsize`.** Resize on a remote session calls
  `ssh.Session.WindowChange(rows, cols)`. Calling `pty.Setsize` against the
  SSH session struct compiles but does nothing — the SSH library has no PTY
  master fd to ioctl.
- **`Stop()` cascades through three escape hatches.** First `Signal(SIGTERM)`,
  which exe.dev's sshd may reject; then `stdin.Close()` so the agent CLI
  reads EOF; finally `session.Close()`. Tests covering Stop use the in-
  process server, which honours signal requests by closing the loopback
  channel — keep that behavior in any future test-server refactor or the
  Stop tests will time out.
- **No reconnection.** A dropped SSH transport surfaces as `Done` with an
  error, identical to a local crash. Adding reconnect logic to the session
  layer is wrong — the daemon's `transitionTaskOnExit` is the right place.

## Workspace bootstrap

- **`CreateWorkspace` auto-suffixes name collisions.** Mirrors local
  `CreateWorktree` semantics (`-1`, `-2`, …) so two cloud tasks with the
  same name don't share a directory.
- **`DestroyWorkspace` refuses non-absolute paths and `/`.** A corrupted
  task row pointing at `~/` would otherwise drive `rm -rf ~/`. The check
  is in the function, not the caller — never bypass it.
- **`shellQuote` wraps every path interpolation.** Any future addition that
  builds a remote command line MUST go through `shellQuote` for filenames
  / branches / commands. Skipping it is a remote command-injection vector
  the moment a task name has a backtick or `;` in it.

## Auth & host keys

- **Auth is key-based, period.** No password fallback, no agent-forwarding.
  An exe.dev host config without `IdentityFile` defaults to
  `~/.ssh/id_ed25519`; if that file is absent or unreadable, the task
  fails to start with a clear error before any session goroutines spawn.
- **Host key verification reads `~/.ssh/known_hosts`.** Strict — unknown
  hosts are rejected. Bootstrap requires the user to `ssh <host>` once
  interactively before pointing Argus at the same host. Tests override the
  callback and `dialFn` via the package-level vars to bypass this.

## Provider & router

- **`exe.dev` provider is lazy.** `daemon.EnsureExeDevProvider()` instantiates
  it on first remote-task start. Vanilla local-only deployments never pay
  the SSH-client cache cost. The router is created up-front with `nil` for
  the remote slot — it tolerates `nil` and returns `ErrNoRemoteProvider`
  for any remote dispatch attempt.
- **SSH client cache is per-host, lifetime = provider.** Connections never
  auto-reconnect. `provider.Close()` (on daemon shutdown) tears them all
  down. A config edit that changes a host's address takes effect on the
  next `Start` for that host name only after the daemon restarts — there
  is no cache invalidation on edit.

## Test harness

- **The fake SSH server doesn't shell out.** `runExec` interprets bash -lc
  patterns and synthesizes behavior with goroutine-based echo loops, not
  by spawning `bash`. This keeps tests hermetic — they pass on minimal
  build images that don't have bash.
- **Provider tests must `t.Cleanup(p.Close)`.** Without it the SSH server's
  `chans` channel never closes (the cached client keeps the TCP open), and
  `fakeSSHServer.Close` blocks the test cleanup forever.
