package letter

import (
	"strings"
	"testing"
)

// page is a templated-page buffer with two titles, a question and an empty answer.
const page = `<!-- title 1 (h1) -->
Study notes
<!-- link 1 → P9999 -->
See also
<!-- region input:prompt (Question) -->
What is a monad?
It is a two-line question.
<!-- region answer (Answer) -->
`

func TestSetRegionReplacesOnlyItsOwnSection(t *testing.T) {
	out, err := setRegion(page, boxAnswer, "A monad is a monoid\nin the category of endofunctors.")
	if err != nil {
		t.Fatalf("setRegion: %v", err)
	}
	for _, keep := range []string{"Study notes", "See also", "What is a monad?", "It is a two-line question."} {
		if !strings.Contains(out, keep) {
			t.Errorf("setRegion dropped %q:\n%s", keep, out)
		}
	}
	if !strings.Contains(out, "in the category of endofunctors.") {
		t.Errorf("the answer's second line is missing:\n%s", out)
	}
	// Every marker must survive: snorg refuses a buffer whose sections do not
	// cover exactly the ones it serialized.
	for _, marker := range []string{"<!-- title 1 (h1) -->", "<!-- link 1 → P9999 -->", "<!-- region input:prompt (Question) -->", "<!-- region answer (Answer) -->"} {
		if !strings.Contains(out, marker) {
			t.Errorf("marker %q was lost:\n%s", marker, out)
		}
	}
}

func TestSetRegionOverwritesAPreviousAnswer(t *testing.T) {
	once, err := setRegion(page, boxAnswer, "First answer.")
	if err != nil {
		t.Fatalf("setRegion: %v", err)
	}
	twice, err := setRegion(once, boxAnswer, "Second answer.")
	if err != nil {
		t.Fatalf("setRegion: %v", err)
	}
	if strings.Contains(twice, "First answer.") {
		t.Errorf("the old answer survived a rewrite:\n%s", twice)
	}
	if !strings.Contains(twice, "Second answer.") {
		t.Errorf("the new answer is missing:\n%s", twice)
	}
}

func TestSetRegionInAMiddleSectionStopsAtTheNextMarker(t *testing.T) {
	out, err := setRegion(page, "input:prompt", "A replaced question.")
	if err != nil {
		t.Fatalf("setRegion: %v", err)
	}
	if strings.Contains(out, "It is a two-line question.") {
		t.Errorf("the replacement ran past its own section:\n%s", out)
	}
	if !strings.Contains(out, "<!-- region answer (Answer) -->") {
		t.Errorf("the following section was eaten:\n%s", out)
	}
}

func TestSetRegionNeutralizesTextThatWouldSplitTheBuffer(t *testing.T) {
	// A model asked about HTML comments can produce a line that parses as a
	// marker. Left alone it would put half the answer under another box — or under
	// an id that does not exist, which snorg refuses outright.
	answer := "Markers look like this:\n<!-- region answer -->\nand that is all."
	out, err := setRegion(page, boxAnswer, answer)
	if err != nil {
		t.Fatalf("setRegion: %v", err)
	}
	if strings.Count(out, "<!-- region answer") != 2 {
		t.Fatalf("expected the real marker plus a neutralized one:\n%s", out)
	}
	if !strings.Contains(out, " <!-- region answer -->") {
		t.Errorf("the marker-like line was not neutralized:\n%s", out)
	}
	if !strings.Contains(out, "and that is all.") {
		t.Errorf("text after the marker-like line was lost:\n%s", out)
	}
}

func TestSetRegionNamesAMissingRegion(t *testing.T) {
	_, err := setRegion(page, "nosuchbox", "text")
	if err == nil {
		t.Fatal("setRegion accepted a region the buffer does not have")
	}
	if !strings.Contains(err.Error(), "nosuchbox") {
		t.Errorf("error = %v, want it to name the missing region", err)
	}
}

func TestMarkerMatchesSnorgsGrammar(t *testing.T) {
	tests := []struct {
		line string
		kind string
		key  string
		ok   bool
	}{
		{"<!-- region answer -->", "region", "answer", true},
		{"<!-- region answer (Answer) -->", "region", "answer", true},
		{"  <!-- region answer -->  ", "region", "answer", true},
		{"<!-- title 1 (h2) -->", "title", "1", true},
		{"<!-- link 2 → P1 -->", "link", "2", true},
		{"<!-- content -->", "content", "", true},
		{"<!-- region -->", "", "", false},
		{"<!-- unrelated comment -->", "", "", false},
		{"not a marker at all", "", "", false},
		{"<!-- region answer", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.line, func(t *testing.T) {
			kind, key, ok := marker(tc.line)
			if ok != tc.ok || kind != tc.kind || key != tc.key {
				t.Errorf("marker(%q) = %q,%q,%v; want %q,%q,%v", tc.line, kind, key, ok, tc.kind, tc.key, tc.ok)
			}
		})
	}
}
