// Package gitrepo wraps the archive's git working tree — the local clone of the
// notes repo that each ingest commits into.
package gitrepo

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Repo is the archive's git working tree. name/email are the committer identity
// applied to each commit (see CommitPush); empty values fall back to git's own
// configuration.
type Repo struct {
	dir   string
	name  string
	email string
}

// Ensure returns a Repo for dir, cloning remote into it when dir is missing or
// empty, and verifying dir is a git work tree otherwise. An empty dir is treated
// like a missing one: `git clone` accepts an existing empty target, and containers
// mount the archive as a pre-created (empty) volume, so requiring absence would
// never clone there. name/email set the committer identity for CommitPush.
func Ensure(dir, remote, name, email string) (*Repo, error) {
	empty, err := dirEmptyOrMissing(dir)
	if err != nil {
		return nil, err
	}
	if empty {
		if _, err := git("", "clone", remote, dir); err != nil {
			return nil, fmt.Errorf("clone %s: %w", remote, err)
		}
		return &Repo{dir: dir, name: name, email: email}, nil
	}
	if _, err := git(dir, "rev-parse", "--is-inside-work-tree"); err != nil {
		return nil, fmt.Errorf("%s is not a git repository: %w", dir, err)
	}
	return &Repo{dir: dir, name: name, email: email}, nil
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
	if _, err := git(r.dir, "add", "-A"); err != nil {
		return false, err
	}
	dirty, err := r.dirty()
	if err != nil {
		return false, err
	}
	if !dirty {
		return false, nil
	}
	args := []string{"commit", "-m", message}
	if r.name != "" && r.email != "" {
		// -c must precede the subcommand: git -c user.name=… -c user.email=… commit …
		args = append([]string{"-c", "user.name=" + r.name, "-c", "user.email=" + r.email}, args...)
	}
	if _, err := git(r.dir, args...); err != nil {
		return false, err
	}
	if _, err := git(r.dir, "push"); err != nil {
		return true, fmt.Errorf("committed but push failed: %w", err)
	}
	return true, nil
}

// dirty reports whether the working tree has staged or unstaged changes.
func (r *Repo) dirty() (bool, error) {
	out, err := git(r.dir, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// git runs a git command in dir (or the current dir when dir is "") and returns its
// combined output, wrapping a non-zero exit with that output for context.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return buf.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(buf.String()))
	}
	return buf.String(), nil
}
