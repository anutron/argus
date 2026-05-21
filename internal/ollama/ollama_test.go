package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/drn/argus/internal/testutil"
)

// writeBrewStub writes a shell script at <dir>/<name> that echoes its args
// and exits 0 (or with exitCode if set). Returns the absolute path.
func writeBrewStub(t *testing.T, dir, name string, exitCode int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\necho \"$@\"\nexit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [12]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

func TestIsRunning(t *testing.T) {
	t.Run("up", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			testutil.Equal(t, r.URL.Path, "/api/tags")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"models":[]}`))
		}))
		defer srv.Close()
		defer SetForTest(srv.URL, nil, 0, 0)()

		testutil.Equal(t, IsRunning(context.Background()), true)
	})

	t.Run("down", func(t *testing.T) {
		// Use a port that's almost certainly closed. We bind+close to grab one.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		addr := srv.URL
		srv.Close()
		defer SetForTest(addr, nil, 0, 0)()

		testutil.Equal(t, IsRunning(context.Background()), false)
	})

	t.Run("non-200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
		}))
		defer srv.Close()
		defer SetForTest(srv.URL, nil, 0, 0)()

		testutil.Equal(t, IsRunning(context.Background()), false)
	})

	t.Run("ctx cancel", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(200 * time.Millisecond)
			w.WriteHeader(200)
		}))
		defer srv.Close()
		defer SetForTest(srv.URL, nil, 0, 0)()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		testutil.Equal(t, IsRunning(ctx), false)
	})
}

func TestStartDaemon(t *testing.T) {
	t.Run("success after brew + wait", func(t *testing.T) {
		// API answers 404 first N probes, then 200.
		var probes int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := atomic.AddInt32(&probes, 1)
			if n < 2 {
				w.WriteHeader(503)
				return
			}
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"models":[]}`))
		}))
		defer srv.Close()

		dir := t.TempDir()
		stub := writeBrewStub(t, dir, "fakebrew", 0)
		defer SetForTest(srv.URL, []string{stub, "services", "start", "ollama"}, time.Second, 0)()

		testutil.NoError(t, StartDaemon(context.Background()))
		if got := atomic.LoadInt32(&probes); got < 2 {
			t.Fatalf("expected >=2 probes, got %d", got)
		}
	})

	t.Run("brew failure surfaces output", func(t *testing.T) {
		dir := t.TempDir()
		stub := writeBrewStub(t, dir, "fakebrew", 1)
		defer SetForTest("http://127.0.0.1:1", []string{stub, "services", "start", "ollama"}, 200*time.Millisecond, 0)()

		err := StartDaemon(context.Background())
		if err == nil {
			t.Fatal("want error, got nil")
		}
		testutil.Contains(t, err.Error(), "fakebrew")
	})

	t.Run("api never comes up", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(503)
		}))
		defer srv.Close()
		dir := t.TempDir()
		stub := writeBrewStub(t, dir, "fakebrew", 0)
		defer SetForTest(srv.URL, []string{stub, "services", "start", "ollama"}, 300*time.Millisecond, 0)()

		err := StartDaemon(context.Background())
		if err == nil {
			t.Fatal("want timeout error")
		}
		testutil.Contains(t, err.Error(), "not ready")
	})

	t.Run("missing brew binary", func(t *testing.T) {
		defer SetForTest("http://127.0.0.1:1", []string{"/no/such/brew/binary/xyz", "services", "start", "ollama"}, 100*time.Millisecond, 0)()
		err := StartDaemon(context.Background())
		if err == nil {
			t.Fatal("want error for missing brew binary")
		}
	})
}

func TestPreloadModel(t *testing.T) {
	t.Run("success with keep_alive", func(t *testing.T) {
		var got map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			testutil.Equal(t, r.Method, "POST")
			testutil.Equal(t, r.URL.Path, "/api/generate")
			_ = json.NewDecoder(r.Body).Decode(&got)
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"done":true}`))
		}))
		defer srv.Close()
		defer SetForTest(srv.URL, nil, 0, time.Second)()

		testutil.NoError(t, PreloadModel(context.Background(), "qwen3:32b"))
		testutil.Equal(t, got["model"], "qwen3:32b")
		testutil.Equal(t, got["keep_alive"], KeepAlive)
	})

	t.Run("model not installed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"model not found"}`))
		}))
		defer srv.Close()
		defer SetForTest(srv.URL, nil, 0, time.Second)()

		err := PreloadModel(context.Background(), "doesnt-exist:1b")
		if err == nil {
			t.Fatal("want error")
		}
		testutil.Contains(t, err.Error(), "ollama pull")
	})

	t.Run("server 500", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":"model failed to load"}`))
		}))
		defer srv.Close()
		defer SetForTest(srv.URL, nil, 0, time.Second)()

		err := PreloadModel(context.Background(), "qwen3:32b")
		if err == nil {
			t.Fatal("want error")
		}
		testutil.Contains(t, err.Error(), "HTTP 500")
	})

	t.Run("network error", func(t *testing.T) {
		defer SetForTest("http://127.0.0.1:1", nil, 0, 200*time.Millisecond)()
		err := PreloadModel(context.Background(), "qwen3:32b")
		if err == nil {
			t.Fatal("want error")
		}
	})
}

func TestEnsureRunning(t *testing.T) {
	t.Run("daemon already up — skips brew", func(t *testing.T) {
		var brewCalls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"models":[]}`))
		}))
		defer srv.Close()

		dir := t.TempDir()
		// stub increments a counter file so the test can detect a call.
		counter := filepath.Join(dir, "calls")
		stub := filepath.Join(dir, "brewcounter")
		script := "#!/bin/sh\necho called >> " + counter + "\nexit 0\n"
		if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		_ = brewCalls // unused, file-based instead
		defer SetForTest(srv.URL, []string{stub}, time.Second, time.Second)()

		testutil.NoError(t, EnsureRunning(context.Background(), "qwen3:32b"))

		if b, _ := os.ReadFile(counter); len(b) != 0 {
			t.Fatalf("brew was called when daemon was already up: %q", string(b))
		}
	})

	t.Run("daemon down — starts then preloads", func(t *testing.T) {
		// First /api/tags probe (called by IsRunning) returns 503 to simulate
		// down. After brew is "started", subsequent probes return 200.
		var started atomic.Bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/tags":
				if !started.Load() {
					w.WriteHeader(503)
					return
				}
				w.WriteHeader(200)
				_, _ = w.Write([]byte(`{"models":[{"name":"qwen3:32b"}]}`))
			case "/api/generate":
				w.WriteHeader(200)
				_, _ = w.Write([]byte(`{"done":true}`))
			default:
				w.WriteHeader(404)
			}
		}))
		defer srv.Close()

		dir := t.TempDir()
		marker := filepath.Join(dir, "started")
		stub := filepath.Join(dir, "brew")
		script := "#!/bin/sh\ntouch " + marker + "\nexit 0\n"
		if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		// flip `started` once the stub touches its marker; do it via a
		// goroutine that watches the file.
		go func() {
			for {
				if _, err := os.Stat(marker); err == nil {
					started.Store(true)
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		}()

		defer SetForTest(srv.URL, []string{stub}, 2*time.Second, time.Second)()

		testutil.NoError(t, EnsureRunning(context.Background(), "qwen3:32b"))
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("brew was not called: %v", err)
		}
	})

	t.Run("daemon down — start fails", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(503)
		}))
		defer srv.Close()

		dir := t.TempDir()
		stub := writeBrewStub(t, dir, "brew", 7)
		defer SetForTest(srv.URL, []string{stub}, 200*time.Millisecond, 200*time.Millisecond)()

		err := EnsureRunning(context.Background(), "qwen3:32b")
		if err == nil {
			t.Fatal("want error")
		}
		testutil.Contains(t, err.Error(), "start ollama daemon")
	})
}
