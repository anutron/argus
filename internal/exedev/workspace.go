package exedev

import (
	"bytes"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// CreateWorkspace creates a per-task working directory on the remote host
// under root. The returned path is the absolute remote directory; callers
// store it on Task.Worktree (with the "exedev://" scheme prefix added at
// the caller).
//
// CloneURL is optional. When set, the workspace is bootstrapped with
// `git clone --branch <baseBranch> <CloneURL>`; when empty the directory is
// created empty (the agent decides what to do with it).
//
// Name collisions are auto-suffixed (-1, -2, …) to mirror local CreateWorktree.
func CreateWorkspace(client *ssh.Client, root, name, cloneURL, baseBranch string) (path string, err error) {
	if root == "" {
		return "", fmt.Errorf("exedev: workspace root is empty")
	}
	if name == "" {
		return "", fmt.Errorf("exedev: workspace name is empty")
	}

	// Resolve the actual remote path under root that doesn't already exist.
	// `mkdir` with -p alone would silently reuse an existing directory and
	// surprise the next clone step; explicitly probe + suffix instead.
	candidate := name
	for i := 0; i < 50; i++ {
		exists, err := remoteDirExists(client, root, candidate)
		if err != nil {
			return "", err
		}
		if !exists {
			break
		}
		i++
		candidate = fmt.Sprintf("%s-%d", name, i)
	}

	// mkdir -p (creates root too if missing) then echo the absolute path.
	// Using POSIX printf with a quoted format string defends against $root
	// containing spaces; the shell expands the variable but won't word-split
	// the printf invocation itself.
	cmd := fmt.Sprintf(
		`set -e; mkdir -p %s/%s && cd %s/%s && pwd`,
		shellQuote(root), shellQuote(candidate),
		shellQuote(root), shellQuote(candidate),
	)
	out, err := runRemote(client, cmd)
	if err != nil {
		return "", fmt.Errorf("exedev: mkdir workspace: %w", err)
	}
	absPath := strings.TrimSpace(string(out))
	if absPath == "" {
		return "", fmt.Errorf("exedev: empty workspace path returned")
	}

	// Optional git clone. Done after the directory is known to exist so a
	// failed clone leaves an empty dir we can blow away with DestroyWorkspace
	// using the same path we already trust.
	if cloneURL != "" {
		clone := fmt.Sprintf(`cd %s && git clone %s .`,
			shellQuote(absPath), shellQuote(cloneURL))
		if baseBranch != "" {
			clone = fmt.Sprintf(`cd %s && git clone --branch %s %s .`,
				shellQuote(absPath), shellQuote(baseBranch), shellQuote(cloneURL))
		}
		if _, err := runRemote(client, clone); err != nil {
			return absPath, fmt.Errorf("exedev: git clone: %w", err)
		}
	}

	return absPath, nil
}

// DestroyWorkspace removes the remote directory. Refuses to operate on
// "/" or paths that don't look absolute, as a defense against a corrupted
// task row driving a destructive shell command.
func DestroyWorkspace(client *ssh.Client, path string) error {
	if path == "" {
		return fmt.Errorf("exedev: empty path")
	}
	if !strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "~/") {
		return fmt.Errorf("exedev: refusing to destroy non-absolute path %q", path)
	}
	if path == "/" || path == "~" || path == "~/" {
		return fmt.Errorf("exedev: refusing to destroy root path %q", path)
	}
	cmd := fmt.Sprintf(`rm -rf %s`, shellQuote(path))
	if _, err := runRemote(client, cmd); err != nil {
		return fmt.Errorf("exedev: rm -rf %s: %w", path, err)
	}
	return nil
}

// remoteDirExists returns true if root/name is an existing directory on
// the remote host. Uses `test -d` rather than parsing `ls` output.
func remoteDirExists(client *ssh.Client, root, name string) (bool, error) {
	cmd := fmt.Sprintf(`test -d %s/%s && echo yes || echo no`,
		shellQuote(root), shellQuote(name))
	out, err := runRemote(client, cmd)
	if err != nil {
		return false, fmt.Errorf("exedev: test -d %s/%s: %w", root, name, err)
	}
	return strings.TrimSpace(string(out)) == "yes", nil
}

// runRemote executes a one-shot command on the remote host and returns
// combined stdout. Stderr is surfaced via err.
func runRemote(client *ssh.Client, cmd string) ([]byte, error) {
	sess, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer func() { _ = sess.Close() }()
	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	if err := sess.Run(cmd); err != nil {
		return stdout.Bytes(), fmt.Errorf("%w: %s", err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// shellQuote wraps s in single quotes, escaping any embedded single quote.
// Used to defend against task names / branch names that contain shell
// metacharacters when interpolated into the remote command line.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
