package letter

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jdlugosz963/snif/pkg/snif"

	"github.com/jdlugosz963/snorgd/internal/dispatch"
	"github.com/jdlugosz963/snorgd/internal/formstate"
)

// The coupling this file exists for: the box ids snorgd's template generates are the
// box ids snif reads a page back under. Nothing checks that but a round trip, because
// the two halves never name an id to each other — they agree on a string, or they
// silently disagree and every page reads as unanswered.
//
// The tablet's part is played by painting ink into the rects the generator produced,
// and snif's archive-free reader (ReadRender) does the rest, so this exercises the
// real generator, the real reader, the real state store and the real handler.

// inkPage is a blank page that ink can be painted onto, the way a pen would.
type inkPage struct{ img *image.Gray }

func newInkPage() *inkPage {
	img := image.NewGray(image.Rect(0, 0, snif.PageW, snif.PageH))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	return &inkPage{img: img}
}

// tick fills most of a box, as a deliberate tick would.
func (p *inkPage) tick(r snif.Rect) {
	p.fill(r.X+r.W/4, r.Y+r.H/4, r.W/2, r.H/2)
}

// mark draws a vertical stroke down a slider track at a given column.
func (p *inkPage) mark(r snif.Rect, x int) {
	p.fill(x, r.Y+r.H/4, 3, r.H/2)
}

func (p *inkPage) fill(x, y, w, h int) {
	for dy := 0; dy < h; dy++ {
		for dx := 0; dx < w; dx++ {
			p.img.SetGray(x+dx, y+dy, color.Gray{Y: 0})
		}
	}
}

// render encodes the page the way snorg renders one for analysis.
func (p *inkPage) render(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, p.img); err != nil {
		t.Fatalf("encode page: %v", err)
	}
	return b.Bytes()
}

// boxOf finds a generated box by id, failing loudly when the id is not there — which
// is the failure this whole file is watching for.
func boxOf(t *testing.T, boxes []snif.Box, id string) snif.Rect {
	t.Helper()
	for _, b := range boxes {
		if b.ID == id {
			return b.Rect
		}
	}
	var ids []string
	for _, b := range boxes {
		ids = append(ids, b.ID)
	}
	t.Fatalf("the generated template declares no box %q; it has: %s", id, strings.Join(ids, ", "))
	return snif.Rect{}
}

func TestAFilledInPageReadsBackAsTheAnswersItWasGiven(t *testing.T) {
	tmpl, err := Declare(cfg())
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	g, _, err := dispatch.Generate(tmpl)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	boxes := g.Boxes()

	page := newInkPage()
	page.tick(boxOf(t, boxes, "radio:topic:study"))

	// Aim the slider at 900 words, using snif's own mapping: the rightmost inked
	// column, taken inclusively, across the track's width.
	track := boxOf(t, boxes, "slider:length:100:2000:100")
	wantWords := 900.0
	col := track.X + int(math.Round((wantWords-minWords)/(maxWords-minWords)*float64(track.W))) - 1
	page.mark(track, col)

	// The question is text, so it arrives the way snorg's vision pass delivers it:
	// keyed by the Input widget's full box id, not by the widget's name.
	form, err := snif.ReadRender(boxes, page.render(t), map[string]string{
		"input:" + widgetPrompt: "What is a monad?",
	}, snif.ReadOptions{})
	if err != nil {
		t.Fatalf("ReadRender: %v", err)
	}
	if err := form.Err(); err != nil {
		t.Fatalf("the form did not read cleanly: %v", err)
	}

	if got, err := form.Choice(widgetTopic); err != nil || got != "study" {
		t.Errorf("style = %q (%v), want study", got, err)
	}
	if got, err := form.Number(widgetLength); err != nil || got != wantWords {
		t.Errorf("length = %v (%v), want %v", got, err, wantWords)
	}
	if got, err := form.Text(widgetPrompt); err != nil || got != "What is a monad?" {
		t.Errorf("question = %q (%v), want the written one", got, err)
	}
}

func TestAPageFilledInThenAnsweredGoesRoundTheWholeLoop(t *testing.T) {
	tmpl, err := Declare(cfg())
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	g, _, err := dispatch.Generate(tmpl)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	boxes := g.Boxes()

	store, err := formstate.NewStore(filepath.Join(t.TempDir(), "forms"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	arch := &fakeArchive{buffer: buffer("")}
	prov := &fakeProvider{answer: "A monad is a monoid in the category of endofunctors."}
	up := &fakeUploader{}
	h := handler(t, arch, prov, up, cfg())

	// A sheet printed and synced but not written on. Its state is new, so it is
	// offered to the handler — which must not spend a model call on it.
	blank := newInkPage()
	read := func(p *inkPage, question string) *snif.Form {
		t.Helper()
		f, err := snif.ReadRender(boxes, p.render(t), map[string]string{"input:" + widgetPrompt: question}, snif.ReadOptions{})
		if err != nil {
			t.Fatalf("ReadRender: %v", err)
		}
		f.PageID = "P20260822120000000000abcd"
		f.FileID = "F1"
		f.Template = templateName
		return f
	}

	empty := read(blank, "")
	st, err := store.Diff(empty)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !st.Changed() {
		t.Fatal("a page seen for the first time did not read as changed")
	}
	if n, err := h.Handle(context.Background(), dispatch.Event{PageID: st.PageID, Form: empty, State: st}); err != nil || n != 0 {
		t.Fatalf("Handle on a blank sheet = %d, %v; want 0, nil", n, err)
	}
	if prov.calls() != 0 {
		t.Fatal("a blank sheet reached the model")
	}
	if err := store.Commit(st); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Nothing has happened since. The page must not be offered again.
	unchanged, err := store.Diff(read(blank, ""))
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if unchanged.Changed() {
		t.Errorf("an untouched page read as changed: %v", unchanged.ChangedFields())
	}

	// Now it is written on: a style ticked and a question asked.
	written := newInkPage()
	written.tick(boxOf(t, boxes, "radio:topic:study"))
	filled := read(written, "What is a monad?")

	st, err = store.Diff(filled)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	moved := st.ChangedFields()
	if len(moved) != 2 || moved[0] != widgetPrompt || moved[1] != widgetTopic {
		t.Errorf("changed fields = %v, want exactly the question and the style", moved)
	}

	n, err := h.Handle(context.Background(), dispatch.Event{PageID: st.PageID, Form: filled, State: st})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if n != 1 {
		t.Errorf("Handle reported %d units, want 1", n)
	}
	if prov.calls() != 1 {
		t.Fatalf("model called %d times, want 1", prov.calls())
	}
	if prov.inputs[0] != "What is a monad?" {
		t.Errorf("the model was asked %q", prov.inputs[0])
	}
	if !strings.Contains(prov.prompts[0], "Be a tutor.") {
		t.Errorf("the ticked style did not reach the prompt:\n%s", prov.prompts[0])
	}
	// The answer is in the page, and the page is now tagged.
	if !strings.Contains(arch.buffer, "A monad is a monoid") {
		t.Errorf("the answer is not in the page buffer:\n%s", arch.buffer)
	}
	if len(arch.tags) != 1 {
		t.Errorf("tags = %v, want the page marked answered", arch.tags)
	}

	// The batch ends. With no typesetter configured the answer still stands in the
	// page — publishing is the convenience, the archive is the record.
	if err := h.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if len(up.paths) != 0 {
		t.Errorf("uploaded %v with no pdf_command configured", up.paths)
	}
}
