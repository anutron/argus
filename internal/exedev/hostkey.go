package exedev

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// defaultHostKeyCallback returns a HostKeyCallback that consults
// $HOME/.ssh/known_hosts. If the file does not exist, dials are rejected
// outright — exe.dev's documented bootstrap is to `ssh` once interactively
// to populate known_hosts, then point Argus at the same host.
//
// This is intentionally strict: a daemon that silently trusts new hosts
// would be vulnerable to MITM swaps of the cloud VM.
func defaultHostKeyCallback() (ssh.HostKeyCallback, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(home, ".ssh", "known_hosts")
	if _, statErr := os.Stat(path); statErr != nil {
		return nil, errors.New("exedev: ~/.ssh/known_hosts missing — run `ssh <host>` once to register the host key")
	}
	return knownhosts.New(path)
}
