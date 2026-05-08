// Package exedev runs Argus tasks on remote exe.dev VMs over SSH.
//
// It implements agent.SessionProvider / agent.SessionHandle by multiplexing
// each task's PTY through an SSH session against a host configured in
// config.ExeDevConfig.Hosts. The local TUI sees the same byte stream and
// surface area as a local-runtime task — only the transport changes.
package exedev

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/drn/argus/internal/config"
	"golang.org/x/crypto/ssh"
)

// ConnectTimeout caps the TCP+SSH handshake. Long enough for cross-region
// dials, short enough that a misconfigured host fails the task instead of
// hanging the daemon.
const ConnectTimeout = 15 * time.Second

// HostKeyCallback resolves the SSH host key verification policy used by Dial.
// Default: read the user's known_hosts via the standard SSH path and reject
// unknown hosts. Callers can replace this for tests.
//
// Set the var (not the function) so the daemon can be configured at startup
// without plumbing a config struct through every call.
var HostKeyCallback = defaultHostKeyCallback

// dialFn is the SSH dialer used by Dial. Tests overwrite this to point at
// an in-process SSH server.
var dialFn = ssh.Dial

// Dial opens an SSH connection to host using the configured identity file.
//
// Errors are wrapped so the daemon's error log carries the host name; the
// task that triggered the dial fails to start with the same message.
func Dial(host config.ExeDevHost) (*ssh.Client, error) {
	if host.Host == "" {
		return nil, errors.New("exedev: host is empty")
	}
	if host.User == "" {
		return nil, errors.New("exedev: user is empty")
	}

	identity := host.IdentityFile
	if identity == "" {
		home, _ := os.UserHomeDir()
		identity = filepath.Join(home, ".ssh", "id_ed25519")
	}
	identity = expandHome(identity)
	keyBytes, err := os.ReadFile(identity)
	if err != nil {
		return nil, fmt.Errorf("exedev: read identity %s: %w", identity, err)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("exedev: parse identity %s: %w", identity, err)
	}

	hkCallback, err := HostKeyCallback()
	if err != nil {
		return nil, fmt.Errorf("exedev: host key policy: %w", err)
	}

	cfg := &ssh.ClientConfig{
		User:            host.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hkCallback,
		Timeout:         ConnectTimeout,
	}

	addr := net.JoinHostPort(host.Host, strconv.Itoa(host.ResolvedPort()))
	client, err := dialFn("tcp", addr, cfg)
	if err != nil {
		return nil, fmt.Errorf("exedev: dial %s: %w", addr, err)
	}
	return client, nil
}

// expandHome rewrites a leading "~/" to the real home directory. Empty input
// is returned unchanged so callers don't need to nil-check.
func expandHome(p string) string {
	if !strings.HasPrefix(p, "~/") && p != "~" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	return filepath.Join(home, p[2:])
}
