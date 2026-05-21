package agent

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/drn/argus/internal/config"
	"github.com/drn/argus/internal/model"
	"github.com/drn/argus/internal/ollama"
)

// prelaunchTimeout caps the wall-clock budget for backend-specific
// prelaunch work. Sized for the worst case (cold ollama daemon + cold
// qwen3:32b load on disk-cached weights): 30s for `brew services start
// ollama` to bring the API up + 5min for the first /api/generate to load
// the model. Overridable via SetPrelaunchTimeoutForTest.
var prelaunchTimeout = 6 * time.Minute

// ensurePrelaunchFn is the function called by Runner.Start before BuildCmd.
// Tests override this to inject mock behavior without exercising real
// brew/network calls.
var ensurePrelaunchFn = EnsurePrelaunch

// SetPrelaunchForTest overrides the prelaunch function. Returns a restore func.
func SetPrelaunchForTest(fn func(ctx context.Context, task *model.Task, cfg config.Config) error) func() {
	old := ensurePrelaunchFn
	ensurePrelaunchFn = fn
	return func() { ensurePrelaunchFn = old }
}

// SetPrelaunchTimeoutForTest overrides the per-call timeout. Returns a restore func.
func SetPrelaunchTimeoutForTest(d time.Duration) func() {
	old := prelaunchTimeout
	prelaunchTimeout = d
	return func() { prelaunchTimeout = old }
}

// EnsurePrelaunch performs backend-specific prelaunch checks. For the pi
// backend (which talks to a local ollama-hosted model), ensures the ollama
// daemon is running and qwen3:32b is loaded before returning. For any other
// backend, returns nil immediately.
//
// Blocks up to prelaunchTimeout on a cold daemon. Callers should run on a
// background goroutine if they cannot tolerate the wait.
func EnsurePrelaunch(ctx context.Context, task *model.Task, cfg config.Config) error {
	backend, err := ResolveBackend(task, cfg)
	if err != nil {
		// Unresolved backend isn't our concern — BuildCmd will surface it.
		return nil
	}
	if !IsPiBackend(backend.Command) {
		return nil
	}

	slog.Info("agent.EnsurePrelaunch: pi backend detected, ensuring ollama",
		"task", task.ID, "model", ollama.DefaultModel)

	ensureCtx, cancel := context.WithTimeout(ctx, prelaunchTimeout)
	defer cancel()

	if err := ollama.EnsureRunning(ensureCtx, ollama.DefaultModel); err != nil {
		return fmt.Errorf("pi backend requires ollama: %w", err)
	}
	slog.Info("agent.EnsurePrelaunch: ollama ready", "task", task.ID)
	return nil
}
