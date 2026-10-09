package letter

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// latexTemplate is the document's look, and the one part of the PDF pipeline snorgd
// owns outright. It is embedded rather than kept in the archive: the archive is the
// notes, and snorgd writes nothing into it that is not a note.
//
//go:embed letters.tex
var latexTemplate []byte

// The names the two inputs are given in the working directory. They are part of the
// interface, not an implementation detail: letter.pdf_command names them.
const (
	markdownFile = "letters.md"
	templateFile = "letters.tex"
	outputFile   = "letters.pdf"
)

// LaTeXTemplate returns the embedded document template, for `snorgd gen-templates
// -print-tex` to show what it would compile against.
func LaTeXTemplate() []byte { return latexTemplate }

// entry is one answered page in the collected document.
type entry struct {
	PageID   string
	Question string
	Answer   string
}

// publish rebuilds the whole document from the archive and uploads it. Flush drives
// it, once per batch.
//
// Rebuilt, not appended to: the archive is the record of every answer, so there is no
// second copy of the history to keep in step with it, and an answer edited in the
// repo shows up in the next PDF without anything having to notice.
func (h *Handler) publish(ctx context.Context) error {
	if strings.TrimSpace(h.cfg.PDFCommand) == "" {
		return nil
	}
	entries, err := h.entries()
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	pdf, err := h.render(ctx, assemble(entries))
	if err != nil {
		return err
	}
	return h.up.Upload(ctx, path.Join(h.cfg.DropboxDir, h.cfg.PDFName), pdf)
}

// entries collects every answered page, newest first.
//
// The order comes from the page id, which snorg mints as "P" followed by the moment
// the page was created on the device. Sorting on it is therefore reverse
// chronological by the only clock that matters here — when the question was written —
// and it needs no bookkeeping of snorgd's own to stay true.
func (h *Handler) entries() ([]entry, error) {
	matches, err := h.arch.Query(h.answered)
	if err != nil {
		return nil, fmt.Errorf("find answered pages: %w", err)
	}
	if len(matches) == 0 {
		return nil, nil
	}
	ids := make([]string, len(matches))
	for i, m := range matches {
		ids[i] = m.PageID
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))

	res, err := h.arch.Retrieve(ids)
	if err != nil {
		return nil, fmt.Errorf("read answered pages: %w", err)
	}

	byID := map[string]entry{}
	for _, note := range res.Notes {
		for _, page := range note.Pages {
			if page.Analysis == nil {
				continue
			}
			var e entry
			e.PageID = page.PageID
			for _, r := range page.Analysis.Regions {
				switch r.ID {
				case boxAnswer:
					e.Answer = strings.TrimSpace(r.Content)
				case promptBoxID:
					e.Question = strings.TrimSpace(r.Content)
				}
			}
			if e.Answer != "" {
				byID[page.PageID] = e
			}
		}
	}

	out := make([]entry, 0, len(byID))
	for _, id := range ids {
		if e, ok := byID[id]; ok {
			out = append(out, e)
		}
	}
	return out, nil
}

// promptBoxID is the question widget's box id, which is what a region carries — the
// widget name is snif's view of it, the id is snorg's.
const promptBoxID = "input:" + widgetPrompt

// assemble turns the collected answers into the markdown pandoc converts.
//
// Each entry is a section, because the template gives every section its own page; the
// question is a block quote, which the template sets apart from the answer.
func assemble(entries []entry) []byte {
	var b bytes.Buffer
	for _, e := range entries {
		fmt.Fprintf(&b, "# %s\n\n", heading(e.PageID))
		if e.Question != "" {
			for _, line := range strings.Split(e.Question, "\n") {
				fmt.Fprintf(&b, "> %s\n", line)
			}
			b.WriteString("\n")
		}
		b.WriteString(e.Answer)
		b.WriteString("\n\n")
	}
	return b.Bytes()
}

// heading dates an entry from its page id ("P" + YYYYMMDDhhmmss + more). An id that
// does not carry a readable date falls back to itself, which is still a stable
// heading and still tells a reader which page they are looking at.
func heading(pageID string) string {
	if len(pageID) >= 15 && (pageID[0] == 'P' || pageID[0] == 'p') {
		if t, err := time.Parse("20060102150405", pageID[1:15]); err == nil {
			return t.Format("2 January 2006, 15:04")
		}
	}
	return pageID
}

// render typesets the markdown by running the configured command.
//
// Both inputs are materialized into one temp directory and the command runs there, so
// a template reference in the command line is a bare filename and the whole thing —
// markdown, template, output — is gone again afterwards. Nothing is left on disk, and
// nothing is written into the archive.
func (h *Handler) render(ctx context.Context, markdown []byte) ([]byte, error) {
	dir, err := os.MkdirTemp("", "snorgd-letters-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	in := filepath.Join(dir, markdownFile)
	out := filepath.Join(dir, outputFile)
	if err := os.WriteFile(in, markdown, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, templateFile), latexTemplate, 0o600); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, h.cfg.PDFTimeout.Duration())
	defer cancel()

	cmdline := substitute(h.cfg.PDFCommand, in, out)
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdline)
	cmd.Dir = dir
	// Combined, because a typesetter says why it failed on whichever stream it
	// feels like and the message is the only clue a daemon's operator gets.
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("%s: %w: %s", h.cfg.PDFCommand, err, strings.TrimSpace(string(output)))
	}

	pdf, err := os.ReadFile(out)
	if err != nil {
		return nil, fmt.Errorf("%s ran but wrote no document to {{out}}: %w", h.cfg.PDFCommand, err)
	}
	if len(pdf) == 0 {
		return nil, fmt.Errorf("%s produced an empty document", h.cfg.PDFCommand)
	}
	return pdf, nil
}

// substitute fills the command's two placeholders.
func substitute(cmdline, in, out string) string {
	return strings.NewReplacer("{{in}}", in, "{{out}}", out).Replace(cmdline)
}
