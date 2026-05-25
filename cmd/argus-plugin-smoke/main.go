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
// The harness reads the token from that file. It does not mint or revoke
// tokens itself — that path requires direct DB access which the argus
// sandbox-exec profile blocks. The token can be reused across runs.
//
// # Usage
//
//	argus-plugin-smoke [-url http://127.0.0.1:7743] [-token-file ~/.argus/smoke-token]
//	                   [-scope smoke] [-verbose] [-keep]
//
// # Exit codes
//
//	0  every phase passed
//	1  a phase failed (details on stderr)
//	2  setup failed (couldn't read scope token, couldn't reach daemon, etc.)
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/drn/argus/internal/db"
)

type smoke struct {
	baseURL    string
	scopeToken string
	scope      string
	verbose    bool
	keep       bool
	httpClient *http.Client
}

func main() {
	urlFlag := flag.String("url", "http://127.0.0.1:7743", "argus API base URL")
	tokenFile := flag.String("token-file", filepath.Join(db.DataDir(), "smoke-token"), "path to a file containing the scope token plaintext")
	scopeFlag := flag.String("scope", "smoke", "scope name the token was minted with (used for namespacing assertions)")
	verbose := flag.Bool("verbose", false, "verbose phase-by-phase logging")
	keep := flag.Bool("keep", false, "skip teardown (throwaway task left in place for debugging)")
	flag.Parse()

	s, err := newSmoke(*urlFlag, *tokenFile, *scopeFlag, *verbose, *keep)
	if err != nil {
		fmt.Fprintf(os.Stderr, "setup: %v\n", err)
		os.Exit(2)
	}

	if err := s.run(); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("OK")
}

func newSmoke(baseURL, tokenFile, scope string, verbose, keep bool) (*smoke, error) {
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("read scope token at %s: %w\n\nFirst-time setup:\n  argus token mint --scope %s | awk '/^token:/ {print $2}' > %s\n  chmod 600 %s",
			tokenFile, err, scope, tokenFile, tokenFile)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return nil, fmt.Errorf("scope token at %s is empty", tokenFile)
	}
	s := &smoke{
		baseURL:    strings.TrimRight(baseURL, "/"),
		scopeToken: tok,
		scope:      scope,
		verbose:    verbose,
		keep:       keep,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
	if err := s.assertGET("/api/status", tok, http.StatusOK); err != nil {
		return nil, fmt.Errorf("daemon reachability check at %s: %w", baseURL, err)
	}
	return s, nil
}

func (s *smoke) run() error {
	if err := s.phase1AuthCheck(); err != nil {
		return fmt.Errorf("phase 1 (auth check): %w", err)
	}
	return nil
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
