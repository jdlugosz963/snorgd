// Package gitrepo wraps the archive's git working tree — the local clone of the
// notes repo that each ingest commits into. It drives git in-process via go-git,
// so the runtime needs no `git` binary or `openssh-client`.
package gitrepo

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	// skeema/knownhosts wraps x/crypto's parser and adds the per-host algorithm
	// enumeration x/crypto exposes no API for — see buildAuth for why that matters.
	"github.com/skeema/knownhosts"
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
// path remotes), and opts is retained so a push failure can be explained in terms of
// the files it was configured with.
type Repo struct {
	repo  *git.Repository
	name  string
	email string
	auth  transport.AuthMethod
	opts  Options
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
			return nil, fmt.Errorf("clone %s: %w", opts.Remote, explainHostKey(err, opts))
		}
	} else {
		repo, err = git.PlainOpen(opts.Dir)
		if err != nil {
			return nil, fmt.Errorf("%s is not a git repository: %w", opts.Dir, err)
		}
	}
	return &Repo{repo: repo, name: opts.Name, email: opts.Email, auth: auth, opts: opts}, nil
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

// SyncResult says what Sync did, so the caller decides how to phrase it. A zero
// value means the local branch already matched the remote and nothing was touched.
type SyncResult struct {
	FastForwarded bool     // local was behind and was moved up to the remote
	Dropped       []string // subjects of local commits discarded by a reset, newest first
	Backup        string   // ref the dropped commits were saved under, "" if none
}

// Sync brings the working tree in line with the remote before a batch builds on it.
//
// The remote has priority, but a reset is the last resort rather than the rule: only
// a genuinely diverged branch loses anything, and what it loses is first saved under
// a backup ref. The four states are decided by ancestry:
//
//	local == remote                    nothing to do
//	local is an ancestor of remote     behind: fast-forward onto it
//	remote is an ancestor of local     unpushed work, remote unmoved: leave it, the
//	                                   batch's own push delivers it
//	neither                            diverged: back the local commits up, reset
//
// Sync must run *before* a batch ingests. snorg writes the archive throughout ingest
// and analyze, so resetting afterwards would destroy the batch's own work; resetting
// first means the work is built on the current remote tip and its push is a plain
// fast-forward.
func (r *Repo) Sync() (SyncResult, error) {
	var res SyncResult

	switch err := r.repo.Fetch(&git.FetchOptions{Auth: r.auth}); {
	case err == nil, errors.Is(err, git.NoErrAlreadyUpToDate):
		// Fetched, or there was nothing new.
	case errors.Is(err, git.ErrRemoteNotFound), errors.Is(err, transport.ErrEmptyRemoteRepository):
		// No remote, or one with no commits yet: nothing to sync against. Not an
		// error here — a push has its own, better-placed complaint to make.
		return res, nil
	default:
		return res, fmt.Errorf("fetch: %w", explainHostKey(err, r.opts))
	}

	head, err := r.repo.Head()
	if err != nil {
		return res, err
	}
	// An unborn or detached HEAD has no remote-tracking counterpart to sync against.
	if !head.Name().IsBranch() {
		return res, nil
	}
	remoteRef, err := r.repo.Reference(
		plumbing.NewRemoteReferenceName(git.DefaultRemoteName, head.Name().Short()), true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		// The branch does not exist on the remote yet — the first push creates it.
		return res, nil
	}
	if err != nil {
		return res, err
	}
	if head.Hash() == remoteRef.Hash() {
		return res, nil
	}

	local, err := r.repo.CommitObject(head.Hash())
	if err != nil {
		return res, err
	}
	remote, err := r.repo.CommitObject(remoteRef.Hash())
	if err != nil {
		return res, err
	}

	// Behind the remote: a reset here is exactly a fast-forward and drops nothing.
	behind, err := local.IsAncestor(remote)
	if err != nil {
		return res, err
	}
	if behind {
		if err := r.reset(remoteRef.Hash()); err != nil {
			return res, err
		}
		res.FastForwarded = true
		return res, nil
	}

	// Ahead of an unmoved remote: the commits are simply unpushed (an earlier push
	// failed, or this batch follows one). Pushing them is a fast-forward for the
	// remote, so they must not be thrown away.
	ahead, err := remote.IsAncestor(local)
	if err != nil {
		return res, err
	}
	if ahead {
		return res, nil
	}

	// Diverged. The remote wins, but the local commits are saved under a ref first:
	// a note whose commit is discarded is unrecoverable otherwise, since its .note
	// was deleted after ingest and the Dropbox cursor has already moved past it.
	res.Dropped, err = r.subjectsSince(local, remote)
	if err != nil {
		return res, err
	}
	res.Backup = fmt.Sprintf("refs/snorgd/dropped/%d", time.Now().Unix())
	backup := plumbing.NewHashReference(plumbing.ReferenceName(res.Backup), head.Hash())
	if err := r.repo.Storer.SetReference(backup); err != nil {
		return res, fmt.Errorf("back up diverged commits: %w", err)
	}
	if err := r.reset(remoteRef.Hash()); err != nil {
		return res, err
	}
	return res, nil
}

// reset moves the branch and working tree to hash, discarding local modifications.
func (r *Repo) reset(hash plumbing.Hash) error {
	wt, err := r.repo.Worktree()
	if err != nil {
		return err
	}
	return wt.Reset(&git.ResetOptions{Commit: hash, Mode: git.HardReset})
}

// subjectsSince lists the subject lines of the commits reachable from local but not
// from remote, newest first — the work a reset is about to discard, named so the log
// says which notes need re-saving from the device.
func (r *Repo) subjectsSince(local, remote *object.Commit) ([]string, error) {
	bases, err := local.MergeBase(remote)
	if err != nil {
		return nil, err
	}
	stop := make(map[plumbing.Hash]bool, len(bases))
	for _, b := range bases {
		stop[b.Hash] = true
	}

	iter := object.NewCommitPreorderIter(local, stop, nil)
	defer iter.Close()

	var subjects []string
	err = iter.ForEach(func(c *object.Commit) error {
		if stop[c.Hash] {
			return nil
		}
		subject, _, _ := strings.Cut(c.Message, "\n")
		subjects = append(subjects, strings.TrimSpace(subject))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return subjects, nil
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
		return true, fmt.Errorf("committed but push failed: %w", explainHostKey(err, r.opts))
	}
	return true, nil
}

// explainHostKey annotates a host-key verification failure with what to do about it.
// go-git surfaces these as bare "knownhosts: key mismatch" / "key is unknown", which
// says nothing about which file is consulted or how to repair it.
func explainHostKey(err error, opts Options) error {
	switch {
	case knownhosts.IsHostUnknown(err):
		return fmt.Errorf("%w — host not in %s; add it with: ssh-keyscan -H <host> >> %s",
			err, opts.KnownHosts, opts.KnownHosts)
	case knownhosts.IsHostKeyChanged(err):
		return fmt.Errorf("%w — the host key in %s no longer matches the server; "+
			"verify the new key out of band before replacing the entry",
			err, opts.KnownHosts)
	}
	return err
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
	kh, err := knownhosts.NewDB(opts.KnownHosts)
	if err != nil {
		return nil, fmt.Errorf("load known_hosts %s: %w", opts.KnownHosts, err)
	}
	auth.HostKeyCallback = kh.HostKeyCallback()

	// Restrict the host key algorithms we advertise to the ones known_hosts actually
	// holds for this host. Without this, Go offers its own default preference (which
	// ranks ECDSA above Ed25519), so a server presenting several host keys can
	// negotiate one we have no entry for — and known_hosts then reports a *mismatch*
	// rather than an unknown host, even though the key we recorded is perfectly
	// correct. OpenSSH avoids this by advertising only what it already knows.
	//
	// An empty list is deliberately not an error: it means the host is absent from
	// known_hosts, and leaving the field unset lets the callback fail with an honest
	// unknown-host error instead of a misleading one here.
	auth.HostKeyAlgorithms = kh.HostKeyAlgorithms(net.JoinHostPort(ep.Host, strconv.Itoa(ep.Port)))
	return auth, nil
}
