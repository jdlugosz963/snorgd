package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jdlugosz963/snorg/pkg/snorg"

	"github.com/jdlugosz963/snorgd/internal/gitrepo"
	"github.com/jdlugosz963/snorgd/internal/ingest"
	"github.com/jdlugosz963/snorgd/internal/rules"
	"github.com/jdlugosz963/snorgd/internal/watch"
)

// quiet is short enough to keep tests fast and long enough that a burst queued up
// front is reliably seen as one batch.
const quiet = 30 * time.Millisecond

// fakeIngester records each batch it was handed.
type fakeIngester struct {
	mu       sync.Mutex
	batches  [][]string // sources per IngestBatch call
	err      error
	recorder *recorder // optional, shared with the committer to check ordering
}

func (f *fakeIngester) IngestBatch(_ context.Context, items []ingest.Item) []ingest.Outcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorder.record("ingest")
	var sources []string
	out := make([]ingest.Outcome, len(items))
	for i, it := range items {
		sources = append(sources, it.Source)
		out[i] = ingest.Outcome{Item: it, Err: f.err}
		if f.err == nil {
			out[i].Note = &snorg.Note{FileID: "F" + it.Source}
		}
	}
	f.batches = append(f.batches, sources)
	return out
}

func (f *fakeIngester) calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.batches...)
}

// fakePhase counts runs and can signal when it starts, so a test can inject an event
// mid-phase.
type fakePhase struct {
	name    string
	unit    string
	mu      sync.Mutex
	runs    int
	work    int
	onStart func()
}

func (p *fakePhase) Name() string { return p.name }

func (p *fakePhase) Unit() string {
	if p.unit == "" {
		return "thing"
	}
	return p.unit
}

func (p *fakePhase) Run(context.Context) (int, error) {
	p.mu.Lock()
	p.runs++
	p.mu.Unlock()
	if p.onStart != nil {
		p.onStart()
	}
	return p.work, nil
}

func (p *fakePhase) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runs
}

// fakeCommitter records commit messages, and the sync that must precede them.
type fakeCommitter struct {
	mu       sync.Mutex
	msgs     []string
	err      error
	syncs    int
	syncErr  error
	syncRes  gitrepo.SyncResult
	recorder *recorder // optional, shared with the ingester to check ordering
}

func (f *fakeCommitter) Sync() (gitrepo.SyncResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncs++
	f.recorder.record("sync")
	return f.syncRes, f.syncErr
}

func (f *fakeCommitter) syncCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.syncs
}

func (f *fakeCommitter) CommitPush(message string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, message)
	return f.err == nil, f.err
}

// recorder is a shared, ordered log of which fake was called when — enough to assert
// that the sync happens before the archive is written to, which is the whole point of
// where it sits in the cycle.
type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recorder) record(name string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, name)
}

func (r *recorder) ordered() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func (f *fakeCommitter) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.msgs...)
}

// event creates a temp dir holding a note file, mimicking what a watcher hands over.
func event(t *testing.T, source string) watch.Event {
	t.Helper()
	dir, err := os.MkdirTemp(t.TempDir(), "note-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	local := filepath.Join(dir, filepath.Base(source))
	if err := os.WriteFile(local, []byte("note bytes"), 0o600); err != nil {
		t.Fatalf("write note: %v", err)
	}
	return watch.Event{Watcher: "test", Source: source, LocalPath: local}
}

// run drives the daemon over events and waits for it to finish.
func run(t *testing.T, dm *Daemon, events chan watch.Event) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		dm.Run(context.Background(), events)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not finish")
	}
}

func TestBurstBecomesOneBatchAndOneCommit(t *testing.T) {
	ing := &fakeIngester{}
	dispatch := &fakePhase{name: "dispatch", unit: "page", work: 5}
	letter := &fakePhase{name: "letter", unit: "letter", work: 1}
	repo := &fakeCommitter{}

	dm := New(nil, ing, []Phase{dispatch, letter}, repo, quiet)

	events := make(chan watch.Event, 8)
	for _, src := range []string{"/Supernote/a.note", "/Supernote/b.note", "/Supernote/c.note"} {
		events <- event(t, src)
	}
	close(events)
	run(t, dm, events)

	// Three notes queued at once must be ingested together, not one at a time.
	calls := ing.calls()
	if len(calls) != 1 {
		t.Fatalf("IngestBatch called %d times with %v, want 1 batch", len(calls), calls)
	}
	if len(calls[0]) != 3 {
		t.Errorf("batch = %v, want all 3 notes", calls[0])
	}
	if got := repo.messages(); len(got) != 1 {
		t.Fatalf("commits = %d (%v), want 1", len(got), got)
	}
	want := "snorgd: 3 notes, 5 pages, 1 letter"
	if got := repo.messages()[0]; got[:len(want)] != want {
		t.Errorf("commit summary = %q, want prefix %q", got, want)
	}
}

func TestEventDuringPhasesDefersTheCommit(t *testing.T) {
	ing := &fakeIngester{}
	repo := &fakeCommitter{}
	events := make(chan watch.Event, 4)

	// The first time the phase runs, a new note shows up. The batch is not settled,
	// so the daemon must fold it in and re-run rather than commit.
	late := &fakePhase{name: "dispatch"}
	var once sync.Once
	late.onStart = func() {
		once.Do(func() { events <- event(t, "/Supernote/late.note") })
	}

	dm := New(nil, ing, []Phase{late}, repo, quiet)
	events <- event(t, "/Supernote/first.note")

	done := make(chan struct{})
	go func() {
		dm.Run(context.Background(), events)
		close(done)
	}()
	// Give the cycle time to ingest, debounce, run the phase, absorb the late note,
	// debounce again and settle.
	time.Sleep(20 * quiet)
	close(events)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not finish")
	}

	// The late note was ingested in the same cycle...
	calls := ing.calls()
	if len(calls) != 2 {
		t.Fatalf("IngestBatch calls = %v, want 2 (first note, then the late one)", calls)
	}
	// ...the phase ran again for it...
	if got := late.count(); got < 2 {
		t.Errorf("phase ran %d times, want at least 2", got)
	}
	// ...and it all landed in a single commit.
	if got := repo.messages(); len(got) != 1 {
		t.Fatalf("commits = %d (%v), want 1 covering both notes", len(got), got)
	}
	if msg := repo.messages()[0]; msg[:len("snorgd: 2 notes")] != "snorgd: 2 notes" {
		t.Errorf("commit = %q, want it to cover 2 notes", msg)
	}
}

func TestRulesSelectTagsAndDropUnmatchedNotes(t *testing.T) {
	ing := &fakeIngester{}
	repo := &fakeCommitter{}
	set := rules.Set{
		{Match: "/Supernote/Scratch/**", Skip: true},
		{Match: "/Supernote/AI/*.note", Tags: []string{"ai"}},
	}
	dm := New(set, ing, nil, repo, quiet)

	events := make(chan watch.Event, 4)
	kept := event(t, "/Supernote/AI/q.note")
	skipped := event(t, "/Supernote/Scratch/tmp.note")
	unmatched := event(t, "/Elsewhere/x.note")
	events <- kept
	events <- skipped
	events <- unmatched
	close(events)
	run(t, dm, events)

	calls := ing.calls()
	if len(calls) != 1 || len(calls[0]) != 1 || calls[0][0] != "/Supernote/AI/q.note" {
		t.Fatalf("ingested %v, want only the AI note", calls)
	}

	// Skipped and unmatched notes must still have their temp dirs cleaned up.
	for _, ev := range []watch.Event{skipped, unmatched} {
		if _, err := os.Stat(filepath.Dir(ev.LocalPath)); !os.IsNotExist(err) {
			t.Errorf("temp dir for %s survived (err=%v)", ev.Source, err)
		}
	}
}

func TestIngestedNotesAreAlwaysCleanedUp(t *testing.T) {
	ing := &fakeIngester{err: errors.New("corrupt note")}
	repo := &fakeCommitter{}
	dm := New(nil, ing, nil, repo, quiet)

	events := make(chan watch.Event, 2)
	ev := event(t, "/Supernote/bad.note")
	events <- ev
	close(events)
	run(t, dm, events)

	if _, err := os.Stat(filepath.Dir(ev.LocalPath)); !os.IsNotExist(err) {
		t.Errorf("temp dir survived a failed ingest (err=%v)", err)
	}
	// A failed ingest still ends the cycle with a commit attempt; the tree is clean
	// so nothing is actually committed.
	if got := repo.messages(); len(got) != 1 {
		t.Fatalf("commits = %v, want 1 attempt", got)
	}
	if want := "snorgd: 0 notes"; repo.messages()[0] != want {
		t.Errorf("commit = %q, want %q", repo.messages()[0], want)
	}
}

func TestNilPhaseListIsSkipped(t *testing.T) {
	ing := &fakeIngester{}
	repo := &fakeCommitter{}
	dm := New(nil, ing, nil, repo, quiet)

	events := make(chan watch.Event, 2)
	events <- event(t, "/Supernote/a.note")
	close(events)
	run(t, dm, events)

	if got := repo.messages(); len(got) != 1 || got[0][:len("snorgd: 1 note")] != "snorgd: 1 note" {
		t.Errorf("commit = %v, want a single-note commit with no phase counts", got)
	}
}

func TestCancelledContextStopsTheDaemon(t *testing.T) {
	ing := &fakeIngester{}
	repo := &fakeCommitter{}
	dm := New(nil, ing, nil, repo, time.Hour) // never settles on its own

	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan watch.Event, 2)
	events <- event(t, "/Supernote/a.note")

	done := make(chan struct{})
	go func() {
		dm.Run(ctx, events)
		close(done)
	}()

	time.Sleep(10 * quiet)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop on context cancellation")
	}

	// The note was ingested before the cancel, so it must still be committed rather
	// than silently lost.
	if got := repo.messages(); len(got) != 1 {
		t.Fatalf("commits = %v, want the ingested work to be committed on shutdown", got)
	}
}

// The sync must land before the archive is written to. snorg writes files throughout
// ingest and the phases, so a sync (and its possible hard reset) afterwards would
// discard the batch's own work.
func TestCycleSyncsBeforeIngesting(t *testing.T) {
	rec := &recorder{}
	ing := &fakeIngester{recorder: rec}
	repo := &fakeCommitter{recorder: rec}

	dm := New(nil, ing, nil, repo, quiet)

	events := make(chan watch.Event, 2)
	events <- event(t, "/Supernote/a.note")
	close(events)
	run(t, dm, events)

	calls := rec.ordered()
	if len(calls) < 2 {
		t.Fatalf("calls = %v, want at least a sync and an ingest", calls)
	}
	if calls[0] != "sync" {
		t.Errorf("calls = %v, want the sync first", calls)
	}
	if repo.syncCount() != 1 {
		t.Errorf("Sync called %d times, want once per cycle", repo.syncCount())
	}
}

// A sync failure must not abandon the batch: the notes are already downloaded and the
// Dropbox cursor has already moved past them, so skipping would lose them outright.
func TestCycleProceedsWhenSyncFails(t *testing.T) {
	ing := &fakeIngester{}
	repo := &fakeCommitter{syncErr: errors.New("network unreachable")}

	dm := New(nil, ing, nil, repo, quiet)

	events := make(chan watch.Event, 2)
	events <- event(t, "/Supernote/a.note")
	close(events)
	run(t, dm, events)

	if calls := ing.calls(); len(calls) != 1 {
		t.Fatalf("IngestBatch called %d times, want the batch to proceed anyway", len(calls))
	}
	if msgs := repo.messages(); len(msgs) != 1 {
		t.Errorf("CommitPush called %d times, want the work committed locally", len(msgs))
	}
}
