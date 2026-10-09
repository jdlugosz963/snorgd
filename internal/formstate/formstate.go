// Package formstate is the dispatcher's memory: the last answers it saw on each
// templated page, and what has moved since.
//
// snif is deliberately stateless — reading the same page twice yields identical
// results and remembers nothing — so a page that has already been acted on is
// indistinguishable from one that has just been filled in. This package supplies the
// missing half. Each page's answers are snapshotted to its own file, and a form read
// later is compared against that snapshot field by field, so a handler is told not
// merely "here is a form" but "here is what changed on it".
//
// The snapshot lives in the state dir, outside the archive: it is a record of what
// the daemon has seen, not part of the notes, and it must never be committed. Losing
// it is safe in the ordinary sense — every page simply looks new again, and handlers
// redo work they had already done.
package formstate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/jdlugosz963/snif/pkg/snif"
)

// Field is one widget's answer, and whether it differs from the last snapshot.
//
// Changed is derived at read time and never stored: what is worth persisting is the
// answer, and the flag is only meaningful relative to the comparison that produced
// it.
type Field struct {
	Value   any  `json:"value"`
	Changed bool `json:"-"`
}

// State is one templated page as the dispatcher sees it: which template it was drawn
// on (the routing key), which page it is, and every widget's answer.
type State struct {
	Template  string           `json:"template"`
	PageID    string           `json:"page_id"`
	FileID    string           `json:"file_id"`
	Fields    map[string]Field `json:"fields"`
	UpdatedAt time.Time        `json:"updated_at"`

	// First reports a page never snapshotted before. Every one of its fields is
	// Changed — a page seen for the first time is entirely new information, and a
	// handler that acts on change must act on all of it.
	First bool `json:"-"`
}

// Changed reports whether anything on the page moved since the last snapshot.
func (s State) Changed() bool {
	for _, f := range s.Fields {
		if f.Changed {
			return true
		}
	}
	return false
}

// ChangedFields names the widgets that moved, sorted, so a handler can decide
// whether the change concerns it and a log line can say what happened.
func (s State) ChangedFields() []string {
	var out []string
	for name, f := range s.Fields {
		if f.Changed {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Value returns the named field's answer and whether the form carried that widget at
// all. An unset or unreadable widget is present with a nil value.
func (s State) Value(name string) (any, bool) {
	f, ok := s.Fields[name]
	return f.Value, ok
}

// IsChanged reports whether the named widget moved. A widget the form does not carry
// has not moved.
func (s State) IsChanged(name string) bool { return s.Fields[name].Changed }

// Store holds one snapshot file per page under dir.
type Store struct{ dir string }

// NewStore prepares the snapshot directory.
func NewStore(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("form state needs a directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("form state dir: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Diff reads f against the stored snapshot of the same page and returns the current
// state with every field flagged. It does not store anything: a handler that fails
// must see the same change again next time, so the snapshot only moves on Commit.
func (s *Store) Diff(f *snif.Form) (State, error) {
	if f == nil {
		return State{}, fmt.Errorf("form state: nil form")
	}
	cur := State{
		Template:  f.Template,
		PageID:    f.PageID,
		FileID:    f.FileID,
		Fields:    map[string]Field{},
		UpdatedAt: time.Now().UTC(),
	}

	if err := validID(f.PageID); err != nil {
		return State{}, err
	}

	prev, found, err := s.load(f.PageID)
	if err != nil {
		return State{}, err
	}
	cur.First = !found

	for name, val := range f.Values() {
		was, had := prev.Fields[name]
		// A widget appearing on a page that had none of it is a change in its own
		// right — a template gaining a widget must not read as "unchanged" merely
		// because the new widget happens to be unset.
		cur.Fields[name] = Field{Value: val, Changed: !found || !had || !reflect.DeepEqual(was.Value, val)}
	}
	return cur, nil
}

// Commit stores st as the new baseline for its page.
func (s *Store) Commit(st State) error {
	if err := validID(st.PageID); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("form state %s: %w", st.PageID, err)
	}
	data = append(data, '\n')
	path := s.path(st.PageID)
	// Written through a temp file in the same directory: a snapshot truncated by a
	// crash would be unreadable, and an unreadable snapshot is worse than a stale
	// one — it fails the page on every subsequent batch.
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("form state %s: %w", st.PageID, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("form state %s: %w", st.PageID, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("form state %s: %w", st.PageID, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("form state %s: %w", st.PageID, err)
	}
	return nil
}

// load reads a page's stored snapshot. A missing file is not an error: it is a page
// nobody has looked at yet.
func (s *Store) load(pageID string) (State, bool, error) {
	raw, err := os.ReadFile(s.path(pageID))
	switch {
	case os.IsNotExist(err):
		return State{}, false, nil
	case err != nil:
		return State{}, false, fmt.Errorf("form state %s: %w", pageID, err)
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return State{}, false, fmt.Errorf("form state %s: %w", pageID, err)
	}
	if st.Fields == nil {
		st.Fields = map[string]Field{}
	}
	return st, true, nil
}

// path is the snapshot file for a page.
func (s *Store) path(pageID string) string {
	return filepath.Join(s.dir, pageID+".json")
}

// validID guards the one place a page id becomes a filename. Ids are
// snorg-generated ("P2026…", alphanumeric) and need no escaping, so anything that
// could climb out of the state dir is a bug somewhere upstream and is refused rather
// than quietly rewritten into something that would land in the wrong file.
func validID(pageID string) error {
	if pageID == "" {
		return fmt.Errorf("form state: page id is empty")
	}
	if strings.ContainsAny(pageID, `/\`) || pageID == "." || pageID == ".." {
		return fmt.Errorf("form state: refusing page id %q as a filename", pageID)
	}
	return nil
}
