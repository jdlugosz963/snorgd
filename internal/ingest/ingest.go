// Package ingest wraps a snorg.Client and registers downloaded .note files into the
// archive, honouring the archive's own config.yaml (ingest.svg toggles, templates)
// and applying the tags an ingest rule matched.
package ingest

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/jdlugosz963/snorg/pkg/snorg"
)

// Ingestor registers .note files into one archive.
type Ingestor struct{ client *snorg.Client }

// Item is one note queued for ingest: the downloaded bytes, where it came from (for
// logs and the commit message), and the tags its matched rule assigns.
type Item struct {
	LocalPath string
	Source    string
	Tags      []string
}

// Outcome is one note's ingest result. Err is set when that note failed; the rest of
// the batch is unaffected, so a single corrupt .note never costs a whole sync.
type Outcome struct {
	Item   Item
	Note   *snorg.Note
	Report *snorg.WriteReport
	Err    error
}

// New opens the archive at archive, applying its config.yaml if present.
func New(archive string) (*Ingestor, error) {
	var cfg *snorg.Config
	cfgPath := filepath.Join(archive, "config.yaml")
	if st, err := os.Stat(cfgPath); err == nil && !st.IsDir() {
		if cfg, err = snorg.LoadConfig([]string{cfgPath}); err != nil {
			return nil, fmt.Errorf("load %s: %w", cfgPath, err)
		}
	}
	client, err := snorg.Open(archive, cfg)
	if err != nil {
		return nil, err
	}
	return &Ingestor{client: client}, nil
}

// Client exposes the underlying snorg client so the downstream phases read
// and write the same archive through the same configuration (templates in
// particular, without which region queries resolve to nothing).
func (i *Ingestor) Client() *snorg.Client { return i.client }

// Migrate upgrades every note in the archive to the current snorg schema, returning
// only the entries that were actually upgraded.
//
// This is not optional maintenance: snorg hard-rejects a note written by an older
// schema, and that error surfaces from every read — query, retrieve, render. An
// archive last written by snorg v0.3.1 (schema 1) is unreadable by the current
// version (schema 5) until this runs.
func (i *Ingestor) Migrate(ctx context.Context) ([]snorg.MigrateResult, error) {
	res, err := i.client.MigrateAll(ctx, snorg.MigrateOptions{})
	if err != nil {
		return nil, err
	}
	upgraded := res[:0]
	for _, r := range res {
		if r.Outcome == snorg.MigrateUpgraded {
			upgraded = append(upgraded, r)
		}
	}
	return upgraded, nil
}

// IngestBatch registers every item in one snorg call and applies each item's rule
// tags to the resulting note. Results are returned in input order, one per item.
//
// Tags are applied note-scoped rather than per page: a rule matches a *file*, and a
// note tag is inherited by every page of that note, which is what lets the dispatch
// stage narrow its selection with a `tag` query.
func (i *Ingestor) IngestBatch(ctx context.Context, items []Item) []Outcome {
	if len(items) == 0 {
		return nil
	}

	paths := make([]string, len(items))
	for n, it := range items {
		paths[n] = it.LocalPath
	}

	out := make([]Outcome, len(items))
	for n, it := range items {
		out[n] = Outcome{Item: it}
	}

	// snorg registers notes one at a time (Client.Ingest → ingest.RunMany); results
	// come back in input order, and a note's failure is reported in its own Result
	// rather than aborting the batch.
	res, err := i.client.Ingest(ctx, paths, snorg.IngestOptions{})
	if err != nil {
		// A top-level error is a configuration failure, not a per-note one: it
		// applies to every item in the batch.
		for n := range out {
			out[n].Err = err
		}
		return out
	}
	if len(res) != len(items) {
		for n := range out {
			out[n].Err = fmt.Errorf("snorg returned %d results for %d notes", len(res), len(items))
		}
		return out
	}

	for n, r := range res {
		if r.Err != nil {
			out[n].Err = r.Err
			continue
		}
		out[n].Note = r.Note
		out[n].Report = r.Report
		if r.Note == nil {
			continue
		}
		if err := i.tag(r.Note.FileID, items[n].Tags); err != nil {
			// The note is in the archive; only its labelling failed. Report it
			// without discarding the successful ingest.
			log.Printf("tag %s: %v", items[n].Source, err)
		}
	}
	return out
}

// tag applies each tag to the note, skipping ones already present (snorg reports a
// no-op add as zero changes and writes nothing).
func (i *Ingestor) tag(fileID string, tags []string) error {
	for _, t := range tags {
		if _, err := i.client.TagNote([]string{fileID}, t); err != nil {
			return fmt.Errorf("%q: %w", t, err)
		}
	}
	return nil
}
