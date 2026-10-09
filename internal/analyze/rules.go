// Package analyze is the ANALYZE stage: it selects pages by their snorg tags and
// hands them to snorg's vision analysis. It is the tag-side counterpart of the
// ingest rules — those match a note's *path* and write the tags, these match the
// tags and decide which pages are transcribed — and the two meet at
// snorg.EffectiveTags, which unions a note's tags with a page's own. That union is
// what lets a rule cover every page of every note a broad ingest rule tagged while
// a single page opts out by carrying an excluded tag of its own.
package analyze

import (
	"fmt"
	"strings"

	"github.com/jdlugosz963/snorg/pkg/snorg"
)

// Rule is one tag selection. Tags are the tags that admit a page — any one of them
// is enough, and an empty list admits every page, which makes an exclude-only rule
// spellable. Exclude overrides: a page carrying any of them is not analyzed by this
// rule however it was admitted.
//
// Both lists hold exact tag strings rather than globs. A tag is a label somebody
// typed deliberately, so matching it loosely would be a way to analyze the wrong
// pages quietly.
type Rule struct {
	Tags    []string `yaml:"tags"`
	Exclude []string `yaml:"exclude"`
}

// Set is a list of rules. Unlike the ingest rules it is unordered: a rule carries no
// payload beyond "analyze these", so a page is analyzed when *any* rule admits it and
// there is nothing for a first match to win.
type Set []Rule

// Predicate builds the page selector for one rule.
func (r Rule) Predicate() snorg.Predicate {
	pred := snorg.Predicate(snorg.MatchAll)
	if len(r.Tags) > 0 {
		pred = snorg.MatchOr(tagPreds(r.Tags)...)
	}
	if len(r.Exclude) == 0 {
		return pred
	}
	return snorg.MatchAnd(pred, snorg.MatchNot(snorg.MatchOr(tagPreds(r.Exclude)...)))
}

// Predicate builds the selector for the whole set: a page is analyzed when at least
// one rule admits it.
//
// An empty Set matches everything, the same convention the ingest rules use — a
// stage switched on without rules does the obvious thing rather than nothing.
func (s Set) Predicate() snorg.Predicate {
	if len(s) == 0 {
		return snorg.MatchAll
	}
	preds := make([]snorg.Predicate, len(s))
	for i, r := range s {
		preds[i] = r.Predicate()
	}
	return snorg.MatchOr(preds...)
}

// Validate reports the first unusable rule. It is called at startup so a typo fails
// the daemon rather than silently selecting the wrong pages — or none — for weeks.
func (s Set) Validate() error {
	for i, r := range s {
		for _, t := range append(append([]string{}, r.Tags...), r.Exclude...) {
			if strings.TrimSpace(t) == "" {
				return fmt.Errorf("rule %d: a tag cannot be empty", i)
			}
		}
		// A tag on both sides makes the rule unsatisfiable, which is never what
		// anyone means to write.
		excluded := make(map[string]bool, len(r.Exclude))
		for _, t := range r.Exclude {
			excluded[t] = true
		}
		for _, t := range r.Tags {
			if excluded[t] {
				return fmt.Errorf("rule %d: tag %q is both included and excluded, so the rule can never match", i, t)
			}
		}
	}
	return nil
}

// tagPreds turns tags into one exact-match predicate each, against the page's
// effective tags (its own plus the ones inherited from its note).
func tagPreds(tags []string) []snorg.Predicate {
	preds := make([]snorg.Predicate, len(tags))
	for i, t := range tags {
		preds[i] = snorg.MatchTag(snorg.Exact(t))
	}
	return preds
}
