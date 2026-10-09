package dispatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jdlugosz963/snif/pkg/snif"
	"github.com/jdlugosz963/snorg/pkg/snorg"
)

// letterish is a template shaped like a real one: the three widget kinds a form
// uses, plus a plain box for a handler to write into.
func letterish() Template {
	return Template{
		Form: snif.Template{
			Name:  "probe",
			Title: "Probe",
			Requests: []snif.WidgetSpec{
				{Kind: snif.Slider, Name: "length", Label: "Length", Min: 100, Max: 2000, Step: 100},
				{Kind: snif.Radio, Name: "topic", Label: "Topic", Options: []string{"general", "study"}},
				{Kind: snif.Input, Name: "prompt", Label: "Question", Style: snif.Lines, H: 900},
			},
		},
		Boxes: []Box{{ID: "answer", Label: "Answer", Rect: Rect{X: 80, Y: 2400, W: 1760, H: 80}}},
	}
}

func TestWriteProducesAConfigSnorgAccepts(t *testing.T) {
	dir := t.TempDir()
	paths, err := Write(letterish(), dir, false)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("Write returned %v, want an image and a config", paths)
	}
	if filepath.Ext(paths[0]) != ".png" || filepath.Ext(paths[1]) != ".yaml" {
		t.Errorf("Write returned %v, want the image first", paths)
	}

	// The point of the appended-text approach: what comes out must still be a
	// config snorg's own loader accepts, not merely valid YAML.
	cfg, err := snorg.LoadConfig([]string{paths[1]})
	if err != nil {
		t.Fatalf("snorg.LoadConfig on the generated config: %v", err)
	}
	if len(cfg.Templates) != 1 {
		t.Fatalf("templates = %d, want 1", len(cfg.Templates))
	}

	ids := map[string]bool{}
	analyze := map[string]bool{}
	for _, b := range cfg.Templates[0].Boxes {
		ids[b.ID] = true
		analyze[b.ID] = b.Analyze
	}
	// Both halves survived: the widgets snif declared, and the box snorgd appended.
	for _, want := range []string{"slider:length:100:2000:100", "radio:topic", "radio:topic:general", "input:prompt", "answer"} {
		if !ids[want] {
			t.Errorf("box %q missing from the generated config", want)
		}
	}
	// The plain box must stay out of analysis: vision writing into it would
	// overwrite whatever a handler put there.
	if analyze["answer"] {
		t.Error("the appended box is marked analyze: true")
	}
	if !analyze["input:prompt"] {
		t.Error("the input widget is not marked analyze: true")
	}
}

func TestWriteRefusesToOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	if _, err := Write(letterish(), dir, false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := Write(letterish(), dir, false); err == nil {
		t.Fatal("Write overwrote an existing template without -force")
	}
	if _, err := Write(letterish(), dir, true); err != nil {
		t.Fatalf("Write with force: %v", err)
	}
}

func TestGenerateIsReproducible(t *testing.T) {
	// The image's hash is the template's identity, so generating twice from the
	// same declaration must produce the same bytes — otherwise regenerating
	// orphans every page already drawn on it.
	a, ay, err := Generate(letterish())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	b, by, err := Generate(letterish())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if a.Hash != b.Hash {
		t.Errorf("hash = %q then %q, want them equal", a.Hash, b.Hash)
	}
	if string(ay) != string(by) {
		t.Error("the generated config differs between runs")
	}
}

func TestAppendBoxesRejectsBadInput(t *testing.T) {
	g, err := snif.Generate(letterish().Form)
	if err != nil {
		t.Fatalf("snif.Generate: %v", err)
	}

	tests := []struct {
		name  string
		yaml  []byte
		boxes []Box
	}{
		{"a box with no id", g.YAML, []Box{{Label: "x"}}},
		{"two boxes with the same id", g.YAML, []Box{{ID: "answer"}, {ID: "answer"}}},
		{"a box colliding with a widget", g.YAML, []Box{{ID: "input:prompt"}}},
		{"a config that does not end in a box", []byte("templates:\n  - image: x.png\n"), []Box{{ID: "answer"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := appendBoxes(tc.yaml, tc.boxes); err == nil {
				t.Fatal("appendBoxes accepted invalid input, want error")
			}
		})
	}
}

func TestAppendBoxesWithoutBoxesChangesNothing(t *testing.T) {
	g, err := snif.Generate(letterish().Form)
	if err != nil {
		t.Fatalf("snif.Generate: %v", err)
	}
	out, err := appendBoxes(g.YAML, nil)
	if err != nil {
		t.Fatalf("appendBoxes: %v", err)
	}
	if string(out) != string(g.YAML) {
		t.Error("appendBoxes rewrote a config it had nothing to add to")
	}
}

func TestAppendBoxesKeepsTheRegenerationHeader(t *testing.T) {
	// The header records the command that drew the image; losing it would make the
	// template unre-renderable.
	_, yaml, err := Generate(letterish())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, err := snif.ReadGenCommand(yaml); err != nil {
		t.Errorf("ReadGenCommand on the appended config: %v", err)
	}
	if !strings.Contains(string(yaml), `{id: "answer"`) {
		t.Error("the appended box is not in the output")
	}
}

func TestFillHeightRunsTheWidgetToTheBottomMargin(t *testing.T) {
	form := letterish().Form
	h, err := FillHeight(form, "prompt")
	if err != nil {
		t.Fatalf("FillHeight: %v", err)
	}
	for i := range form.Requests {
		if form.Requests[i].Name == "prompt" {
			form.Requests[i].H = h
		}
	}
	g, err := snif.Generate(form)
	if err != nil {
		t.Fatalf("snif.Generate at the filled height: %v", err)
	}

	var margin, bottom int
	for _, w := range g.Layout.Widgets {
		if margin == 0 {
			margin = w.Rect.X
		}
		if w.Name == "prompt" {
			bottom = w.Rect.Y + w.Rect.H
		}
	}
	if bottom != snif.PageH-margin {
		t.Errorf("prompt ends at y=%d, want the bottom margin %d", bottom, snif.PageH-margin)
	}
}

func TestFillHeightNamesAMissingWidget(t *testing.T) {
	if _, err := FillHeight(letterish().Form, "nosuch"); err == nil {
		t.Fatal("FillHeight accepted a widget the template does not have")
	}
}

func TestWriteCreatesTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "out")
	if _, err := Write(letterish(), dir, false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "snif-probe.png")); err != nil {
		t.Errorf("image not written: %v", err)
	}
}
