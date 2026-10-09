package letter

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jdlugosz963/snorg/pkg/snorg"

	"github.com/jdlugosz963/snorgd/internal/config"
)

func TestAssembleReadsNewestFirstWithQuestionsAsQuotes(t *testing.T) {
	md := string(assemble([]entry{
		{PageID: "P20260822120000000000abcd", Question: "First question?", Answer: "First answer."},
		{PageID: "P20260801090000000000abcd", Question: "Older question?\nSecond line.", Answer: "Older answer."},
	}))
	if i, j := strings.Index(md, "First answer."), strings.Index(md, "Older answer."); i < 0 || j < 0 || i > j {
		t.Errorf("entries are not in the given order:\n%s", md)
	}
	if !strings.Contains(md, "> First question?") {
		t.Errorf("the question is not a block quote:\n%s", md)
	}
	// Every line of a multi-line question must be quoted, or the rest of it reads
	// as answer text.
	if !strings.Contains(md, "> Older question?\n> Second line.") {
		t.Errorf("a multi-line question was not fully quoted:\n%s", md)
	}
	// One heading per entry: the template gives each section its own page.
	if n := strings.Count(md, "\n# ") + strings.Count(md, "# 22 August"); n < 2 {
		t.Errorf("expected a heading per entry:\n%s", md)
	}
}

func TestAssembleWithoutAQuestionStillCarriesTheAnswer(t *testing.T) {
	md := string(assemble([]entry{{PageID: "P1", Answer: "An answer."}}))
	if !strings.Contains(md, "An answer.") {
		t.Errorf("the answer is missing:\n%s", md)
	}
	if strings.Contains(md, ">") {
		t.Errorf("an empty question produced an empty quote:\n%s", md)
	}
}

func TestHeadingDatesAnEntryFromItsPageID(t *testing.T) {
	tests := []struct {
		pageID string
		want   string
	}{
		{"P20260822143000123456abcd", "22 August 2026, 14:30"},
		{"P20260101000000000000abcd", "1 January 2026, 00:00"},
		// Anything unparseable still has to produce a stable heading that tells a
		// reader which page they are looking at.
		{"P-not-a-date-at-all", "P-not-a-date-at-all"},
		{"P2026", "P2026"},
		{"", ""},
	}
	for _, tc := range tests {
		t.Run(tc.pageID, func(t *testing.T) {
			if got := heading(tc.pageID); got != tc.want {
				t.Errorf("heading(%q) = %q, want %q", tc.pageID, got, tc.want)
			}
		})
	}
}

func TestSubstituteFillsBothPlaceholders(t *testing.T) {
	got := substitute("pandoc -f markdown --template=letters.tex -o {{out}} {{in}}", "/tmp/a/letters.md", "/tmp/a/letters.pdf")
	want := "pandoc -f markdown --template=letters.tex -o /tmp/a/letters.pdf /tmp/a/letters.md"
	if got != want {
		t.Errorf("substitute = %q, want %q", got, want)
	}
}

func TestEntriesSkipsPagesWithNoAnswerYet(t *testing.T) {
	a := &fakeArchive{
		matches: []snorg.Match{{PageID: "P2"}, {PageID: "P1"}},
		result: &snorg.Result{Notes: []*snorg.NoteView{{Pages: []snorg.PageView{
			{PageID: "P1", Analysis: &snorg.PageAnalysisView{Regions: []snorg.RegionView{
				{ID: promptBoxID, Content: "Q1"}, {ID: boxAnswer, Content: "A1"},
			}}},
			{PageID: "P2", Analysis: &snorg.PageAnalysisView{Regions: []snorg.RegionView{
				{ID: promptBoxID, Content: "Q2"}, {ID: boxAnswer, Content: "  "},
			}}},
		}}}},
	}
	h := handler(t, a, &fakeProvider{}, &fakeUploader{}, cfg())
	got, err := h.entries()
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	if len(got) != 1 || got[0].PageID != "P1" {
		t.Fatalf("entries = %+v, want only the answered page", got)
	}
	if got[0].Question != "Q1" || got[0].Answer != "A1" {
		t.Errorf("entry = %+v, want its question and answer", got[0])
	}
}

func TestEntriesAreNewestPageFirst(t *testing.T) {
	// The order comes from the page id, which encodes when the page was created on
	// the device — the only clock that says when the question was written.
	a := &fakeArchive{
		matches: []snorg.Match{{PageID: "P20260101000000000000aaaa"}, {PageID: "P20260822000000000000bbbb"}},
		result: &snorg.Result{Notes: []*snorg.NoteView{{Pages: []snorg.PageView{
			{PageID: "P20260101000000000000aaaa", Analysis: &snorg.PageAnalysisView{Regions: []snorg.RegionView{{ID: boxAnswer, Content: "older"}}}},
			{PageID: "P20260822000000000000bbbb", Analysis: &snorg.PageAnalysisView{Regions: []snorg.RegionView{{ID: boxAnswer, Content: "newer"}}}},
		}}}},
	}
	h := handler(t, a, &fakeProvider{}, &fakeUploader{}, cfg())
	got, err := h.entries()
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	if len(got) != 2 || got[0].Answer != "newer" {
		t.Fatalf("entries = %+v, want the newest page first", got)
	}
}

func TestPublishIsSilentWithoutAPDFCommand(t *testing.T) {
	// The default. The answer is in the archive; typesetting is opt-in.
	u := &fakeUploader{}
	h := handler(t, &fakeArchive{}, &fakeProvider{}, u, cfg())
	if err := h.publish(context.Background()); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(u.paths) != 0 {
		t.Error("something was uploaded with no pdf_command configured")
	}
}

func TestRenderReportsACommandThatWritesNothing(t *testing.T) {
	c := cfg()
	c.PDFCommand = "true" // succeeds, produces no document
	c.PDFTimeout = config.Duration(5 * time.Second)
	h := handler(t, &fakeArchive{}, &fakeProvider{}, &fakeUploader{}, c)

	if _, err := h.render(context.Background(), []byte("# hi\n")); err == nil {
		t.Fatal("render accepted a command that wrote no document")
	}
}

func TestRenderReportsTheCommandsOwnComplaint(t *testing.T) {
	c := cfg()
	c.PDFCommand = "echo 'no such font' >&2; exit 3"
	c.PDFTimeout = config.Duration(5 * time.Second)
	h := handler(t, &fakeArchive{}, &fakeProvider{}, &fakeUploader{}, c)

	_, err := h.render(context.Background(), []byte("# hi\n"))
	if err == nil {
		t.Fatal("render hid a failing command")
	}
	// The typesetter's message is the only clue an operator gets.
	if !strings.Contains(err.Error(), "no such font") {
		t.Errorf("error = %v, want the command's own output in it", err)
	}
}

func TestRenderRunsInADirectoryHoldingTheTemplate(t *testing.T) {
	// letter.pdf_command refers to the template by bare filename, which only works
	// because the command runs where both inputs were put.
	c := cfg()
	c.PDFCommand = "cp letters.tex {{out}}"
	c.PDFTimeout = config.Duration(5 * time.Second)
	h := handler(t, &fakeArchive{}, &fakeProvider{}, &fakeUploader{}, c)

	got, err := h.render(context.Background(), []byte("# hi\n"))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !bytes.Equal(got, latexTemplate) {
		t.Error("letters.tex was not next to the markdown in the working directory")
	}
}

// TestRenderTypesetsRealMarkdown is the check that the whole PDF path works: the
// embedded template, run against the kinds of answer a model actually produces. It is
// the only test that compiles letters.tex, and so the only one that can catch a
// template that a table or a formula makes fatal.
//
// It skips where pandoc and pdflatex are not installed, which is most development
// machines — so it is easy to believe it is passing when it never ran. The parent
// directory is mounted because go.mod's replaces point at the sibling checkouts:
//
//	podman run --rm -v "$PWD/..":/work:z -w /work/snorgd -e HOME=/tmp alpine:3 sh -c '
//	  apk add --no-cache go pandoc-cli texlive texmf-dist-latexrecommended \
//	    texmf-dist-latexextra texmf-dist-fontsrecommended &&
//	  go test ./internal/letter/'
func TestRenderTypesetsRealMarkdown(t *testing.T) {
	if _, err := exec.LookPath("pandoc"); err != nil {
		t.Skip("pandoc is not installed")
	}
	if _, err := exec.LookPath("pdflatex"); err != nil {
		t.Skip("pdflatex is not installed")
	}

	c := cfg()
	c.PDFCommand = "pandoc -f markdown --template=letters.tex -o {{out}} {{in}}"
	c.PDFTimeout = config.Duration(2 * time.Minute)
	h := handler(t, &fakeArchive{}, &fakeProvider{}, &fakeUploader{}, c)

	md := assemble([]entry{{
		PageID:   "P20260822143000123456abcd",
		Question: "How does the Gaussian integral work, with some Python?",
		Answer: strings.Join([]string{
			"Inline maths like $E = mc^2$, and display:",
			"",
			`$$\int_0^\infty e^{-x^2}\,dx = \frac{\sqrt{\pi}}{2}$$`,
			"",
			`\begin{align}`,
			`f(x) &= ax^2 + bx + c \\`,
			`     &= a(x-h)^2 + k`,
			`\end{align}`,
			"",
			"Prose with specials: 50% of a_b & c_d, file_name.txt, cost ~10.",
			"",
			"Polish, because that is what the questions are written in: zażółć",
			"gęślą jaźń, ĄĆĘŁŃÓŚŻŹ.",
			"",
			"- a list item with $\\alpha_i$",
			"- another one",
			"",
			"```python",
			"def gaussian(x, mu=0.0):",
			"    return math.exp(-((x - mu) ** 2) / 2)",
			"```",
			"",
			"| column | meaning |",
			"|--------|---------|",
			"| `mu`   | centre  |",
		}, "\n"),
	}, {
		PageID:   "P20260801090000000000abcd",
		Question: "A second entry?",
		Answer:   "So the document has more than one page.",
	}})

	pdf, err := h.render(context.Background(), md)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF")) {
		t.Fatalf("output is not a PDF (%d bytes, starts %q)", len(pdf), pdf[:min(8, len(pdf))])
	}
	if len(pdf) < 2000 {
		t.Errorf("PDF is %d bytes, suspiciously small for two entries", len(pdf))
	}
}
