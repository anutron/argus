package agent

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/drn/argus/internal/db"
	"github.com/drn/argus/internal/model"
)

// Attachment is a user-uploaded file to be written into the worktree before
// the agent starts. Name is the sanitized filename (no path components); Data
// is the raw bytes. Saved into <worktree>/.context/<name>.
type Attachment struct {
	Name string
	Data []byte
}

// CreateInput describes a new task to create and start.
//
// Only Name and Project are required. Backend defaults to cfg.Defaults.Backend.
// Rows/Cols default to 24/80 — agents auto-resize when the TUI attaches.
type CreateInput struct {
	Name       string
	Prompt     string
	Project    string
	Backend    string // optional; empty = cfg.Defaults.Backend
	PRURL      string // optional; set for review tasks
	BaseBranch string // optional; overrides projCfg.Branch for this task

	// Runtime selects local vs cloud (exe.dev) execution. Defaults to
	// RuntimeLocal so existing call sites stay byte-identical.
	Runtime model.Runtime
	// RemoteHost names the configured exe.dev host when Runtime==RuntimeExeDev.
	RemoteHost string
	// CreateRemoteWorkspace bootstraps a per-task working directory on the
	// remote host. Returns the absolute remote path that becomes Task.Worktree.
	// Required when Runtime==RuntimeExeDev.
	CreateRemoteWorkspace func(taskName, baseBranch string) (path string, err error)
	// DestroyRemoteWorkspace removes the remote workspace. Used as the
	// compensating cleanup for CreateRemoteWorkspace and on task delete.
	DestroyRemoteWorkspace func(path string) error

	// AutoName, when true, fires a fire-and-forget Haiku rename in a
	// background goroutine after the task is fully created. The DB write
	// is race-guarded: it only overwrites Name if the row's current Name
	// still equals the regex-derived slug. Callers should set this only
	// when Name was string-interpolated from Prompt (not user-typed and
	// not a structured slug like "review-pr-123-…" worth preserving).
	AutoName bool

	// Attachments are written to <worktree>/.context/<name> after worktree
	// creation but before the session starts, and their paths are appended
	// to Prompt so the agent sees them on first turn.
	Attachments []Attachment

	Rows uint16 // initial PTY rows (0 → 24)
	Cols uint16 // initial PTY cols (0 → 80)

	// OnWorktreeCreated runs after the worktree exists but before the task
	// row is persisted. Use this for writing per-worktree context files
	// (e.g. fork-task prompts). If it returns an error, the worktree is
	// removed and CreateAndStart returns the error.
	//
	// Runs on the calling goroutine — typically a background goroutine
	// spawned by the TUI. MUST NOT call tview widget methods or mutate
	// any state that assumes the tview main goroutine; those are data
	// races without QueueUpdateDraw.
	OnWorktreeCreated func(wtPath string) error

	// BeforeStart runs immediately before runner.Start. Used by the TUI to
	// bump its startGen counter so in-flight tick reconciliations see a new
	// generation.
	//
	// Runs on the calling goroutine (background, not tview main). Same
	// tview-safety caveat as OnWorktreeCreated applies.
	BeforeStart func()

	// AfterStart runs immediately after runner.Start returns, whether or
	// not it succeeded — callers must not assume the session is live when
	// this fires. On failure the unwind chain runs next. Used by the TUI
	// to post-bump startGen so ticks that captured runningIDs mid-RPC
	// skip stale reconciliation.
	//
	// Runs on the calling goroutine (background, not tview main). Same
	// tview-safety caveat as OnWorktreeCreated applies.
	AfterStart func()
}

// CreateAndStart is the single entry point for fully-transactional task
// creation. It performs, in order: resolve project config, create worktree,
// run OnWorktreeCreated hook, persist task row, generate session ID, and
// start the agent session. Each side-effecting step registers a compensating
// cleanup that runs in LIFO order if any later step fails — so a failure
// leaves no orphan worktree, no orphan branch, and no orphan DB row behind.
//
// Callers that need to react to the slow worktree step (e.g. show a spinner)
// should invoke CreateAndStart from a goroutine; its steps are mutex-safe and
// do not require the tview main goroutine.
//
// The returned task has Status=InProgress and AgentPID populated. The session
// is live and ready to be attached to a UI pane.
func CreateAndStart(database *db.DB, runner SessionProvider, input CreateInput) (*model.Task, SessionHandle, error) {
	cfg := database.Config()

	projCfg, ok := cfg.Projects[input.Project]
	if !ok {
		return nil, nil, fmt.Errorf("project %q not found in config", input.Project)
	}
	// Local runtime requires a configured project path; remote runtime does
	// its own bootstrap on the VM and tolerates an empty Path.
	if input.Runtime == model.RuntimeLocal && projCfg.Path == "" {
		return nil, nil, fmt.Errorf("project %q has no path configured", input.Project)
	}
	if input.Runtime == model.RuntimeExeDev {
		if input.RemoteHost == "" {
			return nil, nil, fmt.Errorf("exedev runtime requires RemoteHost")
		}
		if input.CreateRemoteWorkspace == nil || input.DestroyRemoteWorkspace == nil {
			return nil, nil, fmt.Errorf("exedev runtime requires Create/DestroyRemoteWorkspace callbacks")
		}
		if len(input.Attachments) > 0 || input.OnWorktreeCreated != nil {
			// Out of scope for the first cut — surface clearly instead of
			// silently dropping attachments / fork-context files.
			return nil, nil, fmt.Errorf("exedev runtime does not yet support attachments or worktree hooks")
		}
	}

	// Compensating-action stack: each successful side effect appends an undo
	// closure. unwind runs them LIFO. We run cleanups outside any critical
	// section — any cleanup failure is logged but cannot stop the chain.
	// The triggering label ("runner.Start", "OnWorktreeCreated", …) flows
	// into each cleanup via the unwindTrigger closure variable so per-step
	// error logs carry the trigger context.
	var cleanups []func(trigger string)
	var unwindTrigger string
	unwind := func(label string, cause error) {
		slog.Warn("CreateAndStart: unwinding", "trigger", label, "err", cause)
		unwindTrigger = label
		for i := len(cleanups) - 1; i >= 0; i-- {
			func(fn func(string)) {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("CreateAndStart: unwind step panicked", "trigger", unwindTrigger, "recover", r)
					}
				}()
				fn(unwindTrigger)
			}(cleanups[i])
		}
	}

	// Step 1: create worktree. Local runtime gets a real `git worktree add`;
	// exedev runtime bootstraps a directory on the remote VM via the
	// supplied callback. Either way, the unwind cleanup destroys the
	// worktree if any later step fails.
	baseBranch := input.BaseBranch
	if baseBranch == "" {
		baseBranch = projCfg.Branch
	}
	var (
		wtPath, finalName, branchName string
		err                           error
	)
	if input.Runtime == model.RuntimeExeDev {
		finalName = input.Name
		wtPath, err = input.CreateRemoteWorkspace(input.Name, baseBranch)
		if err != nil {
			return nil, nil, fmt.Errorf("remote workspace: %w", err)
		}
		// branchName stays empty for remote tasks — the remote workspace
		// has its own branch state and we don't track it locally.
		destroyRemote := input.DestroyRemoteWorkspace
		cleanups = append(cleanups, func(trigger string) {
			slog.Info("CreateAndStart unwind: destroy remote workspace", "trigger", trigger, "path", wtPath)
			if dErr := destroyRemote(wtPath); dErr != nil {
				slog.Warn("destroy remote workspace failed", "trigger", trigger, "path", wtPath, "err", dErr)
			}
		})
	} else {
		wtPath, finalName, branchName, err = CreateWorktree(projCfg.Path, input.Project, input.Name, baseBranch)
		if err != nil {
			return nil, nil, fmt.Errorf("worktree: %w", err)
		}
		repoPath := projCfg.Path
		cleanups = append(cleanups, func(trigger string) {
			slog.Info("CreateAndStart unwind: remove worktree", "trigger", trigger, "path", wtPath, "branch", branchName)
			RemoveWorktreeAndBranch(wtPath, branchName, repoPath)
		})
	}

	// Step 2a/b: attachments + per-worktree hook (local-only). Remote
	// runtime is gated above to forbid these inputs, so this branch only
	// runs for local tasks where the helpers expect a local filesystem.
	var attachPaths []string
	if input.Runtime == model.RuntimeLocal {
		attachPaths, err = writeAttachments(wtPath, input.Attachments)
		if err != nil {
			unwind("attachments", err)
			return nil, nil, fmt.Errorf("attachments: %w", err)
		}
		if input.OnWorktreeCreated != nil {
			if err := input.OnWorktreeCreated(wtPath); err != nil {
				unwind("OnWorktreeCreated", err)
				return nil, nil, fmt.Errorf("worktree hook: %w", err)
			}
		}
	}

	// Append attachment paths to the prompt so the agent sees them on the
	// first turn. Done after the hook so any fork-context preamble appears
	// first in the prompt — fork is the established case.
	prompt := input.Prompt
	if len(attachPaths) > 0 {
		prompt = appendAttachmentList(prompt, attachPaths)
	}

	// Step 3: build task and persist.
	backend := input.Backend
	if backend == "" {
		backend = cfg.Defaults.Backend
	}
	task := &model.Task{
		Name:       finalName,
		Status:     model.StatusPending,
		Project:    input.Project,
		Prompt:     prompt,
		Backend:    backend,
		Worktree:   wtPath,
		Branch:     branchName,
		PRURL:      input.PRURL,
		Runtime:    input.Runtime,
		RemoteHost: input.RemoteHost,
	}
	// Persist sandbox state at creation time so the display reflects the
	// setting active when the task was launched, not the current setting.
	// Remote tasks don't apply the local sandbox profile — the VM is the
	// sandbox — so the field stays zero for them.
	if input.Runtime == model.RuntimeLocal {
		task.Sandboxed = IsTaskSandboxed(task, cfg)
	}

	if err := database.Add(task); err != nil {
		unwind("db.Add", err)
		return nil, nil, fmt.Errorf("db add: %w", err)
	}
	// Capture task ID before registering the cleanup so it cannot drift.
	taskID := task.ID
	cleanups = append(cleanups, func(trigger string) {
		if dErr := database.Delete(taskID); dErr != nil {
			slog.Error("CreateAndStart unwind db.Delete failed", "trigger", trigger, "id", taskID, "err", dErr)
		}
	})

	// Step 4: generate session ID for Claude-style backends (Codex captures post-exit).
	if resolved, berr := ResolveBackend(task, cfg); berr == nil && !IsCodexBackend(resolved.Command) {
		task.SessionID = model.GenerateSessionID()
		if uErr := database.Update(task); uErr != nil {
			slog.Warn("CreateAndStart: persist session ID failed (continuing)", "id", taskID, "err", uErr)
		}
	}

	// Step 5: start session.
	rows := input.Rows
	if rows == 0 {
		rows = 24
	}
	cols := input.Cols
	if cols == 0 {
		cols = 80
	}

	if input.BeforeStart != nil {
		input.BeforeStart()
	}
	sess, err := runner.Start(task, cfg, rows, cols, false)
	if input.AfterStart != nil {
		input.AfterStart()
	}
	if err != nil {
		unwind("runner.Start", err)
		return nil, nil, fmt.Errorf("start session: %w", err)
	}

	// Step 6: flip to InProgress and record PID. Happens after AfterStart so
	// startGen is already post-bumped by the time any tick can observe the
	// status change.
	task.SetStatus(model.StatusInProgress)
	task.StartedAt = time.Now()
	task.AgentPID = sess.PID()
	if uErr := database.Update(task); uErr != nil {
		slog.Warn("CreateAndStart: persist InProgress failed (session is running)", "id", taskID, "err", uErr)
	}

	slog.Info("task created and started", "id", taskID, "name", task.Name, "project", input.Project, "pid", sess.PID())

	// Fire-and-forget Haiku rename. Runs after the task is live so a slow
	// or failing LLM call cannot block task startup. The goroutine is
	// race-guarded — if the user manually renames before Haiku returns,
	// the rename is skipped.
	if input.AutoName {
		go runAutoRename(database, taskID, task.Name, input.Prompt)
	}

	return task, sess, nil
}

// AttachmentsDir is the worktree-relative directory where uploaded attachments
// are written. Same dir used by fork-context, which is fine — names are
// disambiguated by the API layer (see SaveAttachments).
const AttachmentsDir = ".context"

// writeAttachments saves each attachment under <wtPath>/.context/<name> and
// returns worktree-relative paths (with leading "./") suitable for embedding
// in a prompt. Names are sanitized by the caller before this is called; we
// still defend with filepath.Base to refuse path traversal.
//
// Within a single batch, duplicate names are auto-suffixed (foo.png,
// foo-1.png, foo-2.png, …) so a client uploading two same-named files
// never silently clobbers the first.
func writeAttachments(wtPath string, atts []Attachment) ([]string, error) {
	if len(atts) == 0 {
		return nil, nil
	}
	dir := filepath.Join(wtPath, AttachmentsDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	paths := make([]string, 0, len(atts))
	used := make(map[string]bool, len(atts))
	for _, a := range atts {
		name := filepath.Base(a.Name)
		if name == "" || name == "." || name == ".." {
			return nil, fmt.Errorf("invalid attachment name %q", a.Name)
		}
		name = uniqueAttachmentName(used, name)
		used[name] = true
		dst := filepath.Join(dir, name)
		// Name is filepath.Base'd above; dst stays under dir.
		if err := os.WriteFile(dst, a.Data, 0o600); err != nil { //nolint:gosec // path validated
			return nil, fmt.Errorf("write %s: %w", dst, err)
		}
		paths = append(paths, "./"+AttachmentsDir+"/"+name)
	}
	return paths, nil
}

// uniqueAttachmentName returns name unchanged if not already in `used`; else
// suffixes with -1, -2, … before the extension until a free slot is found.
// Caller must add the returned name to `used`.
func uniqueAttachmentName(used map[string]bool, name string) string {
	if !used[name] {
		return name
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 1; i < 1000; i++ {
		candidate := fmt.Sprintf("%s-%d%s", base, i, ext)
		if !used[candidate] {
			return candidate
		}
	}
	// Fall back to original; the disk-write step will fail or clobber, but
	// at this point the user is on a 1000-collision pathological input.
	return name
}

// appendAttachmentList appends a human-readable list of attachment paths to
// the prompt so the agent sees them on the first turn. Returns the prompt
// unchanged when paths is empty.
func appendAttachmentList(prompt string, paths []string) string {
	if len(paths) == 0 {
		return prompt
	}
	var b strings.Builder
	if prompt != "" {
		b.WriteString(prompt)
		if !strings.HasSuffix(prompt, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("Attached files:\n")
	for _, p := range paths {
		b.WriteString("- ")
		b.WriteString(p)
		b.WriteString("\n")
	}
	return b.String()
}
