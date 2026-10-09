package letter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jdlugosz963/snif/pkg/snif"
	"github.com/jdlugosz963/snorg/pkg/snorg"

	"github.com/jdlugosz963/snorgd/internal/config"
	"github.com/jdlugosz963/snorgd/internal/dispatch"
)

// fakeArchive is an in-memory page: one buffer, one tag list.
type fakeArchive struct {
	mu sync.Mutex

	buffer    string
	applied   []string
	tags      []string
	matches   []snorg.Match
	result    *snorg.Result
	bufferErr error
	applyErr  error
	tagErr    error
	queryErr  error
}

func (a *fakeArchive) Query(snorg.Predicate) ([]snorg.Match, error) {
	return a.matches, a.queryErr
}

func (a *fakeArchive) Retrieve([]string) (*snorg.Result, error) {
	if a.result == nil {
		return &snorg.Result{}, nil
	}
	return a.result, nil
}

func (a *fakeArchive) PageBuffer(string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.buffer, a.bufferErr
}

func (a *fakeArchive) ApplyPage(_, buffer string) (snorg.PageEdit, error) {
	if a.applyErr != nil {
		return snorg.PageEdit{}, a.applyErr
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.buffer = buffer
	a.applied = append(a.applied, buffer)
	return snorg.PageEdit{}, nil
}

func (a *fakeArchive) Tag(_ []string, tag string) (int, error) {
	if a.tagErr != nil {
		return 0, a.tagErr
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tags = append(a.tags, tag)
	return 1, nil
}

func (a *fakeArchive) writes() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.applied...)
}

// fakeProvider records the prompts it was given.
type fakeProvider struct {
	mu      sync.Mutex
	answer  string
	err     error
	prompts []string
	inputs  []string
}

func (p *fakeProvider) Generate(_ context.Context, prompt, input string) (string, error) {
	p.mu.Lock()
	p.prompts = append(p.prompts, prompt)
	p.inputs = append(p.inputs, input)
	p.mu.Unlock()
	return p.answer, p.err
}

func (p *fakeProvider) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.prompts)
}

type fakeUploader struct {
	mu    sync.Mutex
	paths []string
	err   error
}

func (u *fakeUploader) Upload(_ context.Context, path string, _ []byte) error {
	if u.err != nil {
		return u.err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.paths = append(u.paths, path)
	return nil
}

// cfg is a valid letter configuration with the PDF switched off, which is the
// default and keeps most tests away from a typesetter.
func cfg() config.Letter {
	return config.Letter{
		Enabled:      true,
		Prompt:       "Answer the question.",
		Topics:       map[string]string{"general": "Be plain.", "study": "Be a tutor."},
		DefaultTopic: "general",
		AnsweredTag:  "ai-answered",
		PDFName:      "letters.pdf",
		DropboxDir:   "/Supernote",
	}
}

// buffer is a realistic templated-page edit buffer: a title, then one section per
// template box, in the shape snorg serializes.
func buffer(question string) string {
	return strings.Join([]string{
		"<!-- title 1 (h1) -->",
		"A page title",
		"<!-- region slider:length:100:2000:100 (Answer length (words)) -->",
		"",
		"<!-- region radio:topic (Style) -->",
		"",
		"<!-- region input:prompt (Question) -->",
		question,
		"<!-- region answer (Answer) -->",
		"",
		"",
	}, "\n")
}

// form builds a read form: the question, and optionally a length and a style.
func form(question string, words float64, topic string) *snif.Form {
	f := &snif.Form{PageID: "P20260822120000000000abcd", FileID: "F1", Template: templateName}
	f.Widgets = append(f.Widgets, snif.Widget{Kind: snif.Input, Name: widgetPrompt, Text: question, Set: true})
	length := snif.Widget{Kind: snif.Slider, Name: widgetLength}
	if words > 0 {
		length.Number, length.Set = words, true
	}
	f.Widgets = append(f.Widgets, length)
	style := snif.Widget{Kind: snif.Radio, Name: widgetTopic}
	if topic != "" {
		style.Choice, style.Set = topic, true
	}
	f.Widgets = append(f.Widgets, style)
	return f
}

func event(f *snif.Form) dispatch.Event {
	return dispatch.Event{PageID: f.PageID, FileID: f.FileID, Form: f}
}

// handler wires a handler around the fakes.
func handler(t *testing.T, a *fakeArchive, p *fakeProvider, u *fakeUploader, c config.Letter) *Handler {
	t.Helper()
	h, err := New(Deps{Archive: a, Provider: p, Uploader: u}, c)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func TestHandleAnswersAQuestionIntoThePage(t *testing.T) {
	a := &fakeArchive{buffer: buffer("How do monads work?")}
	p := &fakeProvider{answer: "A monad is a monoid in the category of endofunctors."}
	h := handler(t, a, p, &fakeUploader{}, cfg())

	n, err := h.Handle(context.Background(), event(form("How do monads work?", 0, "")))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if n != 1 {
		t.Errorf("Handle reported %d units, want 1", n)
	}
	writes := a.writes()
	if len(writes) != 1 {
		t.Fatalf("wrote %d buffers, want 1", len(writes))
	}
	if !strings.Contains(writes[0], "A monad is a monoid") {
		t.Errorf("the answer is not in the written buffer:\n%s", writes[0])
	}
	// The question it answered must survive untouched beside its answer.
	if !strings.Contains(writes[0], "How do monads work?") {
		t.Errorf("the question was lost from the buffer:\n%s", writes[0])
	}
	if len(a.tags) != 1 || a.tags[0] != "ai-answered" {
		t.Errorf("tags = %v, want the answered tag", a.tags)
	}
}

func TestHandleOnABlankQuestionNeverReachesTheModel(t *testing.T) {
	// A printed sheet nobody has written on yet reads as an empty question on
	// every batch. Calling the model for it would cost money for nothing.
	for _, question := range []string{"", "   ", "\n\t \n"} {
		t.Run(fmt.Sprintf("%q", question), func(t *testing.T) {
			a := &fakeArchive{buffer: buffer(question)}
			p := &fakeProvider{answer: "should never be asked for"}
			h := handler(t, a, p, &fakeUploader{}, cfg())

			n, err := h.Handle(context.Background(), event(form(question, 800, "study")))
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if n != 0 {
				t.Errorf("Handle reported %d units for a blank question, want 0", n)
			}
			if p.calls() != 0 {
				t.Errorf("the model was called %d times for a blank question", p.calls())
			}
			if len(a.writes()) != 0 || len(a.tags) != 0 {
				t.Error("a blank question still wrote to the page")
			}
		})
	}
}

func TestHandlePutsTheLengthAndStyleInThePrompt(t *testing.T) {
	a := &fakeArchive{buffer: buffer("Explain eigenvalues.")}
	p := &fakeProvider{answer: "An eigenvalue is..."}
	h := handler(t, a, p, &fakeUploader{}, cfg())

	if _, err := h.Handle(context.Background(), event(form("Explain eigenvalues.", 800, "study"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if p.calls() != 1 {
		t.Fatalf("model called %d times, want 1", p.calls())
	}
	prompt := p.prompts[0]
	for _, want := range []string{"Answer the question.", "Be a tutor.", "800 words", "$...$"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("system prompt is missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "Be plain.") {
		t.Error("the unselected style leaked into the prompt")
	}
	// The question is the model's input, not part of its instructions.
	if p.inputs[0] != "Explain eigenvalues." {
		t.Errorf("input = %q, want the question", p.inputs[0])
	}
}

func TestHandleFallsBackToTheDefaultStyle(t *testing.T) {
	a := &fakeArchive{buffer: buffer("A question.")}
	p := &fakeProvider{answer: "An answer."}
	h := handler(t, a, p, &fakeUploader{}, cfg())

	// An unticked radio is unanswered, not an error: the default is what a default
	// is for.
	if _, err := h.Handle(context.Background(), event(form("A question.", 0, ""))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(p.prompts[0], "Be plain.") {
		t.Errorf("the default style was not used:\n%s", p.prompts[0])
	}
}

func TestHandleOmitsTheLengthWhenTheSliderIsUntouched(t *testing.T) {
	a := &fakeArchive{buffer: buffer("A question.")}
	p := &fakeProvider{answer: "An answer."}
	h := handler(t, a, p, &fakeUploader{}, cfg())

	if _, err := h.Handle(context.Background(), event(form("A question.", 0, "general"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if strings.Contains(p.prompts[0], "Aim for about") {
		t.Errorf("an untouched slider still produced a length instruction:\n%s", p.prompts[0])
	}
}

func TestHandleRefusesAStyleTickedTwice(t *testing.T) {
	// The page is asking for two things at once. Quietly picking one would be
	// answering a question nobody asked.
	a := &fakeArchive{buffer: buffer("A question.")}
	p := &fakeProvider{answer: "An answer."}
	h := handler(t, a, p, &fakeUploader{}, cfg())

	f := form("A question.", 0, "")
	for i := range f.Widgets {
		if f.Widgets[i].Name == widgetTopic {
			f.Widgets[i].Err = &snif.AmbiguousError{Widget: widgetTopic}
		}
	}
	if _, err := h.Handle(context.Background(), event(f)); err == nil {
		t.Fatal("Handle accepted a style ticked more than once")
	}
	if p.calls() != 0 {
		t.Error("the model was called for an ambiguous form")
	}
}

func TestHandleRefusesAStyleTheConfigNoLongerHas(t *testing.T) {
	// The page was drawn on a template generated from a different topic set.
	a := &fakeArchive{buffer: buffer("A question.")}
	p := &fakeProvider{answer: "An answer."}
	h := handler(t, a, p, &fakeUploader{}, cfg())

	if _, err := h.Handle(context.Background(), event(form("A question.", 0, "retired"))); err == nil {
		t.Fatal("Handle accepted a style that is not configured")
	}
	if len(a.writes()) != 0 {
		t.Error("an unconfigured style still wrote to the page")
	}
}

func TestHandleRefusesAnEmptyAnswer(t *testing.T) {
	a := &fakeArchive{buffer: buffer("A question.")}
	p := &fakeProvider{answer: "   "}
	h := handler(t, a, p, &fakeUploader{}, cfg())

	if _, err := h.Handle(context.Background(), event(form("A question.", 0, ""))); err == nil {
		t.Fatal("Handle stored an empty answer")
	}
	if len(a.writes()) != 0 {
		t.Error("an empty answer was written to the page")
	}
}

func TestHandleReportsAModelFailureWithoutTagging(t *testing.T) {
	a := &fakeArchive{buffer: buffer("A question.")}
	p := &fakeProvider{err: errors.New("provider down")}
	h := handler(t, a, p, &fakeUploader{}, cfg())

	if _, err := h.Handle(context.Background(), event(form("A question.", 0, ""))); err == nil {
		t.Fatal("Handle hid a provider failure")
	}
	if len(a.tags) != 0 {
		t.Error("a page was marked answered after the model failed")
	}
}

func TestHandleReportsAWriteFailure(t *testing.T) {
	a := &fakeArchive{buffer: buffer("A question."), applyErr: errors.New("read-only archive")}
	p := &fakeProvider{answer: "An answer."}
	h := handler(t, a, p, &fakeUploader{}, cfg())

	if _, err := h.Handle(context.Background(), event(form("A question.", 0, ""))); err == nil {
		t.Fatal("Handle hid a failed write")
	}
	if len(a.tags) != 0 {
		t.Error("a page was marked answered though the answer was not stored")
	}
}

// answeredArchive is an archive holding one page already answered, which is what
// publish reads back when it rebuilds the document.
func answeredArchive(question string) *fakeArchive {
	return &fakeArchive{
		buffer:  buffer(question),
		matches: []snorg.Match{{PageID: "P1"}},
		result: &snorg.Result{Notes: []*snorg.NoteView{{Pages: []snorg.PageView{{
			PageID:   "P1",
			Analysis: &snorg.PageAnalysisView{Regions: []snorg.RegionView{{ID: boxAnswer, Content: "An answer."}}},
		}}}}},
	}
}

// publishing returns a config whose typesetter is the given shell command.
func publishing(command string) config.Letter {
	c := cfg()
	c.PDFCommand = command
	c.PDFTimeout = config.Duration(5_000_000_000)
	return c
}

func TestHandleKeepsTheAnswerWhenPublishingFails(t *testing.T) {
	// Publishing is a convenience on top of an answer that is already in the
	// archive. Failing the handler would leave the page's state uncommitted and
	// pay for another model call next batch to fix a PDF — so the failure surfaces
	// from Flush, after the page's work is already safe.
	a := answeredArchive("A question.")
	p := &fakeProvider{answer: "An answer."}
	u := &fakeUploader{}
	h := handler(t, a, p, u, publishing("false"))

	n, err := h.Handle(context.Background(), event(form("A question.", 0, "")))
	if err != nil {
		t.Fatalf("a failing typesetter failed the whole handler: %v", err)
	}
	if n != 1 {
		t.Errorf("Handle reported %d units, want 1 — the answer was stored", n)
	}
	if len(a.writes()) != 1 {
		t.Error("the answer was not written")
	}
	if err := h.Flush(context.Background()); err == nil {
		t.Error("a failing typesetter flushed without complaint")
	}
	if len(u.paths) != 0 {
		t.Error("something was uploaded despite the typesetter failing")
	}
	if !h.dirty {
		t.Error("a failed publish cleared the flag, so the next batch will not retry")
	}
}

func TestFlushWithoutAnAnsweredPageUploadsNothing(t *testing.T) {
	// The dispatcher flushes every batch, including ones in which no letter moved.
	u := &fakeUploader{}
	h := handler(t, answeredArchive(""), &fakeProvider{}, u, publishing("cp {{in}} {{out}}"))

	if err := h.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if len(u.paths) != 0 {
		t.Errorf("uploaded %v though nothing was answered this batch", u.paths)
	}
}

func TestFlushPublishesOnceForAWholeBatch(t *testing.T) {
	// The document is rebuilt from the whole archive, so its cost belongs to the
	// run. Two questions in one sync must still be one typesetting run and one
	// upload.
	a := answeredArchive("A question.")
	p := &fakeProvider{answer: "An answer."}
	u := &fakeUploader{}
	h := handler(t, a, p, u, publishing("cp {{in}} {{out}}"))

	for i := 0; i < 2; i++ {
		if _, err := h.Handle(context.Background(), event(form("A question.", 0, ""))); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if len(u.paths) != 0 {
			t.Fatal("Handle published on its own instead of leaving it to Flush")
		}
	}
	if err := h.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if len(u.paths) != 1 {
		t.Fatalf("uploaded %d documents for one batch, want 1: %v", len(u.paths), u.paths)
	}
	if u.paths[0] != "/Supernote/letters.pdf" {
		t.Errorf("uploaded to %q", u.paths[0])
	}

	// Nothing has been answered since, so the next batch's flush is silent.
	if err := h.Flush(context.Background()); err != nil {
		t.Fatalf("second Flush: %v", err)
	}
	if len(u.paths) != 1 {
		t.Errorf("a batch that answered nothing republished: %v", u.paths)
	}
}

func TestFlushRetriesAfterAnUnreachableDropbox(t *testing.T) {
	// The answers are committed either way; staying dirty is what makes a transient
	// upload failure heal itself on the next batch, without another model call.
	a := answeredArchive("A question.")
	p := &fakeProvider{answer: "An answer."}
	u := &fakeUploader{err: errors.New("dropbox is down")}
	h := handler(t, a, p, u, publishing("cp {{in}} {{out}}"))

	if _, err := h.Handle(context.Background(), event(form("A question.", 0, ""))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if err := h.Flush(context.Background()); err == nil {
		t.Fatal("an unreachable Dropbox flushed without complaint")
	}

	u.err = nil
	if err := h.Flush(context.Background()); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if len(u.paths) != 1 {
		t.Errorf("uploaded %v, want the document once the upload came back", u.paths)
	}
	if h.dirty {
		t.Error("a successful publish left the handler dirty")
	}
	if p.calls() != 1 {
		t.Errorf("model called %d times, want 1 — a retry must not re-ask", p.calls())
	}
}

func TestNewRejectsIncompleteDependencies(t *testing.T) {
	tests := []struct {
		name string
		deps Deps
		cfg  config.Letter
	}{
		{"no archive", Deps{Provider: &fakeProvider{}}, cfg()},
		{"no provider", Deps{Archive: &fakeArchive{}}, cfg()},
		{"a pdf command with nowhere to publish", Deps{Archive: &fakeArchive{}, Provider: &fakeProvider{}}, func() config.Letter {
			c := cfg()
			c.PDFCommand = "pandoc -o {{out}} {{in}}"
			return c
		}()},
		{"no topics", Deps{Archive: &fakeArchive{}, Provider: &fakeProvider{}}, config.Letter{Enabled: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.deps, tc.cfg); err == nil {
				t.Fatal("New accepted an incomplete configuration")
			}
		})
	}
}

func TestTemplateIsDrawableAndCarriesTheAnswerBox(t *testing.T) {
	h := handler(t, &fakeArchive{}, &fakeProvider{}, &fakeUploader{}, cfg())
	tmpl := h.Template()
	if tmpl.Name() != templateName {
		t.Errorf("template name = %q, want %q", tmpl.Name(), templateName)
	}
	if len(tmpl.Boxes) != 1 || tmpl.Boxes[0].ID != boxAnswer {
		t.Errorf("boxes = %+v, want just the answer box", tmpl.Boxes)
	}
	if tmpl.Boxes[0].Analyze {
		t.Error("the answer box is marked analyze: vision would overwrite the answer")
	}

	// The topics configured must be the options printed on the page.
	var topics []string
	var promptH int
	for _, r := range tmpl.Form.Requests {
		switch r.Name {
		case widgetTopic:
			topics = r.Options
		case widgetPrompt:
			promptH = r.H
		}
	}
	if len(topics) != 2 || topics[0] != "general" || topics[1] != "study" {
		t.Errorf("radio options = %v, want the configured topics in order", topics)
	}
	if promptH <= 0 {
		t.Error("the question box was not given a height")
	}

	// And the whole thing must actually generate.
	if _, _, err := dispatch.Generate(tmpl); err != nil {
		t.Fatalf("the declared template does not generate: %v", err)
	}
}

func TestTemplateChangesWithTheTopics(t *testing.T) {
	// The options are printed on the image, so a different topic set must be a
	// different template — otherwise pages would be read against a page they were
	// not drawn on.
	a, p, u := &fakeArchive{}, &fakeProvider{}, &fakeUploader{}
	one := handler(t, a, p, u, cfg())

	c := cfg()
	c.Topics = map[string]string{"general": "Be plain.", "study": "Be a tutor.", "code": "Be a programmer."}
	two := handler(t, a, p, u, c)

	g1, _, err := dispatch.Generate(one.Template())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	g2, _, err := dispatch.Generate(two.Template())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if g1.Hash == g2.Hash {
		t.Error("adding a topic did not change the template's image")
	}
}
