// Package daemon is the source-agnostic core of snorgd: it drains watch.Events,
// ingests each note into the archive, and commits+pushes the change. Its
// collaborators are interfaces so the loop is testable with fakes; concrete
// implementations (internal/ingest, internal/gitrepo) are wired in by cmd/snorgd.
package daemon

import (
	"log"
	"os"
	"path/filepath"

	"github.com/jdlugosz963/snorg/pkg/snorg"

	"github.com/jdlugosz963/snorgd/internal/watch"
)

// Ingester registers a downloaded .note into the archive.
type Ingester interface {
	Ingest(localPath string) (*snorg.Note, error)
}

// Committer commits and pushes the archive's working tree, reporting whether a
// commit was made.
type Committer interface {
	CommitPush(message string) (bool, error)
}

// Daemon drains Events, ingests each note, and commits+pushes the change.
type Daemon struct {
	ing  Ingester
	repo Committer
}

// New returns a Daemon that ingests via ing and commits via repo.
func New(ing Ingester, repo Committer) *Daemon {
	return &Daemon{ing: ing, repo: repo}
}

// Run processes events until the channel is closed (all watchers stopped).
func (dm *Daemon) Run(events <-chan watch.Event) {
	for ev := range events {
		dm.process(ev)
	}
}

// process ingests one event's note and commits the result. A failure is logged and
// swallowed so one bad note never stops the daemon; the temp dir holding the note is
// always removed.
func (dm *Daemon) process(ev watch.Event) {
	defer os.RemoveAll(filepath.Dir(ev.LocalPath))

	note, err := dm.ing.Ingest(ev.LocalPath)
	if err != nil {
		log.Printf("ingest %s: %v", ev.Source, err)
		return
	}
	committed, err := dm.repo.CommitPush("ingest: " + ev.Source)
	if err != nil {
		log.Printf("commit %s: %v", ev.Source, err)
		return
	}
	if committed {
		log.Printf("pushed %s (%s)", ev.Source, note.FileID)
	} else {
		log.Printf("no change for %s", ev.Source)
	}
}
