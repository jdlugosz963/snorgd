// Package daemon is the source-agnostic core of snorgd: it queues watch.Events,
// ingests them in batches, runs the downstream stages once the queue has been quiet
// for a while, and finally commits and pushes the whole chain as one change.
//
// The batching is the point. A Supernote sync delivers a burst of changed notes; the
// old one-commit-per-note loop turned that into a commit storm and an analysis pass
// per note. Here a burst collapses into a single ingest, a single pass over the
// downstream phases, and a single commit.
//
// Its collaborators are interfaces so the loop is testable with fakes; concrete
// implementations (internal/ingest, internal/dispatch, internal/gitrepo) are wired
// in by cmd/snorgd.
package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jdlugosz963/snorgd/internal/gitrepo"
	"github.com/jdlugosz963/snorgd/internal/ingest"
	"github.com/jdlugosz963/snorgd/internal/rules"
	"github.com/jdlugosz963/snorgd/internal/watch"
)

// Ingester registers a batch of downloaded notes into the archive.
type Ingester interface {
	IngestBatch(ctx context.Context, items []ingest.Item) []ingest.Outcome
}

// Phase is a downstream pipeline stage, run once per settled batch. It reports how
// many units of work it did, which feeds the commit message; Unit names that unit in
// the singular ("page", "letter") so the daemon core can describe a stage it knows
// nothing else about.
type Phase interface {
	Name() string
	Unit() string
	Run(ctx context.Context) (int, error)
}

// Committer commits and pushes the archive's working tree, reporting whether a
// commit was made, and syncs it with the remote before a batch builds on it.
type Committer interface {
	Sync() (gitrepo.SyncResult, error)
	CommitPush(message string) (bool, error)
}

// Daemon batches events through the pipeline.
type Daemon struct {
	rules  rules.Set
	ing    Ingester
	phases []Phase
	repo   Committer

	quiet time.Duration
}

// New returns a Daemon that resolves events against rs, ingests via ing, runs phases
// in order, and commits via repo once the queue has been quiet for quiet.
func New(rs rules.Set, ing Ingester, phases []Phase, repo Committer, quiet time.Duration) *Daemon {
	return &Daemon{rules: rs, ing: ing, phases: phases, repo: repo, quiet: quiet}
}

// Run consumes events until the channel closes or ctx is cancelled.
func (dm *Daemon) Run(ctx context.Context, events <-chan watch.Event) {
	for {
		ev, ok := receive(ctx, events)
		if !ok {
			return
		}
		queued := dm.enqueue(nil, ev)
		if len(queued) == 0 {
			continue // no rule wanted it; nothing to start a cycle for
		}
		if !dm.cycle(ctx, events, queued) {
			return
		}
	}
}

// cycle drives one batch from its first note to a single commit, and reports whether
// the daemon should keep running.
//
// The contract, in order: ingest as soon as the queue drains; wait for the queue to
// stay empty for the quiet period, re-ingesting and restarting the wait whenever
// something new arrives; then run the phases; and only commit once nothing new is
// waiting — if something is, fold it in and go round again. One commit therefore
// covers the entire settled chain rather than each note that triggered it.
func (dm *Daemon) cycle(ctx context.Context, events <-chan watch.Event, queued []ingest.Item) bool {
	b := newBatch(dm.phases)
	alive := true

	dm.sync()

	for {
		// Freeing the entire queue, then ingesting it as one batch.
		queued = dm.drain(queued, events)
		dm.ingest(ctx, b, queued)
		queued = nil

		ev, why := waitQuiet(ctx, events, dm.quiet)
		if why == gotEvent {
			queued = dm.enqueue(queued, ev)
			continue // the quiet period restarts
		}
		if why == ctxCancelled {
			// Shutting down: skip the phases, which can be long, but still commit
			// what was already ingested so the work is not lost.
			alive = false
			break
		}

		// wentQuiet, or the source ended with work in hand: either way the batch
		// has earned its pipeline run.
		dm.runPhases(ctx, b)
		if why == sourceClosed {
			alive = false
			break
		}

		// The batch is only settled if nothing showed up while the phases ran.
		if ev, got := tryReceive(events); got {
			queued = dm.enqueue(queued, ev)
			continue
		}
		break
	}

	dm.commit(b)
	return alive
}

// runPhases runs each stage in order. A stage failure is logged and the chain
// continues: a failed analysis must not cost the commit of an ingest that worked.
func (dm *Daemon) runPhases(ctx context.Context, b *batch) {
	for _, p := range dm.phases {
		if ctx.Err() != nil {
			return
		}
		n, err := p.Run(ctx)
		if err != nil {
			log.Printf("%s: %v", p.Name(), err)
		}
		b.work[p.Name()] += n
	}
}

// enqueue resolves an event against the rules and appends it to items, discarding
// (and cleaning up) anything no rule wants.
func (dm *Daemon) enqueue(items []ingest.Item, ev watch.Event) []ingest.Item {
	rule, ok := dm.rules.Match(ev.Source)
	if !ok {
		log.Printf("skip %s: no matching ingest rule", ev.Source)
		discard(ev.LocalPath)
		return items
	}
	if rule.Skip {
		log.Printf("skip %s: rule %q", ev.Source, rule.Match)
		discard(ev.LocalPath)
		return items
	}
	return append(items, ingest.Item{LocalPath: ev.LocalPath, Source: ev.Source, Tags: rule.Tags})
}

// drain takes everything already buffered on the channel without blocking, so a
// burst becomes one batch rather than one batch per note.
func (dm *Daemon) drain(items []ingest.Item, events <-chan watch.Event) []ingest.Item {
	for {
		ev, ok := tryReceive(events)
		if !ok {
			return items
		}
		items = dm.enqueue(items, ev)
	}
}

// ingest registers a drained batch and records it against b. The temp dir holding
// each note is always removed, whether or not its ingest succeeded — the watcher
// creates one per note and hands ownership here.
func (dm *Daemon) ingest(ctx context.Context, b *batch, items []ingest.Item) {
	if len(items) == 0 {
		return
	}
	for _, out := range dm.ing.IngestBatch(ctx, items) {
		discard(out.Item.LocalPath)
		if out.Err != nil {
			log.Printf("ingest %s: %v", out.Item.Source, out.Err)
			continue
		}
		b.sources = append(b.sources, out.Item.Source)
		b.notes++
		if out.Note != nil {
			log.Printf("ingested %s (%s)", out.Item.Source, out.Note.FileID)
		}
	}
}

// sync brings the archive in line with the remote before the batch writes to it, so
// the batch's own commit lands on the current remote tip and pushes as a
// fast-forward. It runs first for that reason: snorg writes the archive throughout
// ingest and analyze, and a reset afterwards would discard the batch's own work.
//
// A failure is logged and the batch proceeds anyway. Returning early would be worse
// than the failure: the notes have already been downloaded and the Dropbox cursor has
// already moved past them, so abandoning the batch loses them outright, whereas
// committing locally leaves the work in the repo for the next sync to deal with.
func (dm *Daemon) sync() {
	res, err := dm.repo.Sync()
	switch {
	case err != nil:
		log.Printf("sync: %v (continuing; this batch may fail to push)", err)
	case res.Backup != "":
		// The remote won a divergence. Name what was dropped: each subject is a
		// batch whose notes are no longer in the archive, and the Dropbox cursor
		// will not offer them again.
		log.Printf("sync: local diverged from the remote; reset to it and saved %s under %s",
			plural(len(res.Dropped), "dropped commit"), res.Backup)
		for _, subject := range res.Dropped {
			log.Printf("sync: dropped %q — recover it with: git cherry-pick $(git rev-parse %s)",
				subject, res.Backup)
		}
	case res.FastForwarded:
		log.Printf("sync: fast-forwarded to the remote")
	}
}

// commit pushes the whole chain as one change. A clean tree is not an error: snorg
// reconciles in place, so a resaved-but-unchanged note legitimately produces nothing
// to commit.
func (dm *Daemon) commit(b *batch) {
	committed, err := dm.repo.CommitPush(b.message())
	if err != nil {
		log.Printf("commit: %v", err)
		return
	}
	if committed {
		log.Printf("pushed %s", b.summary())
	} else {
		log.Printf("no change after %s", b.summary())
	}
}

// batch accumulates what one commit will describe.
type batch struct {
	sources []string       // note paths ingested, for the commit body
	notes   int            // notes that actually landed in the archive
	phases  []Phase        // in pipeline order, so the summary reads in that order
	work    map[string]int // phase name -> units of work done
}

func newBatch(phases []Phase) *batch { return &batch{phases: phases, work: map[string]int{}} }

// summary is the one-line description of what the batch did: the notes ingested,
// then each phase that did something, named by the phase itself. Phases name their
// own unit so the daemon core needs no knowledge of what any of them do.
func (b *batch) summary() string {
	parts := []string{plural(b.notes, "note")}
	for _, p := range b.phases {
		if n := b.work[p.Name()]; n > 0 {
			parts = append(parts, plural(n, p.Unit()))
		}
	}
	return strings.Join(parts, ", ")
}

// message is the commit message: a summary line, then the notes it covers.
func (b *batch) message() string {
	msg := "snorgd: " + b.summary()
	if len(b.sources) > 0 {
		msg += "\n\n" + strings.Join(b.sources, "\n")
	}
	return msg
}

func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// receive blocks for the next event, reporting false when the source ended or ctx
// was cancelled.
func receive(ctx context.Context, events <-chan watch.Event) (watch.Event, bool) {
	select {
	case ev, ok := <-events:
		return ev, ok
	case <-ctx.Done():
		return watch.Event{}, false
	}
}

// tryReceive takes an event only if one is already waiting.
func tryReceive(events <-chan watch.Event) (watch.Event, bool) {
	select {
	case ev, ok := <-events:
		return ev, ok
	default:
		return watch.Event{}, false
	}
}

// waitOutcome is why a quiet-period wait ended.
type waitOutcome int

const (
	gotEvent     waitOutcome = iota // more work arrived; the wait restarts
	wentQuiet                       // the period elapsed with an empty queue
	sourceClosed                    // every watcher stopped
	ctxCancelled                    // the daemon is shutting down
)

// waitQuiet waits up to d for another event, reporting why the wait ended.
//
// A closed source and a cancelled context are deliberately different: the first
// still deserves a full pipeline run over what was ingested, while the second means
// shutdown and must not start a long analysis.
func waitQuiet(ctx context.Context, events <-chan watch.Event, d time.Duration) (watch.Event, waitOutcome) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case e, ok := <-events:
		if !ok {
			return watch.Event{}, sourceClosed
		}
		return e, gotEvent
	case <-timer.C:
		return watch.Event{}, wentQuiet
	case <-ctx.Done():
		return watch.Event{}, ctxCancelled
	}
}

// discard removes the temp dir a downloaded note lives in.
func discard(localPath string) {
	if localPath == "" {
		return
	}
	os.RemoveAll(filepath.Dir(localPath))
}
