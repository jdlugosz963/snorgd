// Package config loads the daemon's runtime configuration from the environment, so
// secrets never touch the archive or the command line.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config is the daemon's runtime configuration, sourced entirely from the
// environment.
type Config struct {
	// Dropbox OAuth (a long-lived refresh token exchanged for short-lived access
	// tokens). Create an app at https://www.dropbox.com/developers/apps.
	DropboxAppKey       string
	DropboxAppSecret    string
	DropboxRefreshToken string
	// DropboxFolder is the watched folder, recursively. "/" (whole Dropbox) is
	// normalized to "".
	DropboxFolder string

	// Archive is the local clone of the notes repo (git working tree == snorg
	// archive). Cloned from Remote on first run if it does not exist.
	Archive string
	Remote  string

	// StateDir persists the Dropbox cursor and the dispatcher's per-page form state
	// across restarts, kept outside the archive so neither is ever committed.
	StateDir string

	// ConfigFile overrides where the pipeline configuration (ingest rules, the
	// dispatch stage and its handlers) is read from. Empty means
	// <Archive>/snorgd.yaml, so the rules travel with the notes repo.
	ConfigFile string

	// GitName/GitEmail are the committer identity used for archive commits, so the
	// daemon does not depend on an ambient global git identity (containers, CI).
	GitName  string
	GitEmail string

	// SSH auth for an SSH Remote (git@host:…). All optional: HTTPS remotes need
	// none, and a missing key/known_hosts surfaces as a clear error at clone/push
	// time rather than a startup failure.
	SSHKey           string // path to the private key
	SSHKeyPassphrase string // passphrase for the key, if encrypted
	SSHKnownHosts    string // path to a known_hosts file (host keys verified strictly)
}

// Load reads the configuration from the environment, applying defaults and
// validating required fields.
func Load() (*Config, error) {
	// Capture the raw folder value before normalization: a deliberate whole-Dropbox
	// value of "/" normalizes to "" and must not be mistaken for "unset".
	rawFolder := os.Getenv("DROPBOX_FOLDER")

	c := &Config{
		DropboxAppKey:       strings.TrimSpace(os.Getenv("DROPBOX_APP_KEY")),
		DropboxAppSecret:    strings.TrimSpace(os.Getenv("DROPBOX_APP_SECRET")),
		DropboxRefreshToken: strings.TrimSpace(os.Getenv("DROPBOX_REFRESH_TOKEN")),
		DropboxFolder:       rawFolder,
		Archive:             expandHome(os.Getenv("SNORGD_ARCHIVE")),
		Remote:              os.Getenv("SNORGD_REMOTE"),
		StateDir:            expandHome(os.Getenv("SNORGD_STATE_DIR")),
		ConfigFile:          expandHome(strings.TrimSpace(os.Getenv("SNORGD_CONFIG"))),
		GitName:             strings.TrimSpace(os.Getenv("SNORGD_GIT_NAME")),
		GitEmail:            strings.TrimSpace(os.Getenv("SNORGD_GIT_EMAIL")),
		SSHKey:              expandHome(strings.TrimSpace(os.Getenv("SNORGD_SSH_KEY"))),
		SSHKeyPassphrase:    os.Getenv("SNORGD_SSH_KEY_PASSPHRASE"),
		SSHKnownHosts:       expandHome(strings.TrimSpace(os.Getenv("SNORGD_SSH_KNOWN_HOSTS"))),
	}

	// The Dropbox API wants the root as "" (not "/") and no trailing slash.
	c.DropboxFolder = strings.TrimRight(c.DropboxFolder, "/")
	if c.StateDir == "" {
		c.StateDir = defaultStateDir()
	}
	if c.GitName == "" {
		c.GitName = "snorgd"
	}
	if c.GitEmail == "" {
		c.GitEmail = "snorgd@localhost"
	}

	var missing []string
	if c.DropboxAppKey == "" {
		missing = append(missing, "DROPBOX_APP_KEY")
	}
	if c.DropboxAppSecret == "" {
		missing = append(missing, "DROPBOX_APP_SECRET")
	}
	if c.DropboxRefreshToken == "" {
		missing = append(missing, "DROPBOX_REFRESH_TOKEN")
	}
	if c.Archive == "" {
		missing = append(missing, "SNORGD_ARCHIVE")
	}
	if c.Remote == "" {
		missing = append(missing, "SNORGD_REMOTE")
	}
	if rawFolder == "" {
		missing = append(missing, "DROPBOX_FOLDER")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required env: %s", strings.Join(missing, ", "))
	}
	return c, nil
}

// defaultStateDir returns $XDG_STATE_HOME/snorgd, falling back to ~/.local/state.
func defaultStateDir() string {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "snorgd")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "state", "snorgd")
	}
	return ".snorgd-state"
}

// expandHome expands a leading ~ to the user's home directory.
func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(p[1:], "/"))
}
