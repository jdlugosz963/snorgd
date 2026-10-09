package formstate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jdlugosz963/snif/pkg/snif"
)

// form builds a snif form carrying the given answers. Widget kinds are irrelevant
// here — Diff compares the values Form.Values() produces, whatever produced them.
func form(t *testing.T, pageID string, vals map[string]any) *snif.Form {
	t.Helper()
	f := &snif.Form{PageID: pageID, FileID: "F1", Template: "letter"}
	for name, v := range vals {
		w := snif.Widget{Name: name, Set: true}
		switch tv := v.(type) {
		case nil:
			w.Set = false
		case bool:
			w.Kind, w.Bool = snif.Check, tv
		case string:
			w.Kind, w.Text = snif.Input, tv
		case float64:
			w.Kind, w.Number = snif.Slider, tv
		default:
			t.Fatalf("unsupported test value %T", v)
		}
		f.Widgets = append(f.Widgets, w)
	}
	return f
}

func store(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "forms"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func TestDiffOnFirstSightFlagsEveryField(t *testing.T) {
	s := store(t)
	st, err := s.Diff(form(t, "P1", map[string]any{"prompt": "hello", "length": 400.0}))
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !st.First {
		t.Error("First = false on a page never snapshotted")
	}
	if !st.Changed() {
		t.Error("Changed() = false on a page never snapshotted")
	}
	if got := st.ChangedFields(); len(got) != 2 {
		t.Errorf("ChangedFields() = %v, want both", got)
	}
	if st.Template != "letter" || st.PageID != "P1" || st.FileID != "F1" {
		t.Errorf("state identity = %+v, want it carried from the form", st)
	}
}

func TestDiffAfterCommitFlagsNothing(t *testing.T) {
	s := store(t)
	f := form(t, "P1", map[string]any{"prompt": "hello", "length": 400.0})
	st, err := s.Diff(f)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if err := s.Commit(st); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	again, err := s.Diff(f)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if again.First {
		t.Error("First = true on a page already snapshotted")
	}
	if again.Changed() {
		t.Errorf("Changed() = true for an identical form; moved: %v", again.ChangedFields())
	}
}

func TestDiffFlagsOnlyTheFieldThatMoved(t *testing.T) {
	s := store(t)
	st, _ := s.Diff(form(t, "P1", map[string]any{"prompt": "hello", "length": 400.0}))
	if err := s.Commit(st); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	moved, err := s.Diff(form(t, "P1", map[string]any{"prompt": "hello", "length": 900.0}))
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	got := moved.ChangedFields()
	if len(got) != 1 || got[0] != "length" {
		t.Errorf("ChangedFields() = %v, want [length]", got)
	}
	if !moved.IsChanged("length") || moved.IsChanged("prompt") {
		t.Error("IsChanged disagrees with ChangedFields")
	}
	if v, ok := moved.Value("length"); !ok || v != 900.0 {
		t.Errorf("Value(length) = %v (%v), want 900", v, ok)
	}
}

func TestDiffTreatsANewWidgetAsAChange(t *testing.T) {
	// A template that gains a widget must not read as unchanged just because the
	// new widget is unset — the page really is offering something it did not before.
	s := store(t)
	st, _ := s.Diff(form(t, "P1", map[string]any{"prompt": "hello"}))
	if err := s.Commit(st); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	grown, err := s.Diff(form(t, "P1", map[string]any{"prompt": "hello", "topic": nil}))
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	got := grown.ChangedFields()
	if len(got) != 1 || got[0] != "topic" {
		t.Errorf("ChangedFields() = %v, want [topic]", got)
	}
}

func TestDiffSeesAnEmptyAnswerFillIn(t *testing.T) {
	// The transition that matters most for a handler with a blank-input guard: a
	// widget read as empty, then written on.
	s := store(t)
	st, _ := s.Diff(form(t, "P1", map[string]any{"prompt": ""}))
	if err := s.Commit(st); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	filled, err := s.Diff(form(t, "P1", map[string]any{"prompt": "what is a monad"}))
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !filled.IsChanged("prompt") {
		t.Error("an empty prompt later written on did not read as changed")
	}
}

func TestDiffWithoutCommitLeavesTheBaselineAlone(t *testing.T) {
	// The retry contract: a handler that fails must see the same change again.
	s := store(t)
	st, _ := s.Diff(form(t, "P1", map[string]any{"prompt": "hello"}))
	if err := s.Commit(st); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	changed := form(t, "P1", map[string]any{"prompt": "goodbye"})
	if first, _ := s.Diff(changed); !first.IsChanged("prompt") {
		t.Fatal("setup: the change was not seen at all")
	}
	second, err := s.Diff(changed)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !second.IsChanged("prompt") {
		t.Error("an uncommitted change was forgotten on the next read")
	}
}

func TestDiffSeparatesPages(t *testing.T) {
	s := store(t)
	st, _ := s.Diff(form(t, "P1", map[string]any{"prompt": "hello"}))
	if err := s.Commit(st); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	other, err := s.Diff(form(t, "P2", map[string]any{"prompt": "hello"}))
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !other.First {
		t.Error("a different page inherited P1's snapshot")
	}
}

func TestCommitSurvivesAReopenedStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "forms")
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	st, _ := s.Diff(form(t, "P1", map[string]any{"prompt": "hello"}))
	if err := s.Commit(st); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	reopened, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	again, err := reopened.Diff(form(t, "P1", map[string]any{"prompt": "hello"}))
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if again.First || again.Changed() {
		t.Error("the snapshot did not survive a restart")
	}
}

func TestCommitLeavesNoTempFilesBehind(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "forms")
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	st, _ := s.Diff(form(t, "P1", map[string]any{"prompt": "hello"}))
	if err := s.Commit(st); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "P1.json" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("state dir holds %v, want only P1.json", names)
	}
}

func TestStoreRefusesAPageIDThatIsAPath(t *testing.T) {
	s := store(t)
	if _, err := s.Diff(form(t, "../escape", nil)); err == nil {
		t.Error("Diff accepted a page id containing a separator")
	}
	if err := s.Commit(State{PageID: "../escape"}); err == nil {
		t.Error("Commit accepted a page id containing a separator")
	}
	if err := s.Commit(State{PageID: ""}); err == nil {
		t.Error("Commit accepted an empty page id")
	}
}

func TestDiffRejectsAnUnreadableSnapshot(t *testing.T) {
	// A corrupt snapshot must fail the page loudly rather than read as "new" and
	// have every handler redo its work.
	dir := filepath.Join(t.TempDir(), "forms")
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "P1.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := s.Diff(form(t, "P1", map[string]any{"prompt": "hello"})); err == nil {
		t.Error("Diff accepted a corrupt snapshot")
	}
}
