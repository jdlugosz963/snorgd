// Package query parses snorgd's configured page selectors into snorg predicates. It
// exists so the daemon accepts the same query expressions the snorg CLI does, rather
// than inventing its own selection syntax.
package query

import (
	"fmt"
	"strings"

	"github.com/jdlugosz963/snorg/pkg/snorg"
)

// Parse builds a predicate from a query expression — terms joined by AND / OR / NOT
// and grouped with parentheses, exactly as the CLI's `snorg query` accepts.
//
//	unanalyzed
//	templated AND NOT tag~^ai-answered$
//	starred AND (ctime:2026-04-04..2026-09-12 OR content:"some thing")
//
// The expression is handed verbatim to snorg's own parser, so the accepted vocabulary
// tracks snorg without snorgd restating it; only the empty case is snorgd's own, since
// an unset `query:` key is a config mistake rather than a match-nothing selection.
func Parse(expr string) (snorg.Predicate, error) {
	if strings.TrimSpace(expr) == "" {
		return nil, fmt.Errorf("empty query (syntax: %s)", snorg.QuerySyntax)
	}
	return snorg.ParseQuery(expr)
}

// Selector runs a predicate over the archive. *snorg.Client implements it; the
// stages hold it as an interface so they can be tested without an archive.
type Selector interface {
	Query(pred snorg.Predicate) ([]snorg.Match, error)
}

// PageIDs runs pred against the archive and returns the matching PAGEIDs, dropping
// the FILE_ID half of each match — every stage downstream addresses pages.
func PageIDs(sel Selector, pred snorg.Predicate) ([]string, error) {
	matches, err := sel.Query(pred)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(matches))
	for i, m := range matches {
		ids[i] = m.PageID
	}
	return ids, nil
}
