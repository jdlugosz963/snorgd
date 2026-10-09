package analyze

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/jdlugosz963/snorg/pkg/snorg"

	"github.com/jdlugosz963/snorgd/internal/query"
)

// Archive is what the stage needs of the snorg client: select pages, analyze pages.
// *snorg.Client implements it; the interface is here so the phase can be tested
// without an archive or a model, the same reason the dispatch stage takes one.
type Archive interface {
	Query(pred snorg.Predicate) ([]snorg.Match, error)
	Analyze(ctx context.Context, prov snorg.Provider, pageIDs []string, opts snorg.AnalyzeOptions) ([]snorg.AnalyzeResult, error)
}

// Phase is the ANALYZE stage: it implements daemon.Phase, running once per settled
// batch.
type Phase struct {
	arch Archive
	prov snorg.Provider
	pred snorg.Predicate
}

// NewPhase builds the stage from the configured rules. The Provider is the caller's:
// built once at startup and shared with every other stage that wants one, so
// credentials fail before the first note rather than on it.
func NewPhase(arch Archive, prov snorg.Provider, set Set) *Phase {
	return &Phase{arch: arch, prov: prov, pred: set.Predicate()}
}

// Name identifies the stage in logs and in the commit message.
func (p *Phase) Name() string { return "analyze" }

// Unit is what the count in the commit message counts.
func (p *Phase) Unit() string { return "page" }

// Run analyzes every selected page.
//
// The selection is deliberately not narrowed to unanalyzed pages: snorg skips a page
// whose rasterized ink still matches the fingerprint it stored, so an unchanged page
// costs no model call, and a page that was rewritten on the device is picked up again
// on its own. Selecting on `unanalyzed` would cost exactly that second pass.
//
// A page that fails is logged and the rest of the batch continues — one bad page must
// never cost the work the others did. The failures are returned joined so the daemon
// logs a summary too, but the count of work done is returned regardless, since that
// work really happened.
func (p *Phase) Run(ctx context.Context) (int, error) {
	pages, err := query.PageIDs(p.arch, p.pred)
	if err != nil {
		return 0, fmt.Errorf("analyze: query: %w", err)
	}
	if len(pages) == 0 {
		return 0, nil
	}

	// OnResult logs each page as it lands rather than after the whole batch, so a
	// long run says what it is doing while it does it. The tally comes from the
	// returned results, which a cancelled run still carries as far as it got.
	results, err := p.arch.Analyze(ctx, p.prov, pages, snorg.AnalyzeOptions{
		OnResult: func(r snorg.AnalyzeResult) {
			switch {
			case r.Err != nil:
				log.Printf("analyze %s: %v", r.PageID, r.Err)
			case r.Skipped:
				// Unchanged since the last pass: no model call, nothing written.
			default:
				log.Printf("analyze %s: %d call(s)", r.PageID, r.Calls)
			}
		},
	})

	var (
		done int
		errs []error
	)
	for _, r := range results {
		switch {
		case r.Err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", r.PageID, r.Err))
		case !r.Skipped:
			done++
		}
	}
	if err != nil {
		errs = append(errs, err)
	}
	return done, errors.Join(errs...)
}
