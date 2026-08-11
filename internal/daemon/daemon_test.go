package daemon

import (
	"errors"
	"os"
	"testing"

	"github.com/jdlugosz963/snorg/pkg/snorg"

	"github.com/jdlugosz963/snorgd/internal/watch"
)

type fakeIngester struct {
	note *snorg.Note
	err  error
	got  []string
}

func (f *fakeIngester) Ingest(localPath string) (*snorg.Note, error) {
	f.got = append(f.got, localPath)
	return f.note, f.err
}

type fakeCommitter struct {
	committed bool
	err       error
	msgs      []string
}

func (f *fakeCommitter) CommitPush(message string) (bool, error) {
	f.msgs = append(f.msgs, message)
	return f.committed, f.err
}

func tempFile(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "note-*.note")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	return f.Name()
}

func TestDaemonIngestsThenCommits(t *testing.T) {
	ing := &fakeIngester{note: &snorg.Note{FileID: "FID1"}}
	com := &fakeCommitter{committed: true}
	dm := New(ing, com)

	local := tempFile(t)
	dm.process(watch.Event{Source: "/Supernote/a.note", LocalPath: local})

	if len(ing.got) != 1 || ing.got[0] != local {
		t.Fatalf("ingest calls = %v, want [%s]", ing.got, local)
	}
	if len(com.msgs) != 1 || com.msgs[0] != "ingest: /Supernote/a.note" {
		t.Fatalf("commit msgs = %v", com.msgs)
	}
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Fatalf("temp file not cleaned up: %v", err)
	}
}

func TestDaemonSkipsCommitOnIngestError(t *testing.T) {
	ing := &fakeIngester{err: errors.New("supernote-tool missing")}
	com := &fakeCommitter{}
	dm := New(ing, com)

	local := tempFile(t)
	dm.process(watch.Event{Source: "/Supernote/bad.note", LocalPath: local})

	if len(com.msgs) != 0 {
		t.Fatalf("must not commit after a failed ingest; got %v", com.msgs)
	}
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Fatalf("temp file not cleaned up after failure: %v", err)
	}
}

func TestDaemonRunDrainsChannel(t *testing.T) {
	ing := &fakeIngester{note: &snorg.Note{FileID: "FID"}}
	com := &fakeCommitter{committed: true}
	dm := New(ing, com)

	events := make(chan watch.Event, 2)
	events <- watch.Event{Source: "a", LocalPath: tempFile(t)}
	events <- watch.Event{Source: "b", LocalPath: tempFile(t)}
	close(events)
	dm.Run(events)

	if len(ing.got) != 2 {
		t.Fatalf("processed %d events, want 2", len(ing.got))
	}
}
