package query

import (
	"testing"

	"github.com/jdlugosz963/snorg/pkg/snorg"
)

func TestParseAcceptsTheQueryVocabulary(t *testing.T) {
	valid := []string{
		"all",
		"unanalyzed",
		"starred",
		"templated",
		"tag~^ai$",
		"tag=ai",
		"keyword:todo",
		`content:"some thing"`,
		"region[input:prompt]:monad",
		"ctime:today",
		"mtime:2026-04-04..2026-09-12",
		"dtime:..2026-09-12",
		"NOT tag~^ai-answered$",
		// Composition is the point: this is how a dispatch query narrows.
		"templated AND NOT tag~^ai-answered$",
		"starred OR (unanalyzed AND keyword:x)",
		// Surrounding whitespace must not matter.
		"  unanalyzed  ",
	}
	for _, expr := range valid {
		if _, err := Parse(expr); err != nil {
			t.Errorf("Parse(%q) = %v, want nil", expr, err)
		}
	}
}

func TestParseRejectsBadQueries(t *testing.T) {
	invalid := []struct {
		name string
		expr string
	}{
		{"empty", ""},
		{"whitespace only", "   "},
		{"unknown term", "nonsense"},
		{"missing value", "tag"},
		{"value on a bare term", "starred:x"},
		{"invalid regexp", "tag~["},
		{"bad date", "ctime:notaday"},
		{"dangling operator", "unanalyzed AND"},
		{"unbalanced parens", "(starred OR unanalyzed"},
		// The old filter syntax composed with "|"; it is not the language any more,
		// so a config carrying it must fail loudly rather than silently match wrong.
		{"legacy pipe", "unanalyzed | starred"},
		{"legacy filter word", "tag ^ai$"},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(tc.expr); err == nil {
				t.Fatalf("Parse(%q) returned no error", tc.expr)
			}
		})
	}
}

func TestPageIDsOnAnEmptyArchive(t *testing.T) {
	c, err := snorg.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("snorg.Open: %v", err)
	}
	pred, err := Parse("all")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ids, err := PageIDs(c, pred)
	if err != nil {
		t.Fatalf("PageIDs: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("PageIDs = %v, want none", ids)
	}
}
