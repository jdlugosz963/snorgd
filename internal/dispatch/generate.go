package dispatch

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jdlugosz963/snif/pkg/snif"
)

// Generate renders a template: the PNG to import on the device, and the snorg
// templates: section that tells an archive what the picture's regions mean.
//
// snif produces both from one layout, so the rects in the YAML are exactly the rects
// in the image. A handler's plain boxes are appended to that YAML afterwards, because
// snif only knows how to declare widgets — and a box that is not a widget is exactly
// what a handler needs in order to have somewhere on the page to write.
func Generate(t Template) (*snif.Generated, []byte, error) {
	g, err := snif.Generate(t.Form)
	if err != nil {
		return nil, nil, fmt.Errorf("template %q: %w", t.Name(), err)
	}
	yaml, err := appendBoxes(g.YAML, t.Boxes)
	if err != nil {
		return nil, nil, fmt.Errorf("template %q: %w", t.Name(), err)
	}
	return g, yaml, nil
}

// Write renders t into dir and returns the paths written, newest-first in the order
// image, config.
//
// Existing files are refused unless force, so regenerating cannot quietly discard a
// template a device is already drawing on — the image's hash is its identity, and a
// changed image orphans every page drawn on the old one.
func Write(t Template, dir string, force bool) ([]string, error) {
	g, yaml, err := Generate(t)
	if err != nil {
		return nil, err
	}
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	paths := []string{filepath.Join(dir, g.Image), filepath.Join(dir, g.File)}
	if !force {
		for _, p := range paths {
			if _, err := os.Stat(p); err == nil {
				return nil, fmt.Errorf("%s exists (use -force to overwrite)", p)
			}
		}
	}
	for i, data := range [][]byte{g.PNG, yaml} {
		if err := os.WriteFile(paths[i], data, 0o644); err != nil {
			return nil, err
		}
	}
	return paths, nil
}

// boxIndent is the indentation snif writes its box entries at — two levels under
// `templates:`, one list item deep.
const boxIndent = "      "

// appendBoxes adds plain boxes to the end of a generated template's box list.
//
// It appends text rather than re-marshalling so snif's header survives: those comment
// lines record the command that produced the image, which is how the template is
// re-rendered later. The cost is a dependency on snif's output shape, so the shape is
// checked rather than assumed — a generator that stopped ending in a box list would
// otherwise produce a config that parses and is quietly wrong.
func appendBoxes(yaml []byte, boxes []Box) ([]byte, error) {
	if len(boxes) == 0 {
		return yaml, nil
	}
	trimmed := bytes.TrimRight(yaml, "\n")
	lines := bytes.Split(trimmed, []byte("\n"))
	last := string(lines[len(lines)-1])
	if !strings.HasPrefix(last, boxIndent+"- {") {
		return nil, fmt.Errorf("generated config does not end in a box entry (last line: %q); snif's output shape changed", last)
	}

	var b bytes.Buffer
	b.Write(trimmed)
	b.WriteString("\n")
	seen := map[string]bool{}
	for _, box := range boxes {
		if strings.TrimSpace(box.ID) == "" {
			return nil, fmt.Errorf("a box needs an id")
		}
		if seen[box.ID] {
			return nil, fmt.Errorf("duplicate box id %q", box.ID)
		}
		seen[box.ID] = true
		if bytes.Contains(trimmed, []byte("{id: "+strconv.Quote(box.ID)+",")) {
			return nil, fmt.Errorf("box id %q collides with a widget box", box.ID)
		}
		b.WriteString(boxLine(box))
	}
	return b.Bytes(), nil
}

// boxLine renders one box as snif renders its own: a flow mapping on a single line,
// so the appended entries are indistinguishable from the generated ones in a diff.
func boxLine(box Box) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s- {id: %s, label: %s, rect: {x: %d, y: %d, w: %d, h: %d}, analyze: %t",
		boxIndent, strconv.Quote(box.ID), strconv.Quote(box.Label),
		box.Rect.X, box.Rect.Y, box.Rect.W, box.Rect.H, box.Analyze)
	if box.Prompt != "" {
		fmt.Fprintf(&b, ", prompt: %s", strconv.Quote(box.Prompt))
	}
	b.WriteString("}\n")
	return b.String()
}

// FillHeight sizes a widget so it runs to the bottom margin of the page.
//
// snif has no "fill the rest" request: a body height is a number of pixels. Rather
// than restate snif's layout arithmetic here — margins, gaps, caption heights, all of
// which are snif's to change — the template is laid out once with the widget at its
// smallest, and the height is read off where snif actually put it. The first widget's
// left edge is the page margin, and the bottom margin matches it.
func FillHeight(t snif.Template, name string) (int, error) {
	probe := t
	probe.Requests = append([]snif.WidgetSpec(nil), t.Requests...)
	found := false
	for i := range probe.Requests {
		if probe.Requests[i].Name == name {
			probe.Requests[i].H = 1
			found = true
		}
	}
	if !found {
		return 0, fmt.Errorf("template %q has no widget named %q", t.Name, name)
	}

	g, err := snif.Generate(probe)
	if err != nil {
		return 0, err
	}
	for _, w := range g.Layout.Widgets {
		if w.Name != name {
			continue
		}
		h := snif.PageH - w.Rect.X - w.Rect.Y
		if h <= 0 {
			return 0, fmt.Errorf("widget %q starts at y=%d, leaving no room on a %d px page", name, w.Rect.Y, snif.PageH)
		}
		return h, nil
	}
	return 0, fmt.Errorf("widget %q was not placed", name)
}
