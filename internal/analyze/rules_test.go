package analyze

import (
	"testing"

	"github.com/jdlugosz963/snorg/pkg/snorg"
)

// page builds the value a predicate examines. The Client half is nil: tag matching
// reads the two documents and never touches the archive.
func page(noteTags, pageTags []string) snorg.Page {
	return snorg.Page{
		Note: snorg.NoteDoc{Tags: noteTags},
		Doc:  snorg.PageDoc{Tags: pageTags},
	}
}

// match runs pred on p, failing the test if the predicate could not decide.
func match(t *testing.T, pred snorg.Predicate, p snorg.Page) bool {
	t.Helper()
	ok, err := pred(p)
	if err != nil {
		t.Fatalf("predicate: %v", err)
	}
	return ok
}

func TestExcludedPageOptsOutOfItsNotesTag(t *testing.T) {
	// The reason the stage exists: an ingest rule tags a whole note, so every one of
	// its pages inherits the tag, and a single page opts out by carrying an excluded
	// tag of its own.
	set := Set{{Tags: []string{"notes"}, Exclude: []string{"no-ai"}}}
	pred := set.Predicate()

	if !match(t, pred, page([]string{"notes"}, nil)) {
		t.Error("a page inheriting the note's tag was not selected")
	}
	if match(t, pred, page([]string{"notes"}, []string{"no-ai"})) {
		t.Error("a page carrying the excluded tag was selected anyway")
	}
	if match(t, pred, page(nil, nil)) {
		t.Error("an untagged page was selected")
	}
}

func TestPredicate(t *testing.T) {
	tests := []struct {
		name string
		set  Set
		page snorg.Page
		want bool
	}{
		{"empty set matches everything", nil, page(nil, nil), true},
		{"any one tag is enough", Set{{Tags: []string{"a", "b"}}}, page(nil, []string{"b"}), true},
		{"no tag in common", Set{{Tags: []string{"a", "b"}}}, page(nil, []string{"c"}), false},
		{"exclude-only rule admits an untagged page", Set{{Exclude: []string{"no-ai"}}}, page(nil, nil), true},
		{"exclude-only rule still excludes", Set{{Exclude: []string{"no-ai"}}}, page(nil, []string{"no-ai"}), false},
		{"exclusion beats a note tag", Set{{Tags: []string{"a"}, Exclude: []string{"x"}}}, page([]string{"a"}, []string{"x"}), false},
		{"a page's own tag counts too", Set{{Tags: []string{"a"}}}, page(nil, []string{"a"}), true},
		{"rules are OR-ed", Set{{Tags: []string{"a"}}, {Tags: []string{"b"}}}, page(nil, []string{"b"}), true},
		{"another rule can admit an excluded page", Set{{Tags: []string{"a"}, Exclude: []string{"x"}}, {Tags: []string{"x"}}}, page(nil, []string{"a", "x"}), true},
		{"tags match exactly, not loosely", Set{{Tags: []string{"note"}}}, page(nil, []string{"notes"}), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := match(t, tc.set.Predicate(), tc.page); got != tc.want {
				t.Errorf("Predicate() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	ok := []struct {
		name string
		set  Set
	}{
		{"empty set", nil},
		{"tags and exclude", Set{{Tags: []string{"a"}, Exclude: []string{"b"}}}},
		{"exclude only", Set{{Exclude: []string{"b"}}}},
		{"a tag excluded by a different rule", Set{{Tags: []string{"a"}}, {Exclude: []string{"a"}}}},
	}
	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.set.Validate(); err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}

	bad := []struct {
		name string
		set  Set
	}{
		{"unsatisfiable rule", Set{{Tags: []string{"a"}, Exclude: []string{"a"}}}},
		{"empty tag", Set{{Tags: []string{""}}}},
		{"whitespace tag", Set{{Exclude: []string{"  "}}}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.set.Validate(); err == nil {
				t.Error("Validate() accepted an unusable rule, want error")
			}
		})
	}
}
