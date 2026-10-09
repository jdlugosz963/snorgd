package rules

import "testing"

func TestMatchSelectsFirstRule(t *testing.T) {
	set := Set{
		{Match: "/Supernote/Scratch/**", Skip: true},
		{Match: "/Supernote/AI/*.note", Tags: []string{"ai", "question"}},
		{Match: "/Supernote/**/*.note", Tags: []string{"notes"}},
	}

	tests := []struct {
		name    string
		source  string
		matched bool
		skip    bool
		tags    []string
	}{
		{"specific rule wins over the general one below it", "/Supernote/AI/q.note", true, false, []string{"ai", "question"}},
		{"skip rule matches and drops", "/Supernote/Scratch/tmp.note", true, true, nil},
		{"doublestar crosses directories", "/Supernote/Work/2026/a.note", true, false, []string{"notes"}},
		// "**/" must match zero segments, or a rule can never match a note sitting
		// directly in the watched folder.
		{"doublestar matches zero segments", "/Supernote/a.note", true, false, []string{"notes"}},
		// Dropbox preserves path case but is case-insensitive; a rule must not miss
		// a note because the device capitalised the folder differently.
		{"matching is case-insensitive", "/supernote/ai/Q.NOTE", true, false, []string{"ai", "question"}},
		{"unmatched path is dropped", "/Photos/holiday.note", false, false, nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := set.Match(tc.source)
			if ok != tc.matched {
				t.Fatalf("Match(%q) matched = %v, want %v", tc.source, ok, tc.matched)
			}
			if !ok {
				return
			}
			if got.Skip != tc.skip {
				t.Errorf("Match(%q) skip = %v, want %v", tc.source, got.Skip, tc.skip)
			}
			if len(got.Tags) != len(tc.tags) {
				t.Fatalf("Match(%q) tags = %v, want %v", tc.source, got.Tags, tc.tags)
			}
			for i, tag := range tc.tags {
				if got.Tags[i] != tag {
					t.Errorf("Match(%q) tags[%d] = %q, want %q", tc.source, i, got.Tags[i], tag)
				}
			}
		})
	}
}

func TestEmptySetMatchesEverythingUntagged(t *testing.T) {
	got, ok := Set{}.Match("/anything/at/all.note")
	if !ok {
		t.Fatal("empty Set did not match; a daemon with no rules must ingest everything")
	}
	if got.Skip || len(got.Tags) != 0 {
		t.Fatalf("empty Set returned %+v, want a bare rule", got)
	}
}

func TestValidateRejectsBadRules(t *testing.T) {
	if err := (Set{{Match: "/ok/**"}, {Match: "  "}}).Validate(); err == nil {
		t.Error("Validate accepted an empty match pattern")
	}
	if err := (Set{{Match: "/a/[unclosed"}}).Validate(); err == nil {
		t.Error("Validate accepted an invalid glob")
	}
	if err := (Set{{Match: "/Supernote/**/*.note", Tags: []string{"notes"}}}).Validate(); err != nil {
		t.Errorf("Validate rejected a valid rule: %v", err)
	}
}
