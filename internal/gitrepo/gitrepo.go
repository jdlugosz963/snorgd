// Package gitrepo wraps the archive's git working tree — the local clone of the
// notes repo that each ingest commits into. It drives git in-process via go-git,
// so the runtime needs no `git` binary or `openssh-client`.
package gitrepo

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Options configures Ensure. Dir/Remote/Name/Email mirror the daemon config; the
// SSH* fields are only consulted when Remote is an SSH URL.
type Options struct {
	Dir              string
	Remote           string
	Name             string // committer identity (see CommitPush)
	Email            string
	SSHKey           string // path to the private key
	SSHKeyPassphrase string
	KnownHosts       string // path to a known_hosts file (host keys verified strictly)
}

// Repo is the archive's git working tree. name/email are the committer identity
// applied to each commit (see CommitPush); empty values fall back to git's own
// configuration. auth is the transport auth for pushes/clones (nil for HTTPS or
// path remotes).
type Repo struct {
	repo  *git.Repository
	name  string
	email string
	auth  transport.AuthMethod
}

// Ensure returns a Repo for opts.Dir, cloning opts.Remote into it when the dir is
// missing or empty, and opening it as a git work tree otherwise. An empty dir is
// treated like a missing one: clone accepts an existing empty target, and containers
// mount the archive as a pre-created (empty) volume, so requiring absence would never
// clone there. Name/Email set the committer identity for CommitPush.
func Ensure(opts Options) (*Repo, error) {
	auth, err := buildAuth(opts)
	if err != nil {
		return nil, err
	}

	empty, err := dirEmptyOrMissing(opts.Dir)
	if err != nil {
		return nil, err
	}

	var repo *git.Repository
	if empty {
		repo, err = git.PlainClone(opts.Dir, false, &git.CloneOptions{URL: opts.Remote, Auth: auth})
		if err != nil {
			return nil, fmt.Errorf("clone %s: %w", opts.Remote, err)
		}
	} else {
		repo, err = git.PlainOpen(opts.Dir)
		if err != nil {
			return nil, fmt.Errorf("%s is not a git repository: %w", opts.Dir, err)
		}
	}
	return &Repo{repo: repo, name: opts.Name, email: opts.Email, auth: auth}, nil
}

// dirEmptyOrMissing reports whether dir does not exist or exists as an empty
// directory (the two cases where Ensure should clone into it).
func dirEmptyOrMissing(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

// CommitPush stages the working tree and, if anything changed, commits it under
// message and pushes to the upstream branch. It reports whether a commit was made
// (a clean tree is not an error — re-ingesting an unchanged note is a no-op).
func (r *Repo) CommitPush(message string) (bool, error) {
	wt, err := r.repo.Worktree()
	if err != nil {
		return false, err
	}
	// AddOptions{All: true} stages modifications, additions, and deletions — the
	// equivalent of `git add -A`.
	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return false, err
	}
	status, err := wt.Status()
	if err != nil {
		return false, err
	}
	if status.IsClean() {
		return false, nil
	}

	opts := &git.CommitOptions{}
	if r.name != "" && r.email != "" {
		// A configured identity overrides the repo's own user.name/user.email.
		sig := &object.Signature{Name: r.name, Email: r.email, When: time.Now()}
		opts.Author, opts.Committer = sig, sig
	}
	if _, err := wt.Commit(message, opts); err != nil {
		return false, err
	}
	if err := r.repo.Push(&git.PushOptions{Auth: r.auth}); err != nil {
		return true, fmt.Errorf("committed but push failed: %w", err)
	}
	return true, nil
}

// buildAuth returns the transport auth for the remote. SSH remotes use the
// configured private key with strict known_hosts verification; HTTPS and local
// path remotes need no auth (nil).
func buildAuth(opts Options) (transport.AuthMethod, error) {
	ep, err := transport.NewEndpoint(opts.Remote)
	if err != nil {
		return nil, fmt.Errorf("parse remote %q: %w", opts.Remote, err)
	}
	if ep.Protocol != "ssh" {
		return nil, nil
	}

	if opts.SSHKey == "" {
		return nil, errors.New("SSH remote requires a private key (set SNORGD_SSH_KEY)")
	}
	if opts.KnownHosts == "" {
		return nil, errors.New("SSH remote requires a known_hosts file (set SNORGD_SSH_KNOWN_HOSTS)")
	}
	auth, err := gitssh.NewPublicKeysFromFile(ep.User, opts.SSHKey, opts.SSHKeyPassphrase)
	if err != nil {
		return nil, fmt.Errorf("load SSH key %s: %w", opts.SSHKey, err)
	}
	cb, err := knownhosts.New(opts.KnownHosts)
	if err != nil {
		return nil, fmt.Errorf("load known_hosts %s: %w", opts.KnownHosts, err)
	}
	auth.HostKeyCallback = cb
	return auth, nil
}
