package gitrepo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// syncFixture builds the real topology: a bare remote, snorgd's clone of it, and a
// second clone standing in for the user's PC — the other writer whose pushes are what
// put snorgd's branch behind or across the remote.
type syncFixture struct {
	snorgd *Repo
	dir    string // snorgd's working tree
	pc     string // the second clone's working tree
	bare   string
}

func newSyncFixture(t *testing.T) *syncFixture {
	t.Helper()
	bare := seedBare(t)

	dir := filepath.Join(t.TempDir(), "archive")
	r, err := Ensure(Options{Dir: dir, Remote: bare, Name: "snorgd", Email: "snorgd@localhost"})
	if err != nil {
		t.Fatalf("ensure snorgd clone: %v", err)
	}

	pc := filepath.Join(t.TempDir(), "pc")
	if _, err := git.PlainClone(pc, false, &git.CloneOptions{URL: bare}); err != nil {
		t.Fatalf("clone pc: %v", err)
	}
	return &syncFixture{snorgd: r, dir: dir, pc: pc, bare: bare}
}

// commitIn writes a file in one working tree and commits it, returning nothing: the
// caller cares about the resulting topology, not the hash.
func commitIn(t *testing.T, dir, name, message string) {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(message), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add(name); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "test", Email: "test@test"}
	if _, err := wt.Commit(message, &git.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatalf("commit %q: %v", message, err)
	}
}

// pushFrom publishes a clone's commits to the shared remote.
func pushFrom(t *testing.T, dir string) {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Push(&git.PushOptions{}); err != nil {
		t.Fatalf("push from %s: %v", dir, err)
	}
}

func headSubject(t *testing.T, r *Repo) string {
	t.Helper()
	head, err := r.repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	subject, _, _ := strings.Cut(c.Message, "\n")
	return subject
}

// backupRefs lists the refs Sync saved diverged commits under.
func backupRefs(t *testing.T, r *Repo) []string {
	t.Helper()
	iter, err := r.repo.References()
	if err != nil {
		t.Fatal(err)
	}
	defer iter.Close()
	var out []string
	if err := iter.ForEach(func(ref *plumbing.Reference) error {
		if strings.HasPrefix(ref.Name().String(), "refs/snorgd/dropped/") {
			out = append(out, ref.Name().String())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSyncInSyncIsANoOp(t *testing.T) {
	f := newSyncFixture(t)
	res, err := f.snorgd.Sync()
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.FastForwarded || res.Backup != "" {
		t.Errorf("Sync = %+v, want a zero result", res)
	}
	if got := backupRefs(t, f.snorgd); len(got) != 0 {
		t.Errorf("backup refs = %v, want none", got)
	}
}

func TestSyncFastForwardsWhenBehind(t *testing.T) {
	f := newSyncFixture(t)
	// The PC writes and publishes; snorgd has done nothing.
	commitIn(t, f.pc, "from-pc.md", "pc: edited by hand")
	pushFrom(t, f.pc)

	res, err := f.snorgd.Sync()
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !res.FastForwarded {
		t.Errorf("FastForwarded = false, want true")
	}
	if res.Backup != "" {
		t.Errorf("Backup = %q, want none: a fast-forward drops nothing", res.Backup)
	}
	if got := headSubject(t, f.snorgd); got != "pc: edited by hand" {
		t.Errorf("HEAD subject = %q, want the PC's commit", got)
	}
	// The fast-forward must reach the working tree, not just the ref.
	if _, err := os.Stat(filepath.Join(f.dir, "from-pc.md")); err != nil {
		t.Errorf("PC's file missing from the working tree: %v", err)
	}
}

// The regression test for the reported failure: an unpushed commit on an unmoved
// remote is ordinary pending work and must survive, not be reset away.
func TestSyncKeepsUnpushedCommitsWhenRemoteHasNotMoved(t *testing.T) {
	f := newSyncFixture(t)
	commitIn(t, f.dir, "note.md", "ingest: prawo jazdy.note")

	res, err := f.snorgd.Sync()
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.FastForwarded || res.Backup != "" {
		t.Errorf("Sync = %+v, want a zero result: nothing to sync", res)
	}
	if got := headSubject(t, f.snorgd); got != "ingest: prawo jazdy.note" {
		t.Errorf("HEAD subject = %q, want the unpushed commit kept", got)
	}
	// And it must still be pushable — this is the loop the daemon was stuck in.
	if err := f.snorgd.repo.Push(&git.PushOptions{}); err != nil {
		t.Errorf("push after Sync: %v", err)
	}
}

func TestSyncResetsToRemoteWhenDivergedAndBacksUpTheDroppedCommits(t *testing.T) {
	f := newSyncFixture(t)
	// Both sides commit on top of the same root: a genuine divergence.
	commitIn(t, f.pc, "from-pc.md", "pc: edited by hand")
	pushFrom(t, f.pc)
	commitIn(t, f.dir, "note.md", "ingest: prawo jazdy.note")

	before, err := f.snorgd.repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	dropped := before.Hash()

	res, err := f.snorgd.Sync()
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.Backup == "" {
		t.Fatal("Backup = \"\", want a ref holding the dropped commits")
	}
	if want := []string{"ingest: prawo jazdy.note"}; len(res.Dropped) != 1 || res.Dropped[0] != want[0] {
		t.Errorf("Dropped = %v, want %v", res.Dropped, want)
	}
	// The remote won.
	if got := headSubject(t, f.snorgd); got != "pc: edited by hand" {
		t.Errorf("HEAD subject = %q, want the remote's commit", got)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "note.md")); !os.IsNotExist(err) {
		t.Errorf("snorgd's file survived the reset: err=%v", err)
	}
	// The whole point of the backup: the dropped work is still reachable, so the
	// note it ingested can be recovered by hand.
	ref, err := f.snorgd.repo.Reference(plumbing.ReferenceName(res.Backup), true)
	if err != nil {
		t.Fatalf("resolve backup ref: %v", err)
	}
	if ref.Hash() != dropped {
		t.Errorf("backup ref = %s, want the pre-reset HEAD %s", ref.Hash(), dropped)
	}
	if _, err := f.snorgd.repo.CommitObject(dropped); err != nil {
		t.Errorf("dropped commit is unreachable: %v", err)
	}
	// And after the reset the branch is pushable again.
	if err := f.snorgd.repo.Push(&git.PushOptions{}); err != nil && err != git.NoErrAlreadyUpToDate {
		t.Errorf("push after Sync: %v", err)
	}
}

func TestSyncWithoutARemoteTrackingRefIsNotAnError(t *testing.T) {
	// A plain local repo: no remote, so nothing to sync against.
	r, dir := initRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "note.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := r.Sync()
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.FastForwarded || res.Backup != "" {
		t.Errorf("Sync = %+v, want a zero result", res)
	}
}
