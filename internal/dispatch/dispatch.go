// Package dispatch routes templated pages to the code that acts on them.
//
// A page drawn on a snif template is a small user interface: the sliders, radios and
// written-in boxes on it are answers, and snif reads them back as typed values. What
// snif does not say is which of those pages is asking for something, or what. That is
// this package: snorgd declares a template in Go, registers a Handler against it, and
// after every ingest batch each page drawn on a registered template is read, compared
// against what it said last time, and handed to its Handler along with the list of
// what moved.
//
// The arrangement is deliberately ignorant of what a handler does. Nothing here knows
// about models, PDFs or Dropbox; a handler that renames a file on a ticked box is as
// much a first-class handler as one that answers a question. Adding a second kind of
// page is writing a Handler and registering it — not extending this package.
package dispatch

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jdlugosz963/snif/pkg/snif"
	"github.com/jdlugosz963/snorg/pkg/snorg"

	"github.com/jdlugosz963/snorgd/internal/formstate"
)

// Box is a plain snorg template box: a region with an id that is not a snif widget.
// snif skips ids it does not recognize, so a form may carry boxes alongside its
// widgets — which is how a handler gets somewhere in the page to write to, since a
// templated page's transcription is keyed by box id and snorg refuses to store text
// under an id the template does not declare.
type Box = snorg.Box

// Rect is a box's area in page pixel space (1920x2560).
type Rect = snorg.Rect

// Template is a handler's page: the snif form that is drawn, plus any plain boxes
// that belong on it.
type Template struct {
	Form  snif.Template
	Boxes []Box
}

// Name is the template's routing key, and the basename of the image it generates.
func (t Template) Name() string { return t.Form.Name }

// Event is one templated page ready to be acted on: the form as read, and the state
// diff saying what moved since the dispatcher last looked.
type Event struct {
	PageID string
	FileID string
	Form   *snif.Form
	State  formstate.State
}

// Changed reports whether the named widget moved since the last batch that acted on
// this page — the question most handlers ask first.
func (e Event) Changed(name string) bool { return e.State.IsChanged(name) }

// Handler owns one template: how its page is drawn, and what to do when one changes.
//
// Handle is called only for a page whose state actually moved, so it need not
// re-check that anything happened; it decides whether the change is one it cares
// about. The int it returns is units of work done, which reaches the commit message;
// returning 0 with a nil error is the ordinary way to say "nothing to do here".
//
// An error means the work did not happen. The page's state is then left untouched, so
// the same change is offered again on the next batch rather than being lost.
type Handler interface {
	Template() Template
	Handle(ctx context.Context, ev Event) (int, error)
}

// Flusher is an optional Handler extension: work that belongs to a whole batch rather
// than to any one page. The dispatcher calls Flush once, after the last page of the
// batch has been handled, on every registered handler that implements it.
//
// Every registered handler is flushed, whether or not any of its own pages moved, so
// an implementation decides for itself whether it has anything to do. (A batch in
// which no page was read at all is the one exception: there was nothing to act on, so
// nothing is flushed either.) Like Handle, it says nothing about what the work is: a
// handler that rebuilds a document and one that closes a file are the same to the
// dispatcher.
//
// An error is logged by the dispatcher and joined into the phase's, but the batch is
// still committed — flushing is by definition work that comes after the per-page work
// has already succeeded.
type Flusher interface {
	Flush(ctx context.Context) error
}

// Registry maps a template name to the handler that owns it.
type Registry struct {
	byName map[string]Handler
}

// NewRegistry indexes handlers by template name, refusing two that claim the same
// one — the page would otherwise be routed by map iteration order.
//
// A nil handler is skipped, so a caller may pass the result of a constructor that
// returns nothing when its stage is disabled without unwrapping it first.
func NewRegistry(hs ...Handler) (*Registry, error) {
	r := &Registry{byName: map[string]Handler{}}
	for _, h := range hs {
		if h == nil {
			continue
		}
		t := h.Template()
		name := strings.TrimSpace(t.Name())
		if name == "" {
			return nil, fmt.Errorf("handler %T declares a template with no name", h)
		}
		if prev, dup := r.byName[name]; dup {
			return nil, fmt.Errorf("template %q is claimed by both %T and %T", name, prev, h)
		}
		r.byName[name] = h
	}
	return r, nil
}

// Handler returns the handler registered for a template name.
func (r *Registry) Handler(name string) (Handler, bool) {
	h, ok := r.byName[name]
	return h, ok
}

// Len reports how many templates are registered.
func (r *Registry) Len() int { return len(r.byName) }

// Names lists the registered template names, sorted.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.byName))
	for name := range r.byName {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Handlers returns every registered handler, in template-name order, so a caller that
// walks them — the phase flushing them at the end of a batch — does so in an order
// that does not depend on map iteration.
func (r *Registry) Handlers() []Handler {
	out := make([]Handler, 0, len(r.byName))
	for _, name := range r.Names() {
		out = append(out, r.byName[name])
	}
	return out
}

// Templates returns every registered template, in name order — what
// `snorgd gen-templates` writes.
func (r *Registry) Templates() []Template {
	out := make([]Template, 0, len(r.byName))
	for _, name := range r.Names() {
		out = append(out, r.byName[name].Template())
	}
	return out
}
