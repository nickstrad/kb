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

// entryStub is an entry lookup that answers from a table and counts its calls.
type entryStub struct {
	rows  map[string]store.EntryRow
	err   error
	n     int
	asked [][]string
}

func (s *entryStub) EntriesByPath(_ context.Context, paths []string) (map[string]store.EntryRow, error) {
	s.n++
	s.asked = append(s.asked, paths)
	if s.err != nil {
		return nil, s.err
	}
	out := map[string]store.EntryRow{}
	for _, p := range paths {
		if r, ok := s.rows[p]; ok {
			out[p] = r
		}
	}
	return out, nil
}

// TestRenderHitsEntryErrors is the decision table for the entry lookup (Q70, PLAN 3.2): a row
// fills the summary and date, a missing row degrades quietly, any other error is returned, and each
// store is asked once for all its hits, each hit from its own store (by its Store tag); a tag with
// no lookup degrades like a missing row.
func TestRenderHitsEntryErrors(t *testing.T) {
	t.Parallel()
	hits := []search.Hit{{Rank: 1, Path: "a.md", Text: "x"}, {Rank: 2, Path: "a.md", Text: "y"}, {Rank: 3, Path: "gone.md", Text: "z"},
		{Rank: 4, Store: "notes", Path: "a.md"}, {Rank: 5, Store: "lost", Path: "a.md"}}
	boom := errs.New(errs.Interrupted, "sqlite.Entry", "interrupted")

	prim := &entryStub{rows: map[string]store.EntryRow{"a.md": {Summary: "sum", Updated: "2026-01-02"}}}
	notes := &entryStub{rows: map[string]store.EntryRow{"a.md": {Summary: "notes sum"}}}
	got, err := renderHits(context.Background(), map[string]entryLookup{"": prim, "notes": notes}, hits)
	if err != nil || len(got) != 5 {
		t.Fatalf("renderHits = %v, %v", got, err)
	}
	if got[0].Summary != "sum" || got[1].Updated != "2026-01-02" || got[2].Summary != "" || got[2].Updated != "" {
		t.Errorf("hits = %+v, want a.md filled and gone.md degraded", got)
	}
	if got[3].Summary != "notes sum" || got[3].Store != "notes" || got[4].Summary != "" {
		t.Errorf("cross-store hits = %+v, %+v; want notes' own row, and lost degraded", got[3], got[4])
	}
	if prim.n != 1 || notes.n != 1 {
		t.Errorf("EntriesByPath called %d and %d times, want once per store", prim.n, notes.n)
	}

	got, err = renderHits(context.Background(), map[string]entryLookup{"": &entryStub{err: boom}}, hits)
	if !errors.Is(err, boom) || errs.KindOf(err) != errs.Interrupted || got != nil {
		t.Errorf("renderHits with a store error = %v, %v; want the error and no hits", got, err)
	}
}

// TestOpenSearchUnreadableIndex: a stat failure other than "does not exist" is a Usage error that
// names the index and keeps the cause (Q71); a missing index stays NotFound.
func TestOpenSearchUnreadableIndex(t *testing.T) {
	t.Parallel()
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
			_, _, _, err := depsFor(t).openSearch(cmd, ws, config.Default(), "fts", "cli.search", true)
			if errs.KindOf(err) != tc.kind || !errors.Is(err, tc.cause) || !strings.Contains(err.Error(), tc.db) {
				t.Fatalf("err = %v (kind %v), want kind %v wrapping %v and naming %s", err, errs.KindOf(err), tc.kind, tc.cause, tc.db)
			}
			if tc.kind == errs.Usage && strings.Contains(err.Error(), "permission denied") {
				t.Errorf("err = %q leaks the raw os text", err)
			}
		})
	}
}
