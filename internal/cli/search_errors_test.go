package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/search"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/workspace"
)

// entryStub is a store whose Entry answers from a table; every other method is unused.
type entryStub struct {
	store.Store
	rows map[string]*store.EntryRow
	err  error
	n    int
}

func (s *entryStub) Entry(_ context.Context, path string) (*store.EntryRow, error) {
	s.n++
	if s.err != nil {
		return nil, s.err
	}
	return s.rows[path], nil // nil, nil when missing, as the sqlite store does
}

// TestRenderHitsEntryErrors is the decision table for the per-hit entry lookup (Q70): a row fills
// the summary and date, a missing row degrades quietly, any other error is returned, and a path is
// looked up once however many hits it has.
func TestRenderHitsEntryErrors(t *testing.T) {
	hits := []search.Hit{{Rank: 1, Path: "a.md", Text: "x"}, {Rank: 2, Path: "a.md", Text: "y"}, {Rank: 3, Path: "gone.md", Text: "z"}}
	rows := map[string]*store.EntryRow{"a.md": {Summary: "sum", Updated: "2026-01-02"}}
	boom := errs.New(errs.Interrupted, "sqlite.Entry", "interrupted")

	st := &entryStub{rows: rows}
	got, err := renderHits(context.Background(), st, hits)
	if err != nil || len(got) != 3 {
		t.Fatalf("renderHits = %v, %v", got, err)
	}
	if got[0].Summary != "sum" || got[1].Updated != "2026-01-02" || got[2].Summary != "" || got[2].Updated != "" {
		t.Errorf("hits = %+v, want a.md filled and gone.md degraded", got)
	}
	if st.n != 2 {
		t.Errorf("Entry called %d times, want 2 (one per path)", st.n)
	}

	got, err = renderHits(context.Background(), &entryStub{err: boom}, hits)
	if !errors.Is(err, boom) || errs.KindOf(err) != errs.Interrupted || got != nil {
		t.Errorf("renderHits with a store error = %v, %v; want the error and no hits", got, err)
	}
}

// TestOpenSearchUnreadableIndex: a stat failure other than "does not exist" is a Usage error that
// names the index and keeps the cause (Q71); a missing index stays NotFound.
func TestOpenSearchUnreadableIndex(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read anything")
	}
	dir := realTempDir(t)
	sealed := filepath.Join(dir, "sealed")
	if err := os.Mkdir(sealed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sealed, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(sealed, 0o755) })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	for _, tc := range []struct {
		name  string
		db    string
		kind  errs.Kind
		cause error
	}{
		{"permission denied", filepath.Join(sealed, "kb.sqlite"), errs.Usage, syscall.EACCES},
		{"missing", filepath.Join(dir, "none.sqlite"), errs.NotFound, os.ErrNotExist},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := &workspace.Workspace{DBPath: tc.db}
			_, _, _, err := testDeps.openSearch(cmd, ws, config.Default(), "fts", "cli.search", true)
			if errs.KindOf(err) != tc.kind || !errors.Is(err, tc.cause) || !strings.Contains(err.Error(), tc.db) {
				t.Fatalf("err = %v (kind %v), want kind %v wrapping %v and naming %s", err, errs.KindOf(err), tc.kind, tc.cause, tc.db)
			}
			if tc.kind == errs.Usage && strings.Contains(err.Error(), "permission denied") {
				t.Errorf("err = %q leaks the raw os text", err)
			}
		})
	}
}
