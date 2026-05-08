package model

import "fmt"

// Runtime selects where a task's agent process executes.
type Runtime int

const (
	// RuntimeLocal runs the agent locally with a git worktree on disk. Default.
	RuntimeLocal Runtime = iota
	// RuntimeExeDev runs the agent on a remote exe.dev VM over SSH; the
	// "worktree" lives on the VM and the PTY is multiplexed through the
	// SSH session.
	RuntimeExeDev
)

var runtimeNames = [...]string{
	"local",
	"exedev",
}

var runtimeDisplayNames = [...]string{
	"Local",
	"exe.dev",
}

func (r Runtime) String() string {
	if int(r) < len(runtimeNames) {
		return runtimeNames[r]
	}
	return fmt.Sprintf("unknown(%d)", int(r))
}

// DisplayName returns a human-readable name (e.g. "exe.dev").
func (r Runtime) DisplayName() string {
	if int(r) < len(runtimeDisplayNames) {
		return runtimeDisplayNames[r]
	}
	return r.String()
}

// ParseRuntime converts a string to a Runtime. Empty input returns
// RuntimeLocal so DB rows that pre-date the column default cleanly.
func ParseRuntime(s string) (Runtime, error) {
	if s == "" {
		return RuntimeLocal, nil
	}
	for i, name := range runtimeNames {
		if name == s {
			return Runtime(i), nil
		}
	}
	return RuntimeLocal, fmt.Errorf("unknown runtime: %q", s)
}

func (r Runtime) MarshalText() ([]byte, error) {
	return []byte(r.String()), nil
}

func (r *Runtime) UnmarshalText(data []byte) error {
	parsed, err := ParseRuntime(string(data))
	if err != nil {
		return err
	}
	*r = parsed
	return nil
}
