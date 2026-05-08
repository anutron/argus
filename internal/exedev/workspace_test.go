package exedev

import (
	"strings"
	"testing"

	"github.com/drn/argus/internal/testutil"
)

func TestShellQuote(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"plain", "'plain'"},
		{"with space", "'with space'"},
		{"with'quote", `'with'\''quote'`},
		{"", "''"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			testutil.Equal(t, shellQuote(tc.in), tc.want)
		})
	}
}

func TestDestroyWorkspace_RefusesUnsafePaths(t *testing.T) {
	for _, p := range []string{"", "/", "~/", "~", "relative/path", "../../etc"} {
		t.Run(p, func(t *testing.T) {
			err := DestroyWorkspace(nil, p)
			if err == nil {
				t.Fatalf("expected error for %q", p)
			}
			if !strings.Contains(err.Error(), "exedev:") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
