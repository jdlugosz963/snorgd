// Package letter is the "letter to AI" handler: a page that asks the model a
// question, and gets its answer back.
//
// The page is a snif form — a slider for how long the answer should be, a radio for
// which house style to answer in, and the rest of the sheet given over to the
// question itself, written by hand. When anything on it changes, the model is asked,
// and the reply is written into the page's own answer region, so it is committed with
// the batch and readable in the notes repo beside the question that prompted it.
// Every answer in the archive is then typeset into one accumulating PDF, published
// back to the device so the reply can be read where the question was written. That
// happens once per batch rather than once per answer: the document is rebuilt from
// the whole archive every time, so its cost belongs to the run, not to the page.
//
// The handler owns nothing in the archive directory: it reads and writes through
// snorg's API, and the PDF it renders is assembled in a temp dir and uploaded, never
// stored.
package letter

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jdlugosz963/snif/pkg/snif"
	"github.com/jdlugosz963/snorg/pkg/snorg"

	"github.com/jdlugosz963/snorgd/internal/config"
	"github.com/jdlugosz963/snorgd/internal/dispatch"
)

// The template's parts, by the names the form reads them back under. The answer is a
// plain snorg box rather than a widget: it holds text nobody writes by hand, and snif
// skips any id that is not a widget's.
const (
	templateName = "letter"

	widgetLength = "length"
	widgetTopic  = "topic"
	widgetPrompt = "prompt"
	boxAnswer    = "answer"
)

// The length slider's range, in words. The step is coarse on purpose: the value is
// read from where a pen stroke lands, and asking someone to hit a 10-word increment
// with a nib would be asking for a number they did not mean.
const (
	minWords  = 100
	maxWords  = 2000
	wordsStep = 100
)

// markdownConvention is appended to every prompt. The answer is typeset by pandoc
// into LaTeX, which handles maths and code properly — but only for an answer written
// the way pandoc expects. The dollar-sign note is not pedantry: a lone $ in prose
// parses as the start of a formula and swallows the rest of the paragraph.
const markdownConvention = "Write the answer in Markdown. Use $...$ for inline mathematics, $$...$$ for display mathematics, and fenced code blocks for code. Write currency as \"USD 5\" rather than \"$5\", because a lone dollar sign is read as mathematics."

// Archive is the part of snorg the handler needs. Everything it does to the archive
// goes through here: the handler never touches the archive directory itself.
type Archive interface {
	Query(pred snorg.Predicate) ([]snorg.Match, error)
	Retrieve(pageIDs []string) (*snorg.Result, error)
	PageBuffer(pageID string) (string, error)
	ApplyPage(pageID, buffer string) (snorg.PageEdit, error)
	Tag(pageIDs []string, tag string) (int, error)
}

// Generator is the model. snorg.Provider satisfies it, built from the archive's own
// provider configuration, so snorgd never holds model credentials of its own.
type Generator interface {
	Generate(ctx context.Context, prompt, input string) (string, error)
}

// Uploader publishes the assembled document to the device. *dropbox.Client
// implements it.
type Uploader interface {
	Upload(ctx context.Context, path string, data []byte) error
}

// Deps are the handler's collaborators.
type Deps struct {
	Archive  Archive
	Provider Generator
	Uploader Uploader
}

// Handler answers letter pages. It implements dispatch.Handler.
type Handler struct {
	arch Archive
	prov Generator
	up   Uploader
	cfg  config.Letter

	tmpl     dispatch.Template
	answered snorg.Predicate

	// dirty records that this batch answered something and the published document
	// is therefore out of date. Handle runs serially within a batch, so a plain
	// bool needs no guarding.
	dirty bool
}

// New builds the handler and lays its template out, so a template that cannot be
// drawn — a topic set too wide for the page, say — fails at startup rather than on
// the first question.
func New(d Deps, cfg config.Letter) (*Handler, error) {
	if d.Archive == nil || d.Provider == nil {
		return nil, fmt.Errorf("letter: an archive and a provider are required")
	}
	if cfg.PDFCommand != "" && d.Uploader == nil {
		return nil, fmt.Errorf("letter: pdf_command is set but there is nowhere to publish to")
	}
	tmpl, err := Declare(cfg)
	if err != nil {
		return nil, fmt.Errorf("letter: %w", err)
	}
	return &Handler{
		arch: d.Archive, prov: d.Provider, up: d.Uploader, cfg: cfg, tmpl: tmpl,
		answered: snorg.MatchAnd(snorg.MatchTemplated, snorg.MatchTag(snorg.Exact(cfg.AnsweredTag))),
	}, nil
}

// Template is the page this handler owns.
func (h *Handler) Template() dispatch.Template { return h.tmpl }

// Declare lays the form out from configuration alone.
//
// It is separate from the handler because drawing the page and answering it are
// different jobs with different needs: `snorgd gen-templates` has a config file and
// nothing else — no archive, no model credentials — and must still be able to produce
// exactly the image the daemon will read pages against.
//
// The topics come from the configuration, so the options printed on the page and the
// prompts behind them cannot drift apart.
func Declare(cfg config.Letter) (dispatch.Template, error) {
	topics := cfg.TopicNames()
	if len(topics) == 0 {
		return dispatch.Template{}, fmt.Errorf("no topics configured")
	}

	form := snif.Template{
		Name:  templateName,
		Title: "Letter to AI",
		Requests: []snif.WidgetSpec{
			{Kind: snif.Slider, Name: widgetLength, Label: "Answer length (words)",
				Min: minWords, Max: maxWords, Step: wordsStep},
			{Kind: snif.Radio, Name: widgetTopic, Label: "Style", Options: topics},
			{Kind: snif.Input, Name: widgetPrompt, Label: "Question", Style: snif.Lines},
		},
	}
	// The question box takes whatever the other widgets left, which is the whole
	// point of the page: everything else is a setting, and the sheet is for writing.
	height, err := dispatch.FillHeight(form, widgetPrompt)
	if err != nil {
		return dispatch.Template{}, err
	}
	for i := range form.Requests {
		if form.Requests[i].Name == widgetPrompt {
			form.Requests[i].H = height
		}
	}

	return dispatch.Template{
		Form: form,
		// The answer is about the page as a whole, and its rect is never drawn,
		// rendered or measured — analyze: false keeps vision out of it, and snif
		// ignores the id — so the page itself is the honest extent to give it.
		Boxes: []dispatch.Box{{
			ID:    boxAnswer,
			Label: "Answer",
			Rect:  dispatch.Rect{W: snif.PageW, H: snif.PageH},
		}},
	}, nil
}

// Handle answers one page whose form has moved.
//
// A blank question is the ordinary resting state of a freshly printed sheet, and of
// every sheet between being drawn on and being written on, so it is not an error and
// costs nothing: the model is not called at all.
func (h *Handler) Handle(ctx context.Context, ev dispatch.Event) (int, error) {
	question, err := ev.Form.Text(widgetPrompt)
	if err != nil {
		return 0, fmt.Errorf("read the question: %w", err)
	}
	if strings.TrimSpace(question) == "" {
		return 0, nil
	}

	topic, err := h.topic(ev.Form)
	if err != nil {
		return 0, err
	}
	answer, err := h.prov.Generate(ctx, h.systemPrompt(topic, length(ev.Form)), question)
	if err != nil {
		return 0, fmt.Errorf("generate: %w", err)
	}
	if strings.TrimSpace(answer) == "" {
		return 0, fmt.Errorf("the model returned an empty answer")
	}

	if err := h.write(ev.PageID, answer); err != nil {
		return 0, err
	}
	if _, err := h.arch.Tag([]string{ev.PageID}, h.cfg.AnsweredTag); err != nil {
		return 0, fmt.Errorf("tag %s: %w", h.cfg.AnsweredTag, err)
	}

	// The answer is stored and will be committed with the batch. Publishing waits
	// for Flush, at the end of the batch, so a sync carrying five questions
	// typesets and uploads one document instead of five.
	h.dirty = true
	return 1, nil
}

// Flush publishes the collected document, if this batch answered anything. It
// implements dispatch.Flusher, and so runs once after the last page of the batch.
//
// The error is returned rather than swallowed, because the dispatcher logs it and
// commits the batch regardless: the answers are already in the archive, which is where
// they are really kept, and publishing is a convenience on top of that. Leaving the
// handler dirty is what makes an unreachable Dropbox self-heal — the next batch tries
// again, without paying for another model call.
func (h *Handler) Flush(ctx context.Context) error {
	if !h.dirty {
		return nil
	}
	if err := h.publish(ctx); err != nil {
		return fmt.Errorf("letter: publish: %w (the answers are in the archive)", err)
	}
	h.dirty = false
	return nil
}

// write puts the answer into the page's answer region, through snorg's own edit
// round-trip, so it lands in the page's sidecar with the divergence bookkeeping that
// keeps it from being overwritten by a later analysis.
func (h *Handler) write(pageID, answer string) error {
	buf, err := h.arch.PageBuffer(pageID)
	if err != nil {
		return fmt.Errorf("read page %s: %w", pageID, err)
	}
	next, err := setRegion(buf, boxAnswer, strings.TrimSpace(answer))
	if err != nil {
		return fmt.Errorf("page %s: %w", pageID, err)
	}
	if _, err := h.arch.ApplyPage(pageID, next); err != nil {
		return fmt.Errorf("write page %s: %w", pageID, err)
	}
	return nil
}

// topic is the ticked style, or the configured default when none is ticked.
//
// An unticked radio is not an error in snif — it is simply unanswered, and the
// default is exactly what a default is for. Two ticked options are different: the
// page is asking for two things at once, and choosing one of them silently would be
// answering a question nobody asked.
func (h *Handler) topic(f *snif.Form) (string, error) {
	choice, err := f.Choice(widgetTopic)
	if err != nil {
		var ambiguous *snif.AmbiguousError
		if errors.As(err, &ambiguous) {
			return "", fmt.Errorf("the style is ticked more than once: %w", err)
		}
		return h.cfg.DefaultTopic, nil
	}
	if _, ok := h.cfg.Topics[choice]; !ok {
		// The page was drawn on a template generated from a different topic set.
		return "", fmt.Errorf("the page offers a style %q that is no longer configured", choice)
	}
	return choice, nil
}

// length is the slider's value in words, or 0 for an untouched slider.
func length(f *snif.Form) int {
	n, err := f.Number(widgetLength)
	if err != nil {
		return 0
	}
	return int(n)
}

// systemPrompt is what the model is told before the question: the configured base
// instruction, the selected style, how long the answer should be, and the markdown
// conventions the typesetter depends on.
func (h *Handler) systemPrompt(topic string, words int) string {
	parts := []string{strings.TrimSpace(h.cfg.Prompt)}
	if preset := strings.TrimSpace(h.cfg.Topics[topic]); preset != "" {
		parts = append(parts, preset)
	}
	if words > 0 {
		parts = append(parts, fmt.Sprintf("Aim for about %d words.", words))
	}
	parts = append(parts, markdownConvention)

	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n\n")
}
