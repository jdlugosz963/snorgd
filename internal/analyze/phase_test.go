package analyze

import (
	"context"
	"errors"
	"testing"

	"github.com/jdlugosz963/snorg/pkg/snorg"
)

// fakeArchive answers the page query and records what was handed to Analyze.
type fakeArchive struct {
	matches  []snorg.Match
	queryErr error

	results    []snorg.AnalyzeResult
	analyzeErr error

	gotPages []string
	calls    int
}

func (a *fakeArchive) Query(snorg.Predicate) ([]snorg.Match, error) {
	return a.matches, a.queryErr
}

func (a *fakeArchive) Analyze(ctx context.Context, _ snorg.Provider, pageIDs []string, opts snorg.AnalyzeOptions) ([]snorg.AnalyzeResult, error) {
	a.calls++
	a.gotPages = pageIDs
	for _, r := range a.results {
		if opts.OnResult != nil {
			opts.OnResult(r)
		}
	}
	return a.results, a.analyzeErr
}

func result(pageID string, skipped bool, err error) snorg.AnalyzeResult {
	return snorg.AnalyzeResult{PageResult: snorg.PageResult{PageID: pageID, Skipped: skipped}, Err: err}
}

func TestRunAnalyzesTheSelectedPages(t *testing.T) {
	arch := &fakeArchive{
		matches: []snorg.Match{{FileID: "F1", PageID: "P1"}, {FileID: "F1", PageID: "P2"}},
		results: []snorg.AnalyzeResult{result("P1", false, nil), result("P2", false, nil)},
	}
	n, err := NewPhase(arch, nil, Set{{Tags: []string{"notes"}}}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
	if len(arch.gotPages) != 2 || arch.gotPages[0] != "P1" || arch.gotPages[1] != "P2" {
		t.Errorf("analyzed %v, want [P1 P2]", arch.gotPages)
	}
}

func TestRunDoesNotCountSkippedPages(t *testing.T) {
	// The commit message counts work done, and a fingerprint skip is work that did
	// not happen — an all-skipped batch must report nothing.
	arch := &fakeArchive{
		matches: []snorg.Match{{PageID: "P1"}, {PageID: "P2"}},
		results: []snorg.AnalyzeResult{result("P1", true, nil), result("P2", false, nil)},
	}
	n, err := NewPhase(arch, nil, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != 1 {
		t.Errorf("count = %d, want 1", n)
	}
}

func TestRunSurvivesOnePageFailing(t *testing.T) {
	arch := &fakeArchive{
		matches: []snorg.Match{{PageID: "P1"}, {PageID: "P2"}},
		results: []snorg.AnalyzeResult{result("P1", false, errors.New("model refused")), result("P2", false, nil)},
	}
	n, err := NewPhase(arch, nil, nil).Run(context.Background())
	if err == nil {
		t.Fatal("Run returned no error for a failed page")
	}
	// The page that worked is still counted: the work really happened.
	if n != 1 {
		t.Errorf("count = %d, want 1", n)
	}
}

func TestRunSkipsAnEmptySelection(t *testing.T) {
	arch := &fakeArchive{}
	n, err := NewPhase(arch, nil, Set{{Tags: []string{"nothing"}}}).Run(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("Run = %d, %v; want 0, nil", n, err)
	}
	if arch.calls != 0 {
		t.Errorf("Analyze called %d times for an empty selection, want 0", arch.calls)
	}
}

func TestRunReportsAQueryFailure(t *testing.T) {
	arch := &fakeArchive{queryErr: errors.New("archive unreadable")}
	if _, err := NewPhase(arch, nil, nil).Run(context.Background()); err == nil {
		t.Fatal("Run swallowed a query failure")
	}
}

func TestRunReportsCancellationAndKeepsItsCount(t *testing.T) {
	// snorg returns the results so far alongside ctx.Err(), so a cancelled batch
	// still reports the pages it finished.
	arch := &fakeArchive{
		matches:    []snorg.Match{{PageID: "P1"}, {PageID: "P2"}},
		results:    []snorg.AnalyzeResult{result("P1", false, nil)},
		analyzeErr: context.Canceled,
	}
	n, err := NewPhase(arch, nil, nil).Run(context.Background())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n != 1 {
		t.Errorf("count = %d, want 1", n)
	}
}
