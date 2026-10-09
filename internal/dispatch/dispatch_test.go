package dispatch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jdlugosz963/snif/pkg/snif"
	"github.com/jdlugosz963/snorg/pkg/snorg"

	"github.com/jdlugosz963/snorgd/internal/formstate"
)

// fakeHandler records what it was asked to do and answers with what it was told to.
type fakeHandler struct {
	name string
	work int
	err  error

	mu     sync.Mutex
	calls  int
	events []Event
}

func (h *fakeHandler) Template() Template {
	return Template{Form: snif.Template{Name: h.name}}
}

func (h *fakeHandler) Handle(_ context.Context, ev Event) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	h.events = append(h.events, ev)
	return h.work, h.err
}

func (h *fakeHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// fakeReader returns canned read results, ignoring the ids it is given.
type fakeReader struct{ results []snif.Result }

func (r *fakeReader) Forms(context.Context, []string, snif.ReadOptions) []snif.Result {
	return r.results
}

// fakeSelector answers the page query.
type fakeSelector struct {
	matches []snorg.Match
	err     error
}

func (s *fakeSelector) Query(snorg.Predicate) ([]snorg.Match, error) {
	return s.matches, s.err
}

// fakeStore is an in-memory formstate.Store.
type fakeStore struct {
	mu        sync.Mutex
	committed map[string]formstate.State
	changed   bool
	diffErr   error
	commitErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{committed: map[string]formstate.State{}, changed: true}
}

func (s *fakeStore) Diff(f *snif.Form) (formstate.State, error) {
	if s.diffErr != nil {
		return formstate.State{}, s.diffErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := formstate.State{
		Template: f.Template, PageID: f.PageID, FileID: f.FileID,
		Fields: map[string]formstate.Field{},
	}
	_, already := s.committed[f.PageID]
	for name, v := range f.Values() {
		st.Fields[name] = formstate.Field{Value: v, Changed: s.changed && !already}
	}
	st.First = !already
	return st, nil
}

func (s *fakeStore) Commit(st formstate.State) error {
	if s.commitErr != nil {
		return s.commitErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.committed[st.PageID] = st
	return nil
}

func (s *fakeStore) commits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.committed)
}

// result builds a successful read of a one-widget form on the named template.
func result(pageID, template string) snif.Result {
	f := &snif.Form{
		PageID: pageID, FileID: "F1", Template: template,
		Widgets: []snif.Widget{{Kind: snif.Input, Name: "prompt", Text: "hi", Set: true}},
	}
	return snif.Result{PageID: pageID, Form: f}
}

// phase wires a phase around canned results.
func phase(t *testing.T, store Store, results []snif.Result, hs ...Handler) *Phase {
	t.Helper()
	reg, err := NewRegistry(hs...)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	matches := make([]snorg.Match, len(results))
	for i, r := range results {
		matches[i] = snorg.Match{PageID: r.PageID}
	}
	p, err := NewPhase(&fakeReader{results: results}, &fakeSelector{matches: matches}, store, reg, "templated")
	if err != nil {
		t.Fatalf("NewPhase: %v", err)
	}
	return p
}

func TestNewRegistryRejectsTwoHandlersForOneTemplate(t *testing.T) {
	if _, err := NewRegistry(&fakeHandler{name: "letter"}, &fakeHandler{name: "letter"}); err == nil {
		t.Fatal("NewRegistry accepted two handlers claiming one template")
	}
}

func TestNewRegistryRejectsANamelessTemplate(t *testing.T) {
	if _, err := NewRegistry(&fakeHandler{name: "  "}); err == nil {
		t.Fatal("NewRegistry accepted a template with no name")
	}
}

func TestNewRegistrySkipsNilHandlers(t *testing.T) {
	// A constructor that returns nothing when its stage is disabled can be passed
	// straight through without the caller unwrapping it.
	reg, err := NewRegistry(nil, &fakeHandler{name: "letter"}, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if reg.Len() != 1 {
		t.Errorf("Len() = %d, want 1", reg.Len())
	}
}

func TestNewPhaseNeedsATemplateAndAValidQuery(t *testing.T) {
	empty, err := NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if _, err := NewPhase(nil, nil, nil, empty, "templated"); err == nil {
		t.Error("NewPhase accepted an empty registry")
	}
	reg, err := NewRegistry(&fakeHandler{name: "letter"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if _, err := NewPhase(nil, nil, nil, reg, ""); err == nil {
		t.Error("NewPhase accepted an empty query")
	}
}

func TestRunHandsAChangedPageToItsHandler(t *testing.T) {
	h := &fakeHandler{name: "letter", work: 1}
	store := newFakeStore()
	p := phase(t, store, []snif.Result{result("P1", "letter")}, h)

	n, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != 1 {
		t.Errorf("Run reported %d units, want 1", n)
	}
	if h.count() != 1 {
		t.Fatalf("handler called %d times, want 1", h.count())
	}
	ev := h.events[0]
	if ev.PageID != "P1" || ev.Form == nil {
		t.Errorf("event = %+v, want the page and its form", ev)
	}
	if !ev.Changed("prompt") {
		t.Error("the event does not report the changed field")
	}
	if store.commits() != 1 {
		t.Error("a successful handler did not advance the stored state")
	}
}

func TestRunSkipsAPageThatDidNotChange(t *testing.T) {
	h := &fakeHandler{name: "letter", work: 1}
	store := newFakeStore()
	store.changed = false
	p := phase(t, store, []snif.Result{result("P1", "letter")}, h)

	n, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != 0 || h.count() != 0 {
		t.Errorf("an unchanged page reached the handler (%d calls, %d units)", h.count(), n)
	}
	if store.commits() != 0 {
		t.Error("an unchanged page was committed, which would hide a later change")
	}
}

func TestRunSkipsATemplateNobodyOwns(t *testing.T) {
	h := &fakeHandler{name: "letter"}
	store := newFakeStore()
	p := phase(t, store, []snif.Result{result("P1", "someone-elses-form")}, h)

	n, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("an unowned template was reported as a failure: %v", err)
	}
	if n != 0 || h.count() != 0 {
		t.Error("an unowned template reached the handler")
	}
}

func TestRunSkipsAPageWhoseTemplateCouldNotBeResolved(t *testing.T) {
	// snif reports an unresolvable template as an empty name, not as an error.
	// Routing on "" would route on a guess.
	h := &fakeHandler{name: ""}
	store := newFakeStore()
	reg, err := NewRegistry(&fakeHandler{name: "letter"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	results := []snif.Result{result("P1", "")}
	p, err := NewPhase(&fakeReader{results: results}, &fakeSelector{matches: []snorg.Match{{PageID: "P1"}}}, store, reg, "templated")
	if err != nil {
		t.Fatalf("NewPhase: %v", err)
	}
	if n, err := p.Run(context.Background()); n != 0 || err != nil {
		t.Errorf("Run = %d, %v; want a silent skip", n, err)
	}
	if h.count() != 0 {
		t.Error("a page with no resolved template reached a handler")
	}
}

func TestRunIgnoresPagesThatAreNotForms(t *testing.T) {
	// The query selects templated pages, not snif ones, so an archive holding both
	// yields these on every batch. They are the ordinary case, not failures.
	h := &fakeHandler{name: "letter"}
	store := newFakeStore()
	p := phase(t, store, []snif.Result{
		{PageID: "P1", Err: fmt.Errorf("page P1: %w", snif.ErrNotTemplated)},
		{PageID: "P2", Err: fmt.Errorf("page P2: %w", snif.ErrNotSnif)},
	}, h)

	if n, err := p.Run(context.Background()); n != 0 || err != nil {
		t.Errorf("Run = %d, %v; want a silent skip", n, err)
	}
}

func TestRunReportsAReadFailureWithoutLosingTheBatch(t *testing.T) {
	h := &fakeHandler{name: "letter", work: 1}
	store := newFakeStore()
	p := phase(t, store, []snif.Result{
		{PageID: "P1", Err: errors.New("render failed")},
		result("P2", "letter"),
	}, h)

	n, err := p.Run(context.Background())
	if err == nil {
		t.Error("Run hid a read failure")
	}
	// The good page still got its work done and counted.
	if n != 1 || h.count() != 1 {
		t.Errorf("one bad page cost the good one: %d units, %d calls", n, h.count())
	}
}

func TestRunLeavesStateUncommittedWhenTheHandlerFails(t *testing.T) {
	// The retry contract: the same change must be offered again next batch.
	h := &fakeHandler{name: "letter", work: 1, err: errors.New("provider down")}
	store := newFakeStore()
	p := phase(t, store, []snif.Result{result("P1", "letter")}, h)

	n, err := p.Run(context.Background())
	if err == nil {
		t.Error("Run hid a handler failure")
	}
	if n != 0 {
		t.Errorf("Run counted %d units for a failed handler, want 0", n)
	}
	if store.commits() != 0 {
		t.Error("a failed handler advanced the stored state, so the change is lost")
	}
}

func TestRunStillCountsWorkWhenTheStateCannotBeStored(t *testing.T) {
	h := &fakeHandler{name: "letter", work: 3}
	store := newFakeStore()
	store.commitErr = errors.New("disk full")
	p := phase(t, store, []snif.Result{result("P1", "letter")}, h)

	n, err := p.Run(context.Background())
	if err == nil {
		t.Error("a failed commit was not reported")
	}
	if n != 3 {
		t.Errorf("Run reported %d units, want the 3 the handler really did", n)
	}
}

func TestRunReportsAFailedQuery(t *testing.T) {
	reg, err := NewRegistry(&fakeHandler{name: "letter"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	p, err := NewPhase(&fakeReader{}, &fakeSelector{err: errors.New("archive unreadable")}, newFakeStore(), reg, "templated")
	if err != nil {
		t.Fatalf("NewPhase: %v", err)
	}
	if _, err := p.Run(context.Background()); err == nil {
		t.Fatal("Run hid a query failure")
	}
}

func TestRunOnAnEmptyArchiveDoesNothing(t *testing.T) {
	h := &fakeHandler{name: "letter"}
	store := newFakeStore()
	p := phase(t, store, nil, h)
	if n, err := p.Run(context.Background()); n != 0 || err != nil {
		t.Errorf("Run = %d, %v; want nothing to do", n, err)
	}
}

func TestRunStopsOnACancelledContext(t *testing.T) {
	h := &fakeHandler{name: "letter", work: 1}
	store := newFakeStore()
	p := phase(t, store, []snif.Result{result("P1", "letter"), result("P2", "letter")}, h)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Run(ctx); err == nil {
		t.Error("Run ignored a cancelled context")
	}
	if h.count() != 0 {
		t.Error("Run kept handling pages after cancellation")
	}
}

func TestPhaseNamesItselfForTheCommitMessage(t *testing.T) {
	p := phase(t, newFakeStore(), nil, &fakeHandler{name: "letter"})
	if p.Name() != "dispatch" || p.Unit() != "page" {
		t.Errorf("Name/Unit = %q/%q", p.Name(), p.Unit())
	}
}

// flushingHandler is a fakeHandler that also wants an end-of-batch call.
type flushingHandler struct {
	fakeHandler
	flushErr error

	flushes int
}

func (h *flushingHandler) Flush(context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.flushes++
	return h.flushErr
}

func TestRunFlushesAHandlerOncePerBatch(t *testing.T) {
	// The point of the seam: work whose cost belongs to the run, not to the page,
	// is paid once however many pages the batch carried.
	h := &flushingHandler{fakeHandler: fakeHandler{name: "letter", work: 1}}
	store := newFakeStore()
	p := phase(t, store, []snif.Result{result("P1", "letter"), result("P2", "letter")}, h)

	n, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != 2 {
		t.Errorf("Run reported %d units, want 2", n)
	}
	if h.count() != 2 {
		t.Errorf("handler called %d times, want one per page", h.count())
	}
	if h.flushes != 1 {
		t.Errorf("flushed %d times, want exactly 1", h.flushes)
	}
}

func TestRunFlushesAHandlerNoPageOfWhichMoved(t *testing.T) {
	// A handler cannot be asked "did anything of yours change?" — it is flushed
	// regardless and decides for itself.
	letter := &flushingHandler{fakeHandler: fakeHandler{name: "letter"}}
	other := &fakeHandler{name: "other", work: 1}
	p := phase(t, newFakeStore(), []snif.Result{result("P1", "other")}, letter, other)

	if _, err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if letter.count() != 0 {
		t.Error("a page was routed to the wrong handler")
	}
	if letter.flushes != 1 {
		t.Errorf("flushed %d times, want 1 even with no page of its own", letter.flushes)
	}
}

func TestRunReportsAFailedFlushWithoutLosingTheBatch(t *testing.T) {
	// The pages are done and their state committed before the flush runs, so a
	// flush that fails costs the batch only a log line — the daemon commits anyway.
	h := &flushingHandler{
		fakeHandler: fakeHandler{name: "letter", work: 1},
		flushErr:    errors.New("dropbox is down"),
	}
	store := newFakeStore()
	p := phase(t, store, []snif.Result{result("P1", "letter")}, h)

	n, err := p.Run(context.Background())
	if err == nil {
		t.Fatal("Run swallowed the flush failure")
	}
	if n != 1 {
		t.Errorf("Run reported %d units, want the page it really handled", n)
	}
	if store.commits() != 1 {
		t.Error("a failed flush undid the page's committed state")
	}
}

func TestRunDoesNotFlushAHandlerThatDoesNotWantIt(t *testing.T) {
	// The plain Handler interface is unchanged: not implementing Flusher is not a
	// failure, it is the ordinary case.
	h := &fakeHandler{name: "letter", work: 1}
	p := phase(t, newFakeStore(), []snif.Result{result("P1", "letter")}, h)
	if _, err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRunDoesNotFlushAfterCancellation(t *testing.T) {
	h := &flushingHandler{fakeHandler: fakeHandler{name: "letter", work: 1}}
	p := phase(t, newFakeStore(), []snif.Result{result("P1", "letter")}, h)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Run(ctx); err == nil {
		t.Error("Run ignored a cancelled context")
	}
	if h.flushes != 0 {
		t.Error("Run flushed on a cancelled context")
	}
}

func TestHandlersAreReturnedInTemplateNameOrder(t *testing.T) {
	reg, err := NewRegistry(&fakeHandler{name: "letter"}, &fakeHandler{name: "agenda"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	hs := reg.Handlers()
	if len(hs) != 2 {
		t.Fatalf("Handlers returned %d, want 2", len(hs))
	}
	if hs[0].Template().Name() != "agenda" || hs[1].Template().Name() != "letter" {
		t.Errorf("Handlers order = %q, %q; want name order",
			hs[0].Template().Name(), hs[1].Template().Name())
	}
}
