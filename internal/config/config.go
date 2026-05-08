package config

import (
	"os"
	"path/filepath"
)

// Config is the top-level configuration.
type Config struct {
	Defaults    Defaults           `toml:"defaults"`
	Backends    map[string]Backend `toml:"backends"`
	Projects    map[string]Project `toml:"projects"`
	Keybindings Keybindings        `toml:"keybindings"`
	UI          UIConfig           `toml:"ui"`
	Sandbox     SandboxConfig      `toml:"sandbox"`
	KB          KBConfig           `toml:"kb"`
	API         APIConfig          `toml:"api"`
	Argus       ArgusConfig        `toml:"argus"`
	ExeDev      ExeDevConfig       `toml:"exedev"`
}

// ExeDevConfig holds the list of remote exe.dev VMs available for cloud
// runtime tasks. Persisted as JSON under config key "exedev.hosts".
type ExeDevConfig struct {
	Hosts map[string]ExeDevHost `toml:"hosts" json:"hosts"`
}

// ExeDevHost describes a single exe.dev VM. Name is the user-facing label
// (matches Task.RemoteHost). Host/Port/User identify the SSH endpoint.
// IdentityFile is the absolute path to a private key. WorkspaceRoot is the
// remote directory where per-task workspaces are created.
type ExeDevHost struct {
	Host          string `toml:"host"           json:"host"`
	Port          int    `toml:"port"           json:"port,omitempty"`
	User          string `toml:"user"           json:"user"`
	IdentityFile  string `toml:"identity_file"  json:"identity_file,omitempty"`
	WorkspaceRoot string `toml:"workspace_root" json:"workspace_root,omitempty"`
	// AgentCommand overrides cfg.Backends[task.Backend].Command for this host
	// when set. Empty = use the configured backend command verbatim. Useful
	// when the remote VM has a different binary path or wrapper.
	AgentCommand string `toml:"agent_command" json:"agent_command,omitempty"`
}

// DefaultExeDevPort is the SSH port used when ExeDevHost.Port is unset.
const DefaultExeDevPort = 22

// DefaultExeDevWorkspaceRoot is the remote directory under which per-task
// workspaces are created when ExeDevHost.WorkspaceRoot is unset.
const DefaultExeDevWorkspaceRoot = "~/argus"

// ArgusConfig holds settings for self-updating the Argus binary.
type ArgusConfig struct {
	SourcePath string `toml:"source_path"` // local clone of the Argus repo for go install
}

// APIConfig controls the HTTP REST API for remote control.
type APIConfig struct {
	Enabled  bool `toml:"enabled"`   // default false — must be turned on in settings
	HTTPPort int  `toml:"http_port"` // default 7743
}

// KBConfig controls the knowledge base server.
type KBConfig struct {
	Enabled        bool   `toml:"enabled"`          // default false — must be turned on in settings
	HTTPPort       int    `toml:"http_port"`        // default 7742
	MetisVaultPath string `toml:"metis_vault_path"` // Obsidian vault for KB indexing (Metis)
}

// iCloudObsidianBase is the default iCloud-synced Obsidian vault parent directory.
const iCloudObsidianBase = "Library/Mobile Documents/iCloud~md~obsidian/Documents"

// DefaultMetisVaultPath returns the default iCloud path for the Metis (KB) vault.
func DefaultMetisVaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, iCloudObsidianBase, "Metis")
}

type Defaults struct {
	Backend      string `toml:"backend"`
	ReviewPrompt string `toml:"review_prompt"` // prompt sent to agent for PR review tasks
}

type Backend struct {
	Command    string `toml:"command"`
	PromptFlag string `toml:"prompt_flag"`
}

// ProjectSandboxConfig holds per-project sandbox overrides.
// A nil Enabled means "inherit from global"; non-nil overrides the global setting.
type ProjectSandboxConfig struct {
	Enabled    *bool    // nil = inherit global; true/false = override
	DenyRead   []string // additional paths appended to the global deny_read list
	ExtraWrite []string // additional paths appended to the global extra_write list
}

type Project struct {
	Path    string               `toml:"path"`
	Branch  string               `toml:"branch"`
	Backend string               `toml:"backend"`
	Sandbox ProjectSandboxConfig `toml:"sandbox"`
}

type Keybindings struct {
	New      string `toml:"new"`
	Attach   string `toml:"attach"`
	Status   string `toml:"status"`
	Delete   string `toml:"delete"`
	Quit     string `toml:"quit"`
	Help     string `toml:"help"`
	Filter   string `toml:"filter"`
	Prompt   string `toml:"prompt"`
	Worktree string `toml:"worktree"`
}

type UIConfig struct {
	Theme            string `toml:"theme"`
	ShowElapsed      bool   `toml:"show_elapsed"`
	ShowIcons        bool   `toml:"show_icons"`
	CleanupWorktrees *bool  `toml:"cleanup_worktrees,omitempty"`
	SpinnerStyle     string `toml:"spinner_style"`
}

// SandboxConfig controls OS-level sandboxing of agent processes.
type SandboxConfig struct {
	Enabled    bool     `toml:"enabled"`
	DenyRead   []string `toml:"deny_read"`
	ExtraWrite []string `toml:"extra_write"`
}

// ShouldCleanupWorktrees returns whether worktrees should be auto-removed on task delete.
// Defaults to true if not explicitly set.
func (u UIConfig) ShouldCleanupWorktrees() bool {
	if u.CleanupWorktrees == nil {
		return true
	}
	return *u.CleanupWorktrees
}

// DefaultConfig returns a config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		Defaults: Defaults{Backend: "claude", ReviewPrompt: "/review"},
		Backends: map[string]Backend{
			"claude": {
				Command:    "claude --dangerously-skip-permissions --permission-mode plan",
				PromptFlag: "",
			},
			"codex": {
				Command:    "codex --dangerously-bypass-approvals-and-sandbox",
				PromptFlag: "",
			},
		},
		Projects:    make(map[string]Project),
		Keybindings: DefaultKeybindings(),
		UI: UIConfig{
			Theme:        "default",
			ShowElapsed:  true,
			ShowIcons:    true,
			SpinnerStyle: "progress",
		},
		KB: KBConfig{
			HTTPPort: 7742,
		},
		API: APIConfig{
			HTTPPort: 7743,
		},
		ExeDev: ExeDevConfig{
			Hosts: map[string]ExeDevHost{},
		},
	}
}

// ResolvedPort returns h.Port or DefaultExeDevPort.
func (h ExeDevHost) ResolvedPort() int {
	if h.Port == 0 {
		return DefaultExeDevPort
	}
	return h.Port
}

// ResolvedWorkspaceRoot returns h.WorkspaceRoot or DefaultExeDevWorkspaceRoot.
func (h ExeDevHost) ResolvedWorkspaceRoot() string {
	if h.WorkspaceRoot == "" {
		return DefaultExeDevWorkspaceRoot
	}
	return h.WorkspaceRoot
}

func DefaultKeybindings() Keybindings {
	return Keybindings{
		New:      "n",
		Attach:   "enter",
		Status:   "s",
		Delete:   "d",
		Quit:     "q",
		Help:     "?",
		Filter:   "/",
		Prompt:   "p",
		Worktree: "w",
	}
}
