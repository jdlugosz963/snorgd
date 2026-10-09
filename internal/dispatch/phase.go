package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/jdlugosz963/snif/pkg/snif"
	"github.com/jdlugosz963/snorg/pkg/snorg"

	"github.com/jdlugosz963/snorgd/internal/formstate"
	"github.com/jdlugosz963/snorgd/internal/query"
)

// Reader reads pages back as snif forms. *snif.Client is the implementation; the
// interface is here so the phase can be tested without an archive.
type Reader interface {
	Forms(ctx context.Context, pageIDs []string, opts snif.ReadOptions) []snif.Result
}

// Store is the dispatcher's memory of what each page said last time.
// *formstate.Store implements it.
type Store interface {
	Diff(f *snif.Form) (formstate.State, error)
	Commit(st formstate.State) error
}

// Phase is the DISPATCH stage: it implements daemon.Phase, running once per settled
// batch.
type Phase struct {
	reader Reader
	sel    query.Selector
	store  Store
	reg    *Registry
	pred   snorg.Predicate
}

// NewPhase builds the stage. The query selects which pages are read — "templated",
// every page drawn on a configured template, is the default and the right answer
// almost always; narrowing it is how a large archive stays cheap, since each selected
// page costs one rasterization.
func NewPhase(reader Reader, sel query.Selector, store Store, reg *Registry, q string) (*Phase, error) {
	if reg == nil || reg.Len() == 0 {
		return nil, fmt.Errorf("dispatch: no templates registered")
	}
	pred, err := query.Parse(q)
	if err != nil {
		return nil, fmt.Errorf("dispatch: query: %w", err)
	}
	return &Phase{reader: reader, sel: sel, store: store, reg: reg, pred: pred}, nil
}

// Name identifies the stage in logs and in the commit message.
func (p *Phase) Name() string { return "dispatch" }

// Unit is what the count in the commit message counts.
func (p *Phase) Unit() string { return "page" }

// Run reads every selected page and hands the ones that moved to their handler.
//
// A page that fails — unreadable, or a handler that errored — is logged and the rest
// of the batch continues: one bad page must never cost the work the others did. The
// failures are returned joined so the daemon logs a summary too, but the count of
// work done is returned regardless, since that work really happened.
func (p *Phase) Run(ctx context.Context) (int, error) {
	pages, err := p.pageIDs()
	if err != nil {
		return 0, err
	}
	if len(pages) == 0 {
		return 0, nil
	}

	// Analyze: true lets snif transcribe an Input widget nobody has read yet, which
	// is what makes a freshly ingested page readable at all. It costs a vision call
	// only for a page that actually carries an untranscribed Input.
	var (
		done int
		errs []error
	)
	for _, res := range p.reader.Forms(ctx, pages, snif.ReadOptions{Analyze: true}) {
		if ctx.Err() != nil {
			return done, ctx.Err()
		}
		n, err := p.page(ctx, res)
		done += n
		if err != nil {
			log.Printf("dispatch %s: %v", res.PageID, err)
			errs = append(errs, err)
		}
	}

	// Every page of the batch has been handled and its state committed, so anything
	// a handler deferred to the end of the run happens now. The work each page did
	// is already counted and already safe: a flush that fails is logged, joined and
	// otherwise costs the batch nothing.
	if ctx.Err() == nil {
		errs = append(errs, p.flush(ctx)...)
	}
	return done, errors.Join(errs...)
}

// flush gives each handler that wants one its end-of-batch call.
func (p *Phase) flush(ctx context.Context) []error {
	var errs []error
	for _, h := range p.reg.Handlers() {
		f, ok := h.(Flusher)
		if !ok {
			continue
		}
		if err := f.Flush(ctx); err != nil {
			log.Printf("dispatch: flush %T: %v", h, err)
			errs = append(errs, err)
		}
	}
	return errs
}

// page handles one read result.
func (p *Phase) page(ctx context.Context, res snif.Result) (int, error) {
	switch {
	case errors.Is(res.Err, snif.ErrNotTemplated), errors.Is(res.Err, snif.ErrNotSnif):
		// Not a form at all, or a snorg template that declares no widgets. The
		// query selects templated pages, not snif ones, so this is the ordinary
		// case for an archive holding both — not a failure.
		return 0, nil
	case res.Err != nil:
		return 0, res.Err
	case res.Form == nil:
		return 0, fmt.Errorf("read returned neither a form nor an error")
	}

	// An empty name means the template could not be resolved, not that the page was
	// drawn on a template called "". Routing on it would be routing on a guess.
	name := res.Form.Template
	if name == "" {
		return 0, nil
	}
	h, ok := p.reg.Handler(name)
	if !ok {
		return 0, nil // a snif form snorgd does not own
	}

	st, err := p.store.Diff(res.Form)
	if err != nil {
		return 0, err
	}
	if !st.Changed() {
		return 0, nil
	}

	n, err := h.Handle(ctx, Event{
		PageID: res.Form.PageID,
		FileID: res.Form.FileID,
		Form:   res.Form,
		State:  st,
	})
	if err != nil {
		// Deliberately not committing: the state only advances past a change once
		// something has successfully acted on it, so a failure is retried on the
		// next batch instead of being swallowed.
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	if err := p.store.Commit(st); err != nil {
		// The work is done and must be counted; failing to remember it means it
		// will be done again, which is worth saying loudly.
		return n, err
	}
	return n, nil
}

// pageIDs resolves the configured selection to page ids.
func (p *Phase) pageIDs() ([]string, error) {
	ids, err := query.PageIDs(p.sel, p.pred)
	if err != nil {
		return nil, fmt.Errorf("dispatch: query: %w", err)
	}
	return ids, nil
}
