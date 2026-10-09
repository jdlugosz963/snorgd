package gitrepo

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
)

// TestRecoversFromTheReportedWedge reproduces the exact reported failure end to end:
// snorgd commits a note, the push is rejected because the PC moved master, and every
// later batch fails the same way. It then asserts a synced batch gets unstuck.
func TestRecoversFromTheReportedWedge(t *testing.T) {
	f := newSyncFixture(t)

	// The PC edits the archive by hand and pushes.
	commitIn(t, f.pc, "by-hand.md", "pc: fixed a transcription")
	pushFrom(t, f.pc)

	// snorgd, which has not fetched, ingests a note and tries to push.
	if err := os.WriteFile(filepath.Join(f.dir, "prawo-jazdy.md"), []byte("note"), 0o644); err != nil {
		t.Fatal(err)
	}
	committed, err := f.snorgd.CommitPush("ingest: prawo jazdy.note")
	if !committed {
		t.Fatalf("expected a commit; err=%v", err)
	}
	if err == nil {
		t.Fatal("expected the reported non-fast-forward push failure, got a clean push")
	}
	t.Logf("reproduced: %v", err)

	// A second batch fails identically — the loop the daemon was stuck in.
	if err := os.WriteFile(filepath.Join(f.dir, "prompts.md"), []byte("note"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.snorgd.CommitPush("ingest: prompts.note"); err == nil {
		t.Fatal("expected the second push to fail too")
	}

	// Now the fix: sync at the start of the next batch.
	res, err := f.snorgd.Sync()
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.Backup == "" || len(res.Dropped) != 2 {
		t.Fatalf("Sync = %+v, want both stuck commits backed up", res)
	}

	// The next batch's work now lands on the remote tip and pushes cleanly.
	if err := os.WriteFile(filepath.Join(f.dir, "next.md"), []byte("note"), 0o644); err != nil {
		t.Fatal(err)
	}
	committed, err = f.snorgd.CommitPush("ingest: next.note")
	if !committed {
		t.Fatalf("expected a commit; err=%v", err)
	}
	if err != nil {
		t.Fatalf("push after Sync still failed: %v", err)
	}

	// The remote now holds the PC's work and snorgd's, and the dropped commits are
	// still recoverable from the backup ref.
	remote, err := git.PlainOpen(f.bare)
	if err != nil {
		t.Fatal(err)
	}
	if got := commitCount(t, remote); got != 3 {
		t.Errorf("remote commit count = %d, want 3 (root, pc, snorgd)", got)
	}
	t.Logf("recovered; dropped work kept at %s: %v", res.Backup, res.Dropped)
}
