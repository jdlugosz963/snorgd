package gitrepo

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// initRepo makes a throwaway git repo with an isolated config so commits work
// without the developer's global identity.
func initRepo(t *testing.T) *Repo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "snorgd@test"},
		{"config", "user.name", "snorgd"},
		{"commit", "--allow-empty", "-q", "-m", "root"},
	} {
		if out, err := git(dir, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return &Repo{dir: dir}
}

func TestCommitPushDirty(t *testing.T) {
	r := initRepo(t)
	if err := os.WriteFile(filepath.Join(r.dir, "note.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No remote configured, so push fails — but a commit must still be made.
	committed, err := r.CommitPush("ingest: note")
	if !committed {
		t.Fatalf("expected a commit; err=%v", err)
	}
	count, _ := git(r.dir, "rev-list", "--count", "HEAD")
	if got := trim(count); got != "2" {
		t.Fatalf("commit count = %q, want 2", got)
	}
}

func TestCommitPushUsesConfiguredIdentity(t *testing.T) {
	r := initRepo(t)
	// A distinct identity that overrides the repo's own user.name/user.email.
	r.name, r.email = "snorgd", "snorgd@localhost"
	if err := os.WriteFile(filepath.Join(r.dir, "note.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if committed, _ := r.CommitPush("ingest: note"); !committed {
		t.Fatal("expected a commit")
	}
	out, err := git(r.dir, "log", "-1", "--format=%an <%ae>")
	if err != nil {
		t.Fatal(err)
	}
	if got := trim(out); got != "snorgd <snorgd@localhost>" {
		t.Fatalf("author = %q, want snorgd <snorgd@localhost>", got)
	}
}

func TestCommitPushClean(t *testing.T) {
	r := initRepo(t)
	committed, err := r.CommitPush("ingest: nothing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if committed {
		t.Fatal("clean tree must not produce a commit")
	}
}

func TestEnsureRejectsNonRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir() // exists, non-empty, but is not a git repo
	if err := os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(dir, "git@example:none", "", ""); err == nil {
		t.Fatal("expected error for a non-git directory")
	}
}

func trim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}
