package uxlog

import (
	"bytes"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestInitAndLog(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test-ux.log")

	if err := Init(logPath); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer Close()

	Log("hello %s %d", "world", 42)
	Log("second line")

	// Close to flush
	Close()

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "hello world 42") {
		t.Errorf("expected 'hello world 42' in log, got: %s", content)
	}
	if !strings.Contains(content, "second line") {
		t.Errorf("expected 'second line' in log, got: %s", content)
	}

	// Each line should have a timestamp prefix
	lines := strings.Split(strings.TrimSpace(content), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	for _, line := range lines {
		// Timestamp format: 2006/01/02 15:04:05.000
		if len(line) < 24 {
			t.Errorf("line too short for timestamp: %s", line)
		}
	}
}

func TestLogNoInit(t *testing.T) {
	// Ensure Log is a no-op when not initialized — should not panic.
	// Reset global state to simulate uninitialized.
	mu.Lock()
	old := file
	file = nil
	mu.Unlock()

	Log("this should be a no-op")

	mu.Lock()
	file = old
	mu.Unlock()
}

func TestInitIdempotent(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test-ux.log")

	if err := Init(logPath); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// Second init should be a no-op (not error)
	if err := Init(logPath); err != nil {
		t.Fatalf("second Init failed: %v", err)
	}

	Close()
}

func TestPath(t *testing.T) {
	got := Path("/home/user/.argus")
	if got != "/home/user/.argus/ux.log" {
		t.Errorf("Path returned %q", got)
	}
}

// TestWriter_ReturnsDiscardWhenNotInitialized pins the safety contract that
// `Writer()` never returns nil. Callers (notably runTUI's
// `slog.SetDefault(slog.New(slog.NewTextHandler(uxlog.Writer(), nil)))`)
// would panic on a nil writer.
func TestWriter_ReturnsDiscardWhenNotInitialized(t *testing.T) {
	mu.Lock()
	old := file
	file = nil
	mu.Unlock()
	defer func() {
		mu.Lock()
		file = old
		mu.Unlock()
	}()

	w := Writer()
	if w == nil {
		t.Fatal("Writer returned nil when uxlog not initialized")
	}
	if w != io.Discard {
		t.Errorf("Writer should return io.Discard when uninitialized, got %T", w)
	}
	// Should not panic when written to.
	if _, err := w.Write([]byte("test")); err != nil {
		t.Errorf("write to discard returned error: %v", err)
	}
}

// TestWriter_ReturnsLogFileWhenInitialized pins that Writer hands out the
// same file uxlog writes to, so slog output co-located with uxlog timestamps
// for easy correlation.
func TestWriter_ReturnsLogFileWhenInitialized(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test-ux.log")
	if err := Init(logPath); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer Close()

	w := Writer()
	if w == nil {
		t.Fatal("Writer returned nil after Init")
	}
	if w == io.Discard {
		t.Fatal("Writer returned io.Discard after Init")
	}

	// Writing through Writer() and reading via the file should round-trip.
	if _, err := w.Write([]byte("via writer\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	Close()

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(data), "via writer") {
		t.Errorf("expected 'via writer' in log, got: %s", string(data))
	}
}

// TestSlogWithUxlogWriter_DoesNotReachStderr is the regression guard for the
// "slog leaks to terminal" class of bug. CLAUDE.md hard rule 6: no code path
// reachable from runTUI may write to os.Stderr or os.Stdout once app.Run has
// taken over, because those fds ARE the user's terminal and writes corrupt
// tcell's displayed cell state. The fix is to redirect slog's default
// handler in runTUI; this test asserts that wiring `uxlog.Writer()` as the
// slog handler's destination genuinely keeps output OUT of stderr.
//
// If a future refactor breaks the wiring (e.g., removes `uxlog.Writer()` or
// changes slog.NewTextHandler's destination), this test fails before the
// regression ships.
//
// Test-isolation contract: this test mutates THREE process globals —
// `slog.Default()`, `log`'s default writer, and `os.Stderr`. All three are
// captured up front and restored via `t.Cleanup` so subsequent tests in
// the same binary run see the pre-test state. Without restoration, every
// following test in the package (or any package run in the same `go test`
// binary) would have its slog/log output redirected through this test's
// pipe — which is closed mid-body — producing silent dropped logs and
// confusing write-to-closed-fd errors. The unanimous BLOCKING finding
// from /rereview iter 1 was specifically this restore gap.
func TestSlogWithUxlogWriter_DoesNotReachStderr(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test-ux.log")
	if err := Init(logPath); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// Save process-global state we're about to mutate so subsequent tests
	// see the pre-test defaults. `t.Cleanup` fires in LIFO order, after any
	// `defer` in this test body — so cleanups run even on `t.Fatalf` panic.
	origSlog := slog.Default()
	origLog := log.Writer()
	origStderr := os.Stderr
	t.Cleanup(func() {
		slog.SetDefault(origSlog)
		log.SetOutput(origLog)
		os.Stderr = origStderr
		Close()
	})

	// Capture anything that hits stderr during this test.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w

	// Mirror runTUI's wiring exactly.
	slog.SetDefault(slog.New(slog.NewTextHandler(Writer(), nil)))
	log.SetOutput(Writer())

	// Fire the kind of calls that historically bled through.
	slog.Info("slog info from TUI process")
	slog.Error("slog error from TUI process", "task", "test-task")
	log.Printf("stdlib log print from TUI process")

	// Read whatever (if anything) reached stderr.
	if cerr := w.Close(); cerr != nil {
		t.Fatalf("close pipe writer: %v", cerr)
	}
	captured, rerr := io.ReadAll(r)
	if rerr != nil {
		t.Fatalf("read captured stderr: %v", rerr)
	}
	if len(captured) != 0 {
		t.Errorf("slog/log wrote to stderr after redirect: %q", string(captured))
	}

	// Restore slog/log defaults BEFORE closing the uxlog file, so any
	// late-firing slog calls in `t.Cleanup` (e.g., from goroutines leaked
	// by earlier tests) write to the original destination, not the
	// now-closed file. `t.Cleanup` restores os.Stderr and re-Closes uxlog
	// — re-Close is safe because uxlog.Close is idempotent (nils file).
	slog.SetDefault(origSlog)
	log.SetOutput(origLog)
	Close()

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		"slog info from TUI process",
		"slog error from TUI process",
		"stdlib log print from TUI process",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("expected %q in uxlog, got: %s", want, content)
		}
	}
}

// TestFd2RedirectViaDup2_CatchesRawSyscallWrites is the regression guard for
// the OS-level fd 2 redirect that runTUI installs as belt-and-braces for
// everything slog/log redirects can't catch — runtime panic stack dumps,
// subprocess fd 2 inheritance, third-party library writes to fd 2. The
// slog/log redirects only change the Go-level Writer that the standard
// loggers use; they do NOT change the OS-level meaning of file descriptor 2.
// Code that writes directly to fd 2 (e.g., via syscall.Write or via the Go
// runtime's internal panic-printing path) bypasses every Writer-based
// redirect.
//
// This test simulates a raw fd 2 write (the same syscall path the Go runtime
// uses for panic stack dumps) and verifies that with the Dup2 in place,
// those bytes land in the uxlog file instead of leaking to the terminal.
//
// Test-isolation: like the slog test above, this mutates the global fd 2
// and restores it via t.Cleanup. No t.Parallel so cross-test races are
// bounded to sequential package-test execution.
func TestFd2RedirectViaDup2_CatchesRawSyscallWrites(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test-ux.log")
	if err := Init(logPath); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// Mirror runTUI's Dup2 + deferred restore wiring.
	f, ok := Writer().(*os.File)
	if !ok {
		t.Fatalf("Writer() returned %T, expected *os.File", Writer())
	}
	// fd values from *os.File.Fd() are guaranteed-small positive ints — the
	// uintptr → int conversion can never overflow. Silence gosec G115.
	stderrFd := int(os.Stderr.Fd()) //nolint:gosec // see comment
	uxlogFd := int(f.Fd())          //nolint:gosec // see comment

	origStderrFd, err := syscall.Dup(stderrFd)
	if err != nil {
		t.Fatalf("Dup(stderr): %v", err)
	}
	if err := syscall.Dup2(uxlogFd, stderrFd); err != nil {
		_ = syscall.Close(origStderrFd)
		t.Fatalf("Dup2(uxlog → stderr): %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Dup2(origStderrFd, stderrFd)
		_ = syscall.Close(origStderrFd)
		Close()
	})

	// Simulate the runtime's panic-printing path: write directly to fd 2
	// via the raw syscall. If the Dup2 took effect, these bytes go to the
	// uxlog file. If the redirect didn't work, they'd appear on the test's
	// stderr (which Go's test runner inherits — so you'd see them in the
	// `go test` output as garbage).
	sentinel := []byte("FD2_RAW_WRITE_SENTINEL_via_syscall_Write\n")
	if _, werr := syscall.Write(stderrFd, sentinel); werr != nil {
		t.Fatalf("syscall.Write(fd 2): %v", werr)
	}

	// Force a flush by closing and re-reading. We close uxlog (idempotent;
	// Cleanup re-closes) so the buffer is committed to disk before read.
	Close()

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !bytes.Contains(data, sentinel) {
		t.Errorf("fd 2 Dup2 did not redirect raw syscall write to uxlog; "+
			"sentinel missing. logfile=%s contents=%q", logPath, string(data))
	}
}
