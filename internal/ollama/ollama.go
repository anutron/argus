// Package ollama wraps the local ollama HTTP API so backends that depend on a
// running ollama daemon (currently: pi.dev's coding agent) can ensure the
// daemon is up and a target model is warm before a task launches.
//
// EnsureRunning is the single entry point. It probes /api/tags; if the daemon
// is down it shells out to `brew services start ollama` (launchd-managed
// ollama gets the GUI session's Metal/GPU access — a daemon-forked
// `ollama serve` panics during Metal init on macOS); then POSTs an empty
// generate with keep_alive so the model is loaded by the time the agent's
// first inference call arrives.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"time"
)

// Endpoint is the ollama HTTP endpoint. Overridable via SetForTest.
var Endpoint = "http://127.0.0.1:11434"

// DefaultModel is the qwen tag Argus preloads for pi-backed tasks.
const DefaultModel = "qwen3:32b"

// KeepAlive keeps the loaded model resident this long after the preload call,
// so the first real inference request from pi is instant.
const KeepAlive = "30m"

// brewCmd is the shell-out used to start the ollama daemon. Overridable via
// SetForTest. `brew services` is the chosen path because launchd gives ollama
// the GUI session's Metal access — a direct `ollama serve` fork from the
// Argus daemon hits `ggml_metal_init: failed to create command queue`.
var brewCmd = []string{"brew", "services", "start", "ollama"}

// startTimeout bounds the wait for `brew services start ollama` to bring the
// API up after the brew command returns.
var startTimeout = 30 * time.Second

// preloadTimeout bounds the model preload call. Cold model load can take
// 10-30s on disk-cached weights, longer on first-ever load.
var preloadTimeout = 5 * time.Minute

// probeTimeout bounds a single /api/tags liveness probe.
var probeTimeout = 500 * time.Millisecond

// pollInterval is how often StartDaemon re-probes /api/tags while waiting.
var pollInterval = 200 * time.Millisecond

// SetForTest overrides package-level config for tests. Returns a restore func.
//
// Pass an empty endpoint or nil brewArgs to leave that field untouched.
func SetForTest(endpoint string, brewArgs []string, startWait, preload time.Duration) func() {
	oldEndpoint := Endpoint
	oldBrew := brewCmd
	oldStart := startTimeout
	oldPreload := preloadTimeout
	if endpoint != "" {
		Endpoint = endpoint
	}
	if brewArgs != nil {
		brewCmd = brewArgs
	}
	if startWait > 0 {
		startTimeout = startWait
	}
	if preload > 0 {
		preloadTimeout = preload
	}
	return func() {
		Endpoint = oldEndpoint
		brewCmd = oldBrew
		startTimeout = oldStart
		preloadTimeout = oldPreload
	}
}

// IsRunning probes /api/tags with a short timeout. True iff the daemon
// answers 200 quickly. Network errors, non-200, or context expiry all map
// to false — this is a liveness probe, not a diagnostic.
func IsRunning(ctx context.Context) bool {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, Endpoint+"/api/tags", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// StartDaemon runs the configured brew command and polls IsRunning until the
// daemon answers or startTimeout elapses. brew returning success doesn't mean
// the daemon is ready — launchd's bring-up is async.
func StartDaemon(ctx context.Context) error {
	slog.Info("ollama: starting daemon", "cmd", brewCmd)
	cmd := exec.CommandContext(ctx, brewCmd[0], brewCmd[1:]...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	// Pipe-backed Stdout/Stderr means cmd.Wait() blocks on the I/O goroutines
	// returning, which won't happen until every child process inheriting our
	// pipe FDs exits. Without WaitDelay, a SIGKILL on `brew` leaves any
	// grandchild (e.g. launchctl, or a slow brew helper) holding the pipe
	// indefinitely after context cancel. 500ms is well above brew's normal
	// runtime and well below any user-visible threshold.
	cmd.WaitDelay = 500 * time.Millisecond
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %w (output: %s)", brewCmd, err, bytes.TrimSpace(out.Bytes()))
	}

	waitCtx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	deadline := time.Now().Add(startTimeout)
	for {
		if IsRunning(waitCtx) {
			slog.Info("ollama: daemon ready")
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ollama daemon not ready within %s after %v", startTimeout, brewCmd)
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("ollama daemon not ready within %s after %v", startTimeout, brewCmd)
		case <-time.After(pollInterval):
		}
	}
}

// PreloadModel POSTs an empty generate with keep_alive set, which tells
// ollama to load the model into VRAM and hold it for KeepAlive after the
// call returns. Returns a clear error if the model isn't installed.
func PreloadModel(ctx context.Context, model string) error {
	loadCtx, cancel := context.WithTimeout(ctx, preloadTimeout)
	defer cancel()

	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"keep_alive": KeepAlive,
	})
	req, err := http.NewRequestWithContext(loadCtx, http.MethodPost, Endpoint+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("preload %s: build request: %w", model, err)
	}
	req.Header.Set("Content-Type", "application/json")
	slog.Info("ollama: preloading model", "model", model, "keep_alive", KeepAlive)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("preload %s: %w", model, err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("ollama model %q is not installed (run: ollama pull %s): %s",
			model, model, bytes.TrimSpace(rb))
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("preload %s: HTTP %d: %s", model, resp.StatusCode, bytes.TrimSpace(rb))
	}
	slog.Info("ollama: model preloaded", "model", model)
	return nil
}

// EnsureRunning is the orchestrator: probe, start if down, then preload the
// model. Blocks up to startTimeout + preloadTimeout on a cold daemon.
func EnsureRunning(ctx context.Context, model string) error {
	if !IsRunning(ctx) {
		if err := StartDaemon(ctx); err != nil {
			return fmt.Errorf("start ollama daemon: %w", err)
		}
	}
	return PreloadModel(ctx, model)
}
