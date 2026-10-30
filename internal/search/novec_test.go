package search

import (
	"context"
	"database/sql"
	"errors"
	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/errs"
	"testing"

	"github.com/nickstrad/kb/internal/embed/fake"
)

// countingEmbedder counts Embed calls, so a test can prove a search never paid for an embedding.
type countingEmbedder struct {
	*fake.Embedder
	calls *int
}

func (e countingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	*e.calls++
	return e.Embedder.Embed(ctx, texts)
}

// dropVectors puts a fixture database into the state schema migration 2→3 leaves behind: no
// chunks_vec table and no embed_meta row, with entries, chunks and FTS intact.
func dropVectors(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec("DROP TABLE chunks_vec; DELETE FROM embed_meta;"); err != nil {
		t.Fatalf("drop vectors: %v", err)
	}
}

// TestSearchNoVectorTableDecision is the TESTING.md §4.3 table for the no-chunks_vec branch of
// Search: hybrid degrades to fts-fallback with ErrNoVectors (the CLI's warning carries its
// `kb reindex --all` hint) without embedding the query; vec has nothing to fall back to and
// returns ErrNoVectors; fts never looks at vectors. The "table present" rows pin the other
// outcome of the same branch.
func TestSearchNoVectorTableDecision(t *testing.T) {
	for _, tc := range []struct {
		name      string
		dropVec   bool
		mode      string
		wantMode  string // "" = Search returns an error
		wantEmbed int    // Embed calls expected
	}{
		{"no table, hybrid: fallback, no embed", true, ModeHybrid, ModeFTSFallback, 0},
		{"no table, vec: error, no embed", true, ModeVec, "", 0},
		{"no table, fts: plain fts", true, ModeFTS, ModeFTS, 0},
		{"table present, hybrid: hybrid", false, ModeHybrid, ModeHybrid, 1},
		{"table present, vec: vec", false, ModeVec, ModeVec, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := testSearcher(t)
			if tc.dropVec {
				dropVectors(t, dbOf(s))
			}
			calls := 0
			s.Embedder = countingEmbedder{fake.New("fake-embed", 768), &calls}

			out, err := s.Search(context.Background(), Request{Query: "quokka widget", Mode: tc.mode, K: 8})
			if calls != tc.wantEmbed {
				t.Errorf("Embed calls = %d, want %d", calls, tc.wantEmbed)
			}
			if tc.wantMode == "" {
				if !errors.Is(err, ErrNoVectors) {
					t.Fatalf("err = %v, want one wrapping ErrNoVectors", err)
				}
				if errs.KindOf(err) != errs.Conflict || errors.Is(err, embed.ErrUnavailable) {
					t.Errorf("err %v (kind %v) should be a Conflict, not unavailable: the embedder is fine, the index lacks vectors", err, errs.KindOf(err))
				}
				return
			}
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if out.Mode != tc.wantMode || out.Result.Mode != tc.wantMode {
				t.Fatalf("mode = %s / result mode %s, want %s", out.Mode, out.Result.Mode, tc.wantMode)
			}
			if tc.wantMode == ModeFTSFallback {
				if !errors.Is(out.FallbackErr, ErrNoVectors) {
					t.Errorf("fallback err = %v, want ErrNoVectors", out.FallbackErr)
				}
				if out.NVec != 0 || out.EmbedModel != "" || out.EmbedMs != 0 {
					t.Errorf("n_vec, embed_model, embed_ms = %d, %q, %d; want 0, \"\", 0", out.NVec, out.EmbedModel, out.EmbedMs)
				}
			} else if out.FallbackErr != nil {
				t.Errorf("fallback err = %v, want nil", out.FallbackErr)
			}
			if tc.mode != ModeVec && out.NFTS == 0 {
				t.Error("n_fts = 0; the FTS list should have run")
			}
			if len(out.Result.Hits) == 0 {
				t.Error("no hits")
			}
		})
	}
}

// TestSearchAndLogNoVectorTable: the fallback is logged like any other fts-fallback search, with
// no embed model, so the log stays honest about what ran before the reindex.
func TestSearchAndLogNoVectorTable(t *testing.T) {
	s, _ := testSearcher(t)
	dropVectors(t, dbOf(s))

	out, err := s.SearchAndLog(context.Background(), Request{Query: "quokka widget", Mode: ModeHybrid, K: 8, Caller: "test"})
	if err != nil {
		t.Fatalf("SearchAndLog: %v", err)
	}
	var mode string
	var embedModel sql.NullString
	var nVec int
	if err := dbOf(s).QueryRow("SELECT mode, embed_model, n_vec FROM searches WHERE id = ?", out.Result.SearchID).
		Scan(&mode, &embedModel, &nVec); err != nil {
		t.Fatalf("read logged search: %v", err)
	}
	if mode != ModeFTSFallback || embedModel.Valid || nVec != 0 {
		t.Errorf("logged mode, embed_model, n_vec = %s, %v, %d; want %s, NULL, 0", mode, embedModel, nVec, ModeFTSFallback)
	}
}
