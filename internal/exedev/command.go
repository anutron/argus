package exedev

import (
	"fmt"

	"github.com/drn/argus/internal/agent"
	"github.com/drn/argus/internal/config"
	"github.com/drn/argus/internal/model"
)

// buildAgentCommand returns the shell-quoted command line that runs the
// agent on the remote host. It mirrors agent.BuildCmd's flag-stitching but
// drops the local-only concerns (sandbox profile, exec.Cmd construction,
// stat-the-worktree pre-flight) — the worktree lives on the remote VM and
// the VM is itself the sandbox.
//
// Backend.Command is overridden by host.AgentCommand when set, letting a
// VM that installs the agent CLI under a different path (or via a wrapper)
// substitute it without forking the per-task config.
func buildAgentCommand(task *model.Task, cfg config.Config, host config.ExeDevHost, resume bool) (string, error) {
	backend, err := agent.ResolveBackend(task, cfg)
	if err != nil {
		return "", err
	}
	cmdStr := backend.Command
	if host.AgentCommand != "" {
		cmdStr = host.AgentCommand
	}

	if resume {
		if agent.IsCodexBackend(backend.Command) {
			cmdStr = "codex resume --dangerously-bypass-approvals-and-sandbox " + shellQuote(task.SessionID)
		} else if task.SessionID != "" {
			cmdStr += " --resume " + shellQuote(task.SessionID)
		}
	} else {
		if !agent.IsCodexBackend(backend.Command) && task.SessionID != "" {
			cmdStr += " --session-id " + shellQuote(task.SessionID)
		}
		if task.Prompt != "" {
			if backend.PromptFlag != "" {
				cmdStr += " " + backend.PromptFlag + " " + shellQuote(task.Prompt)
			} else {
				cmdStr += " -- " + shellQuote(task.Prompt)
			}
		}
	}

	if task.Worktree == "" {
		return "", fmt.Errorf("exedev: task %q has no remote worktree path", task.Name)
	}
	return cmdStr, nil
}
