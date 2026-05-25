// argus-plugin-smoke is a black-box test harness for the argus plugin
// substrate. It exercises every plugin-facing HTTP surface (events SSE,
// task_meta, MCP tool registration, plugin views, settings sections, input
// injection) against a running argus daemon, asserts the contract holds, and
// cleans up after itself.
//
// Designed to run against the user's real local daemon. State mutations are
// scoped to a `smoke` token + a throwaway task that get torn down on exit.
//
// # Setup (one-time)
//
// Mint a scope token outside any sandbox (your normal shell):
//
//	argus token mint --scope smoke | awk '/^token:/ {print $2}' > ~/.argus/smoke-token
//	chmod 600 ~/.argus/smoke-token
//
// The harness reads the scope token from that file. It does not mint or
// revoke tokens itself — that path requires direct DB access which the
// argus sandbox-exec profile blocks. The token can be reused across runs.
//
// The harness ALSO reads the master token from ~/.argus/api-token (read-only,
// allowed by the sandbox). Master is used only for setup/teardown that's
// outside a plugin's normal surface (creating a sacrificial backend + task
// for the test target). Plugins never see master in production.
//
// # Usage
//
//	argus-plugin-smoke [-url http://127.0.0.1:7743] [-token-file ~/.argus/smoke-token]
//	                   [-master-token-file ~/.argus/api-token] [-project ARGUS]
//	                   [-scope smoke] [-verbose] [-keep]
//
// # Exit codes
//
//	0  every phase passed
//	1  a phase failed (details on stderr)
//	2  setup failed (couldn't read scope token, couldn't reach daemon, etc.)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/drn/argus/internal/db"
)

type smoke struct {
	baseURL     string
	scopeToken  string
	masterToken string
	scope       string
	project     string
	verbose     bool
	keep        bool
	httpClient  *http.Client

	// Resources allocated during run() and torn down on close(). The mu
	// guards taskID/backendName because cleanup may race a phase that
	// recorded a new resource.
	mu          sync.Mutex
	taskID      string // empty until phase 5 succeeds
	backendName string // empty until phase 5 succeeds
}

func main() {
	urlFlag := flag.String("url", "http://127.0.0.1:7743", "argus API base URL")
	tokenFile := flag.String("token-file", filepath.Join(db.DataDir(), "smoke-token"), "path to a file containing the scope token plaintext")
	masterTokenFile := flag.String("master-token-file", filepath.Join(db.DataDir(), "api-token"), "path to the master token file (used only for setup/teardown)")
	projectFlag := flag.String("project", "ARGUS", "argus project name to host the throwaway task")
	scopeFlag := flag.String("scope", "smoke", "scope name the token was minted with (used for namespacing assertions)")
	verbose := flag.Bool("verbose", false, "verbose phase-by-phase logging")
	keep := flag.Bool("keep", false, "skip teardown (throwaway task + backend left in place for debugging)")
	flag.Parse()

	s, err := newSmoke(*urlFlag, *tokenFile, *masterTokenFile, *projectFlag, *scopeFlag, *verbose, *keep)
	if err != nil {
		fmt.Fprintf(os.Stderr, "setup: %v\n", err)
		os.Exit(2)
	}
	defer s.cleanup()

	if err := s.run(); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("OK")
}

func newSmoke(baseURL, tokenFile, masterTokenFile, project, scope string, verbose, keep bool) (*smoke, error) {
	scopeTok, err := readTokenFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("read scope token at %s: %w\n\nFirst-time setup:\n  argus token mint --scope %s | awk '/^token:/ {print $2}' > %s\n  chmod 600 %s",
			tokenFile, err, scope, tokenFile, tokenFile)
	}
	masterTok, err := readTokenFile(masterTokenFile)
	if err != nil {
		return nil, fmt.Errorf("read master token at %s: %w", masterTokenFile, err)
	}
	s := &smoke{
		baseURL:     strings.TrimRight(baseURL, "/"),
		scopeToken:  scopeTok,
		masterToken: masterTok,
		project:     project,
		scope:       scope,
		verbose:     verbose,
		keep:        keep,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
	}
	if err := s.assertGET("/api/status", scopeTok, http.StatusOK); err != nil {
		return nil, fmt.Errorf("daemon reachability check at %s: %w", baseURL, err)
	}
	return s, nil
}

func readTokenFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return tok, nil
}

func (s *smoke) run() error {
	if err := s.phase1AuthCheck(); err != nil {
		return fmt.Errorf("phase 1 (auth check): %w", err)
	}
	if err := s.phase3EventStream(); err != nil {
		return fmt.Errorf("phase 3 (event stream): %w", err)
	}
	if err := s.phase5ThrowawayTask(); err != nil {
		return fmt.Errorf("phase 5 (throwaway task): %w", err)
	}
	return nil
}

// cleanup tears down resources allocated during the run. Safe to call
// multiple times; idempotent per resource. Skipped when --keep is set.
func (s *smoke) cleanup() {
	if s.keep {
		s.mu.Lock()
		taskID := s.taskID
		backendName := s.backendName
		s.mu.Unlock()
		if taskID != "" {
			fmt.Printf("--keep: task id=%s and backend %q left in place\n", taskID, backendName)
		}
		return
	}
	s.mu.Lock()
	taskID := s.taskID
	backendName := s.backendName
	s.taskID = ""
	s.backendName = ""
	s.mu.Unlock()

	if taskID != "" {
		if err := s.adminDELETE("/api/tasks/"+taskID, http.StatusOK); err != nil {
			s.logf("cleanup: delete task %s failed: %v", taskID, err)
		} else {
			s.logf("cleanup: deleted task %s", taskID)
		}
	}
	if backendName != "" {
		if err := s.adminDELETE("/api/backends/"+backendName, http.StatusOK); err != nil {
			s.logf("cleanup: delete backend %q failed: %v", backendName, err)
		} else {
			s.logf("cleanup: deleted backend %q", backendName)
		}
	}
}

// phase1AuthCheck confirms the externally-minted scope token authenticates
// against a scope-aware endpoint. The token itself is treated as opaque —
// the harness never mints or revokes.
func (s *smoke) phase1AuthCheck() error {
	if err := s.assertGET("/api/plugins/views", s.scopeToken, http.StatusOK); err != nil {
		return fmt.Errorf("scope-token auth against GET /api/plugins/views: %w", err)
	}
	s.logf("scope=%s token authenticates against /api/plugins/views", s.scope)
	return nil
}

// phase3EventStream opens the SSE channel against the live daemon, confirms
// the handshake succeeds, and closes cleanly. No event assertions yet — later
// phases drive the daemon and then call sub.WaitFor on the resulting events.
func (s *smoke) phase3EventStream() error {
	sub, err := s.startEventStream(0)
	if err != nil {
		return fmt.Errorf("open SSE: %w", err)
	}
	defer sub.Close()
	s.logf("SSE handshake OK; ready to consume events")
	return nil
}

// phase5ThrowawayTask creates a transient bash-backed backend ("bash-smoke")
// and a task on the configured project. Both are torn down in cleanup().
// The bash session itself is idle waiting for stdin, which is exactly what
// Phase 9 (input injection) needs.
//
// If the backend already exists (e.g., from a prior --keep run), the harness
// reuses it but does NOT claim ownership for cleanup — it cannot tell
// whether the existing backend was someone else's bash backend or a leftover
// from us.
func (s *smoke) phase5ThrowawayTask() error {
	const backendName = "bash-smoke"
	owned, err := s.ensureBashBackend(backendName)
	if err != nil {
		return fmt.Errorf("ensure backend %q: %w", backendName, err)
	}
	if owned {
		s.mu.Lock()
		s.backendName = backendName
		s.mu.Unlock()
		s.logf("registered backend %q (bash)", backendName)
	} else {
		s.logf("reusing pre-existing backend %q (cleanup skipped)", backendName)
	}

	taskName := fmt.Sprintf("argus-plugin-smoke-%d", time.Now().Unix())
	taskID, err := s.createBashTask(taskName, backendName)
	if err != nil {
		return fmt.Errorf("create task: %w", err)
	}
	s.mu.Lock()
	s.taskID = taskID
	s.mu.Unlock()
	s.logf("created task id=%s name=%q project=%q backend=%q", taskID, taskName, s.project, backendName)
	return nil
}

// ensureBashBackend POSTs the backend definition. Returns owned=true when we
// just created it (cleanup later), owned=false when it already existed
// (cleanup skipped). Treats any 2xx as success.
func (s *smoke) ensureBashBackend(name string) (bool, error) {
	body := fmt.Sprintf(`{"name":%q,"command":"bash"}`, name)
	resp, err := s.adminPOST("/api/backends", body)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusCreated:
		return true, nil
	case resp.StatusCode == http.StatusConflict:
		return false, nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return true, nil
	default:
		out, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("POST /api/backends: status %d: %s", resp.StatusCode, truncate(string(out), 200))
	}
}

func (s *smoke) createBashTask(name, backend string) (string, error) {
	body := fmt.Sprintf(`{"name":%q,"project":%q,"backend":%q}`, name, s.project, backend)
	resp, err := s.adminPOST("/api/tasks", body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		out, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("POST /api/tasks: status %d: %s", resp.StatusCode, truncate(string(out), 200))
	}
	var decoded struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return "", fmt.Errorf("decode task response: %w", err)
	}
	if decoded.ID == "" {
		return "", fmt.Errorf("daemon returned empty task id")
	}
	return decoded.ID, nil
}

func (s *smoke) assertGET(path, token string, want int) error {
	req, err := http.NewRequest(http.MethodGet, s.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GET %s: status %d (want %d): %s", path, resp.StatusCode, want, truncate(string(body), 200))
	}
	return nil
}

// adminPOST sends a JSON POST authenticated with the master token. Used by
// phase 5 (backend + task creation) and cleanup. Caller owns the response
// body and must Close() it.
func (s *smoke) adminPOST(path, body string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, s.baseURL+path, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.masterToken)
	req.Header.Set("Content-Type", "application/json")
	return s.httpClient.Do(req)
}

// adminDELETE sends a DELETE authenticated with the master token. Used by
// cleanup. Expects `want` status; returns an error otherwise.
func (s *smoke) adminDELETE(path string, want int) error {
	req, err := http.NewRequest(http.MethodDelete, s.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.masterToken)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("DELETE %s: status %d (want %d): %s", path, resp.StatusCode, want, truncate(string(body), 200))
	}
	return nil
}

func (s *smoke) logf(format string, args ...any) {
	if s.verbose {
		fmt.Printf("[smoke] "+format+"\n", args...)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
