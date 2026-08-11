// Package watch defines the source-agnostic seam between a note source and the
// daemon core: a Watcher streams Event values that the daemon ingests. Dropbox is
// today's only Watcher (internal/dropbox); a filesystem/inotify source would be
// another implementation of the same interface, leaving the core untouched.
package watch

import "context"

// Event is one changed .note surfaced by a Watcher, already downloaded to a local
// temp file. The daemon core ingests LocalPath and uses Source for the commit
// message; LocalPath's parent temp dir is removed after processing.
type Event struct {
	Watcher   string // which watcher produced this (for logs)
	Source    string // the note's path/name in the watched store
	LocalPath string // path to a temp copy of the .note bytes to ingest
}

// Watcher streams .note change events until ctx is cancelled. It is the seam that
// isolates the note source (Dropbox today; a filesystem/inotify watcher could be
// added as another implementation without touching the daemon core). Watch must
// return when ctx is done; a returned error ends the daemon.
type Watcher interface {
	Name() string
	Watch(ctx context.Context, out chan<- Event) error
}
