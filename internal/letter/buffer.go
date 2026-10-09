package letter

import (
	"fmt"
	"strings"
)

// A templated page's editable buffer is a sequence of marker-introduced sections —
// one per title, link and template box — and the text of a section is every line
// after its marker up to the next one (snorg internal/edit/buffer.go, and the
// grammar at internal/archive/regions.go). Writing an answer into a page is
// therefore a matter of replacing one section's body and handing the whole buffer
// back, which is what these two functions do.
//
// Rebuilding the buffer from scratch is not an option: it carries the page's title
// and link transcriptions too, and snorg refuses a buffer whose sections do not
// cover exactly the ones it serialized.

// setRegion replaces the body of the section with the given box id, leaving every
// other section — including the transcriptions of the boxes the model read — exactly
// as it found them.
func setRegion(buffer, id, text string) (string, error) {
	lines := strings.Split(buffer, "\n")

	start := -1
	for i, line := range lines {
		if got, ok := regionMarker(line); ok && got == id {
			start = i
			break
		}
	}
	if start < 0 {
		return "", fmt.Errorf("the page's buffer has no %q region: the template must declare a box with that id", id)
	}

	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if isMarker(lines[i]) {
			end = i
			break
		}
	}

	out := make([]string, 0, len(lines))
	out = append(out, lines[:start+1]...)
	out = append(out, strings.Split(neutralize(text), "\n")...)
	out = append(out, lines[end:]...)
	return strings.Join(out, "\n"), nil
}

// regionMarker reports the box id a `<!-- region <id> [(label)] -->` line names.
func regionMarker(line string) (string, bool) {
	kind, key, ok := marker(line)
	if !ok || kind != "region" {
		return "", false
	}
	return key, true
}

// isMarker reports whether a line ends the section above it.
func isMarker(line string) bool {
	_, _, ok := marker(line)
	return ok
}

// marker parses the comment lines snorg's buffer is structured by. It mirrors
// snorg's own parser (internal/edit/buffer.go parseMarker) — deliberately, since a
// line this does not recognize as a boundary but snorg does would have the answer
// silently land in the wrong section.
func marker(line string) (kind, key string, ok bool) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "<!--") || !strings.HasSuffix(s, "-->") {
		return "", "", false
	}
	inner := strings.TrimSpace(s[len("<!--") : len(s)-len("-->")])
	fields := strings.Fields(inner)
	if len(fields) == 0 {
		return "", "", false
	}
	switch fields[0] {
	case "content":
		return "content", "", true
	case "title", "link", "region":
		if len(fields) < 2 {
			return "", "", false
		}
		return fields[0], fields[1], true
	}
	return "", "", false
}

// neutralize makes text safe to store as a section body.
//
// A section runs until the next marker line, so an answer containing something that
// parses as one would split itself in two and land half of itself under another box
// — or under an id that does not exist, which snorg refuses outright. A model asked
// about HTML comments can produce exactly that. Indenting the offending line by one
// space stops it parsing as a marker while leaving it legible; refusing the answer
// instead would throw away work already paid for.
func neutralize(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if isMarker(line) {
			lines[i] = " " + line
		}
	}
	return strings.Join(lines, "\n")
}
