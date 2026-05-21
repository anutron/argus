package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/drn/argus/internal/config"
	"github.com/drn/argus/internal/model"
	"github.com/drn/argus/internal/ollama"
	"github.com/drn/argus/internal/testutil"
)

func TestEnsurePrelaunch(t *testing.T) {
	mkCfg := func() config.Config {
		return config.Config{
			Defaults: config.Defaults{Backend: "test"},
			Backends: map[string]config.Backend{
				"test":   {Command: "echo hello"},
				"pi":     {Command: "pi"},
				"claude": {Command: "claude"},
			},
		}
	}

	t.Run("non-pi backend is a no-op", func(t *testing.T) {
		// If ollama would be invoked, this test fails loudly because Endpoint
		// is unset; we assert no error and no probe attempt by giving it an
		// unreachable endpoint with a brew stub that would error on call.
		defer ollama.SetForTest("http://127.0.0.1:1", []string{"/bin/false"}, 50*time.Millisecond, 50*time.Millisecond)()
		task := &model.Task{Backend: "claude"}
		testutil.NoError(t, EnsurePrelaunch(context.Background(), task, mkCfg()))
	})

	t.Run("unresolved backend is a no-op", func(t *testing.T) {
		task := &model.Task{Backend: "nope-doesnt-exist"}
		// Should NOT error — we let BuildCmd surface the resolve failure later.
		testutil.NoError(t, EnsurePrelaunch(context.Background(), task, mkCfg()))
	})

	t.Run("pi backend ensures ollama via brew + preload", func(t *testing.T) {
		var generateCalls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/tags":
				w.WriteHeader(200)
				_, _ = w.Write([]byte(`{"models":[{"name":"qwen3:32b"}]}`))
			case "/api/generate":
				atomic.AddInt32(&generateCalls, 1)
				w.WriteHeader(200)
				_, _ = w.Write([]byte(`{"done":true}`))
			default:
				w.WriteHeader(404)
			}
		}))
		defer srv.Close()
		defer ollama.SetForTest(srv.URL, []string{"/bin/true"}, time.Second, time.Second)()

		task := &model.Task{Backend: "pi"}
		testutil.NoError(t, EnsurePrelaunch(context.Background(), task, mkCfg()))
		testutil.Equal(t, atomic.LoadInt32(&generateCalls), int32(1))
	})

	t.Run("pi backend surfaces ollama failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/tags":
				w.WriteHeader(503)
			default:
				w.WriteHeader(500)
			}
		}))
		defer srv.Close()
		// Brew "succeeds" (exit 0) but the daemon never reports healthy.
		defer ollama.SetForTest(srv.URL, []string{"/bin/true"}, 200*time.Millisecond, 200*time.Millisecond)()
		defer SetPrelaunchTimeoutForTest(1 * time.Second)()

		task := &model.Task{Backend: "pi"}
		err := EnsurePrelaunch(context.Background(), task, mkCfg())
		if err == nil {
			t.Fatal("want error")
		}
		testutil.Contains(t, err.Error(), "pi backend requires ollama")
	})

	t.Run("pi backend respects timeout", func(t *testing.T) {
		// Brew script sleeps longer than the prelaunch timeout to force a
		// context-cancel error path.
		dir := t.TempDir()
		stub := filepath.Join(dir, "brew")
		_ = os.WriteFile(stub, []byte("#!/bin/sh\nsleep 5\nexit 0\n"), 0o755)

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(503)
		}))
		defer srv.Close()
		defer ollama.SetForTest(srv.URL, []string{stub}, 100*time.Millisecond, 100*time.Millisecond)()
		defer SetPrelaunchTimeoutForTest(150 * time.Millisecond)()

		task := &model.Task{Backend: "pi"}
		start := time.Now()
		err := EnsurePrelaunch(context.Background(), task, mkCfg())
		if err == nil {
			t.Fatal("want timeout error")
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("prelaunch did not honor timeout: took %s", elapsed)
		}
	})
}

// TestRunner_Start_CallsPrelaunch verifies the runner hook fires on every
// Start call. Uses SetPrelaunchForTest to inject a counter without touching
// ollama at all.
func TestRunner_Start_CallsPrelaunch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var calls int32
	defer SetPrelaunchForTest(func(ctx context.Context, task *model.Task, cfg config.Config) error {
		atomic.AddInt32(&calls, 1)
		return nil
	})()

	cfg := config.Config{
		Defaults: config.Defaults{Backend: "test"},
		Backends: map[string]config.Backend{
			"test": {Command: "echo hello", PromptFlag: ""},
		},
	}
	runner := NewRunner(nil)
	task := &model.Task{ID: "t-prelaunch-call", Backend: "test", Worktree: t.TempDir()}

	sess, err := runner.Start(task, cfg, 24, 80, false)
	testutil.NoError(t, err)
	defer func() { _ = sess.Stop() }()

	testutil.Equal(t, atomic.LoadInt32(&calls), int32(1))
}

// TestRunner_Start_PrelaunchFailureAborts asserts that a prelaunch error
// short-circuits Start, frees the slot reservation, and surfaces the error.
func TestRunner_Start_PrelaunchFailureAborts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	wantErr := errors.New("simulated prelaunch failure")
	defer SetPrelaunchForTest(func(ctx context.Context, task *model.Task, cfg config.Config) error {
		return wantErr
	})()

	cfg := config.Config{
		Defaults: config.Defaults{Backend: "test"},
		Backends: map[string]config.Backend{
			"test": {Command: "echo hello", PromptFlag: ""},
		},
	}
	runner := NewRunner(nil)
	task := &model.Task{ID: "t-prelaunch-fail", Backend: "test", Worktree: t.TempDir()}

	sess, err := runner.Start(task, cfg, 24, 80, false)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wraps %v", err, wantErr)
	}
	if sess != nil {
		t.Fatalf("sess = %v, want nil", sess)
	}

	// Slot must be released so a retry with the same task ID can succeed.
	defer SetPrelaunchForTest(func(ctx context.Context, task *model.Task, cfg config.Config) error {
		return nil
	})()
	retry, err := runner.Start(task, cfg, 24, 80, false)
	testutil.NoError(t, err)
	defer func() { _ = retry.Stop() }()
}
