// Package rules maps a note's path in the watched store onto an ingest decision:
// whether to ingest it at all, and which snorg tags the resulting note carries. It
// is the "INGEST" stage of the pipeline configuration — the only stage that runs per
// file rather than per batch, because it is the only one that knows where a note
// came from.
package rules

import (
	"fmt"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Rule is one glob-to-tags mapping. Match is a doublestar pattern against the
// source path ("/Supernote/Notes/**/*.note"); Tags are applied note-scoped after a
// successful ingest, so every page of the note inherits them. Skip marks a
// deliberate exclusion: the path is matched and then dropped, which is how a rule
// carves an exception out of a broader rule below it.
type Rule struct {
	Match string   `yaml:"match"`
	Tags  []string `yaml:"tags"`
	Skip  bool     `yaml:"skip"`
}

// Set is an ordered list of rules; the first match wins, so specific patterns
// belong above general ones.
type Set []Rule

// Match returns the first rule matching source and whether one did. Matching is
// case-insensitive: Dropbox preserves the case of a path but treats it as
// case-insensitive, so a rule written "/Supernote" must still match "/supernote".
//
// An empty Set matches everything with no tags — a daemon with no configuration
// behaves exactly as it did before rules existed. A non-empty Set that matches
// nothing returns false, and the caller drops the note: once rules exist, they are
// the allowlist.
func (s Set) Match(source string) (Rule, bool) {
	if len(s) == 0 {
		return Rule{}, true
	}
	src := strings.ToLower(source)
	for _, r := range s {
		if ok, err := doublestar.Match(strings.ToLower(r.Match), src); err == nil && ok {
			return r, true
		}
	}
	return Rule{}, false
}

// Validate reports the first malformed pattern. It is called at startup so a typo in
// a glob fails the daemon rather than silently matching nothing for weeks.
func (s Set) Validate() error {
	for i, r := range s {
		if strings.TrimSpace(r.Match) == "" {
			return fmt.Errorf("rule %d: match is required", i)
		}
		if !doublestar.ValidatePattern(r.Match) {
			return fmt.Errorf("rule %d: invalid glob %q", i, r.Match)
		}
	}
	return nil
}
