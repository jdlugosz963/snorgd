package gitrepo

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
)

// initRepo makes a throwaway git repo (worktree) with a root commit under a fixed
// identity, so commits work without the developer's global config. It returns the
// Repo wrapper and the worktree directory.
func initRepo(t *testing.T) (*Repo, string) {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "snorgd", Email: "snorgd@test"}
	if _, err := wt.Commit("root", &git.CommitOptions{Author: sig, AllowEmptyCommits: true}); err != nil {
		t.Fatalf("root commit: %v", err)
	}
	return &Repo{repo: repo}, dir
}

// commitCount returns how many commits are reachable from HEAD.
func commitCount(t *testing.T, repo *git.Repository) int {
	t.Helper()
	iter, err := repo.Log(&git.LogOptions{})
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	defer iter.Close()
	n := 0
	if err := iter.ForEach(func(*object.Commit) error { n++; return nil }); err != nil {
		t.Fatalf("iter: %v", err)
	}
	return n
}

func TestCommitPushDirty(t *testing.T) {
	r, dir := initRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "note.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No remote configured, so push fails — but a commit must still be made.
	committed, err := r.CommitPush("ingest: note")
	if !committed {
		t.Fatalf("expected a commit; err=%v", err)
	}
	if got := commitCount(t, r.repo); got != 2 {
		t.Fatalf("commit count = %d, want 2", got)
	}
}

func TestCommitPushUsesConfiguredIdentity(t *testing.T) {
	r, dir := initRepo(t)
	// A distinct identity applied to the commit.
	r.name, r.email = "snorgd", "snorgd@localhost"
	if err := os.WriteFile(filepath.Join(dir, "note.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if committed, _ := r.CommitPush("ingest: note"); !committed {
		t.Fatal("expected a commit")
	}
	head, err := r.repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Author.Name + " <" + c.Author.Email + ">"; got != "snorgd <snorgd@localhost>" {
		t.Fatalf("author = %q, want snorgd <snorgd@localhost>", got)
	}
}

func TestCommitPushClean(t *testing.T) {
	r, _ := initRepo(t)
	committed, err := r.CommitPush("ingest: nothing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if committed {
		t.Fatal("clean tree must not produce a commit")
	}
}

func TestEnsureRejectsNonRepo(t *testing.T) {
	dir := t.TempDir() // exists, non-empty, but is not a git repo
	if err := os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An https remote needs no auth, so we reach (and fail at) the open step.
	if _, err := Ensure(Options{Dir: dir, Remote: "https://example.com/none"}); err == nil {
		t.Fatal("expected error for a non-git directory")
	}
}

// TestEnsureClonesAndPushes exercises the full clone→commit→push path against a
// local bare repo over the file transport (no SSH, no network).
func TestEnsureClonesAndPushes(t *testing.T) {
	bare := seedBare(t)

	clone := filepath.Join(t.TempDir(), "clone") // missing → Ensure clones
	r, err := Ensure(Options{Dir: clone, Remote: bare, Name: "snorgd", Email: "snorgd@localhost"})
	if err != nil {
		t.Fatalf("ensure/clone: %v", err)
	}
	if err := os.WriteFile(filepath.Join(clone, "note.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	committed, err := r.CommitPush("ingest: note")
	if err != nil {
		t.Fatalf("commit/push: %v", err)
	}
	if !committed {
		t.Fatal("expected a commit")
	}

	// Reopen the bare remote and confirm the pushed commit landed (root + note).
	remote, err := git.PlainOpen(bare)
	if err != nil {
		t.Fatal(err)
	}
	if got := commitCount(t, remote); got != 2 {
		t.Fatalf("remote commit count = %d, want 2", got)
	}
}

// seedBare creates a bare repo with one root commit on the default branch and
// returns its path, ready to be cloned.
func seedBare(t *testing.T) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatalf("init bare: %v", err)
	}

	src := t.TempDir()
	repo, err := git.PlainInit(src, false)
	if err != nil {
		t.Fatalf("init src: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "root.md"), []byte("root"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("root.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("root", &git.CommitOptions{Author: &object.Signature{Name: "snorgd", Email: "snorgd@test"}}); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{bare}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Push(&git.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{"refs/heads/*:refs/heads/*"},
	}); err != nil {
		t.Fatalf("seed push: %v", err)
	}
	return bare
}

// writeKnownHosts writes a known_hosts file containing exactly the given lines and
// returns its path.
func writeKnownHosts(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "known_hosts")
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}
	return path
}

// sshAuthOpts builds Options for an SSH remote with a throwaway private key.
func sshAuthOpts(t *testing.T, remote, knownHosts string) Options {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pem := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	path := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(path, pem, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return Options{Remote: remote, SSHKey: path, KnownHosts: knownHosts}
}

// The regression this guards: buildAuth used to set only HostKeyCallback, leaving
// HostKeyAlgorithms empty. Go then advertised its own defaults, which rank ECDSA
// above Ed25519, so a server offering several host keys could negotiate one absent
// from known_hosts — reported as a *mismatch*, not an unknown host, even though the
// recorded key was correct.
func TestBuildAuthRestrictsHostKeyAlgorithmsToKnownHosts(t *testing.T) {
	// An Ed25519-only entry, exactly like the failing real-world case.
	kh := writeKnownHosts(t, "git.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIH0hDvHNCbnQ7pT2wPMwAj0LqQCU2FyGwNZbYbBGqDBk")

	auth, err := buildAuth(sshAuthOpts(t, "git@git.example.com:repo", kh))
	if err != nil {
		t.Fatalf("buildAuth: %v", err)
	}
	pk, ok := auth.(*gitssh.PublicKeys)
	if !ok {
		t.Fatalf("auth is %T, want *ssh.PublicKeys", auth)
	}

	if len(pk.HostKeyAlgorithms) == 0 {
		t.Fatal("HostKeyAlgorithms is empty; Go would fall back to its own preference and negotiate an algorithm known_hosts has no entry for")
	}
	for _, algo := range pk.HostKeyAlgorithms {
		if !strings.Contains(algo, "ed25519") {
			t.Errorf("advertised %q, but known_hosts holds only an ed25519 key", algo)
		}
	}
	if pk.HostKeyCallback == nil {
		t.Error("HostKeyCallback is nil; host keys would not be verified")
	}
}

func TestBuildAuthLeavesAlgorithmsEmptyForAnUnknownHost(t *testing.T) {
	kh := writeKnownHosts(t, "other.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIH0hDvHNCbnQ7pT2wPMwAj0LqQCU2FyGwNZbYbBGqDBk")

	// An absent host must not fail here: the callback reports it honestly at
	// handshake time, which is a clearer error than anything buildAuth could give.
	auth, err := buildAuth(sshAuthOpts(t, "git@git.example.com:repo", kh))
	if err != nil {
		t.Fatalf("buildAuth on an unknown host: %v", err)
	}
	if got := auth.(*gitssh.PublicKeys).HostKeyAlgorithms; len(got) != 0 {
		t.Errorf("HostKeyAlgorithms = %v, want empty for a host with no entry", got)
	}
}

func TestBuildAuthSkipsNonSSHRemotes(t *testing.T) {
	auth, err := buildAuth(Options{Remote: "https://example.com/repo.git"})
	if err != nil {
		t.Fatalf("buildAuth: %v", err)
	}
	if auth != nil {
		t.Errorf("auth = %v, want nil for an HTTPS remote", auth)
	}
}
