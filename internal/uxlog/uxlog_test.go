package uxlog

import (
	"bytes"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
func TestSlogWithUxlogWriter_DoesNotReachStderr(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test-ux.log")
	if err := Init(logPath); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer Close()

	// Capture anything that hits stderr during this test.
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	defer func() {
		os.Stderr = origStderr
	}()

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
	if !bytes.Equal(captured, []byte{}) {
		t.Errorf("slog/log wrote to stderr after redirect: %q", string(captured))
	}

	// And verify the messages DID land in the uxlog file.
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
