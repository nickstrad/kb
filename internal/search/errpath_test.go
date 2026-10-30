package search

import (
	"context"
	"errors"
	"testing"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
)

// faultyRetriever wraps a real store and fails the named call (and counts FTS calls by op), so
// the error paths and the AND→OR retry decision of Search are reachable without a broken database.
type faultyRetriever struct {
	store.Retriever
	failOn  string // "HasVectors", "FTS", "FTS#2", "KNN", "ChunkMeta"
	ftsOps  []store.FTSOp
	ftsRows []store.FTSRow // when non-nil, FTS returns these instead of asking the store
	match   string
}

var errDriver = errs.New(errs.Internal, "sqlite.test", "driver failed")

func (f *faultyRetriever) HasVectors(ctx context.Context) (bool, error) {
	if f.failOn == "HasVectors" {
		return false, errDriver
	}
	return f.Retriever.HasVectors(ctx)
}

func (f *faultyRetriever) FTS(ctx context.Context, q store.FTSQuery) (store.FTSResult, error) {
	f.ftsOps = append(f.ftsOps, q.Op)
	if f.failOn == "FTS" || (f.failOn == "FTS#2" && len(f.ftsOps) == 2) {
		return store.FTSResult{}, errDriver
	}
	if f.ftsRows != nil || f.match != "" {
		return store.FTSResult{Rows: f.ftsRows, Match: f.match}, nil
	}
	return f.Retriever.FTS(ctx, q)
}

func (f *faultyRetriever) KNN(ctx context.Context, q store.KNNQuery) ([]store.VecRow, error) {
	if f.failOn == "KNN" {
		return nil, errDriver
	}
	return f.Retriever.KNN(ctx, q)
}

func (f *faultyRetriever) ChunkMeta(ctx context.Context, ids []int64) (map[int64]store.ChunkMeta, error) {
	if f.failOn == "ChunkMeta" {
		return nil, errDriver
	}
	return f.Retriever.ChunkMeta(ctx, ids)
}

// TestSearchStoreErrors: a store failure at any stage aborts the search with the store's kind
// kept (a driver failure stays Internal, so the CLI reports an incident, never a usage error).
func TestSearchStoreErrors(t *testing.T) {
	for _, tc := range []struct {
		failOn, query, mode string
	}{
		{"HasVectors", "quokka", ModeHybrid},
		{"KNN", "quokka", ModeHybrid},
		{"FTS", "quokka", ModeFTS},
		{"FTS#2", "quokka fluffernutter", ModeFTS}, // the OR retry
		{"ChunkMeta", "quokka", ModeFTS},
	} {
		t.Run(tc.failOn, func(t *testing.T) {
			s, _ := testSearcher(t)
			s.Store = &faultyRetriever{Retriever: s.Store, failOn: tc.failOn}
			out, err := s.Search(context.Background(), Request{Query: tc.query, Mode: tc.mode, K: 8})
			if err == nil || out != nil {
				t.Fatalf("Search = (%v, %v), want the store error", out, err)
			}
			if !errors.Is(err, errDriver) || errs.KindOf(err) != errs.Internal {
				t.Errorf("err = %v (kind %v), want errDriver kept as Internal", err, errs.KindOf(err))
			}
		})
	}
}

// TestFTSListRetryDecision: ftsList retries with OR only when the AND query ran (Match != ""),
// found nothing, and had more than one term.
func TestFTSListRetryDecision(t *testing.T) {
	one := []store.FTSRow{{ChunkID: 1}}
	for _, tc := range []struct {
		name  string
		terms []string
		rows  []store.FTSRow
		match string
		want  []store.FTSOp
	}{
		{"AND empty, two terms: retry with OR", []string{"a", "b"}, []store.FTSRow{}, "m", []store.FTSOp{store.OpAnd, store.OpOr}},
		{"AND empty, one term: no retry", []string{"a"}, []store.FTSRow{}, "m", []store.FTSOp{store.OpAnd}},
		{"nothing ran (all terms sanitised away): no retry", []string{"a", "b"}, []store.FTSRow{}, "", []store.FTSOp{store.OpAnd}},
		{"AND matched: no retry", []string{"a", "b"}, one, "m", []store.FTSOp{store.OpAnd}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &faultyRetriever{ftsRows: tc.rows, match: tc.match}
			s := &Searcher{Store: f}
			if _, err := s.ftsList(context.Background(), tc.terms, ""); err != nil {
				t.Fatal(err)
			}
			if len(f.ftsOps) != len(tc.want) {
				t.Fatalf("FTS ops = %v, want %v", f.ftsOps, tc.want)
			}
			for i := range tc.want {
				if f.ftsOps[i] != tc.want[i] {
					t.Fatalf("FTS ops = %v, want %v", f.ftsOps, tc.want)
				}
			}
		})
	}
}

// TestFusedBeforeTotalOrder pins each level of Fuse's total order directly. Through Fuse the
// vec-rank and chunk-id levels are unreachable (ranks within a list are unique, so equal scores
// with equal fts ranks need equal vec ranks), but the assertion in Fuse relies on the full order.
func TestFusedBeforeTotalOrder(t *testing.T) {
	r := func(n int) *int { return &n }
	for _, tc := range []struct {
		name string
		a, b Candidate
	}{
		{"higher score first", Candidate{ChunkID: 9, RRFScore: 0.2}, Candidate{ChunkID: 1, RRFScore: 0.1, FTSRank: r(1)}},
		{"tie: better fts rank first", Candidate{ChunkID: 9, RRFScore: 0.1, FTSRank: r(1)}, Candidate{ChunkID: 1, RRFScore: 0.1, FTSRank: r(2)}},
		{"tie: an fts rank beats none", Candidate{ChunkID: 9, RRFScore: 0.1, FTSRank: r(5)}, Candidate{ChunkID: 1, RRFScore: 0.1, VecRank: r(1)}},
		{"tie on fts: better vec rank first", Candidate{ChunkID: 9, RRFScore: 0.1, VecRank: r(1)}, Candidate{ChunkID: 1, RRFScore: 0.1, VecRank: r(2)}},
		{"tie on both ranks: lower chunk id first", Candidate{ChunkID: 1, RRFScore: 0.1, VecRank: r(1)}, Candidate{ChunkID: 2, RRFScore: 0.1, VecRank: r(1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !fusedBefore(tc.a, tc.b) {
				t.Errorf("fusedBefore(a, b) = false, want true")
			}
			if fusedBefore(tc.b, tc.a) {
				t.Errorf("fusedBefore(b, a) = true, want false")
			}
		})
	}
	same := Candidate{ChunkID: 3, RRFScore: 0.1, FTSRank: r(1)}
	if fusedBefore(same, same) {
		t.Error("fusedBefore(x, x) = true, want false (strict order)")
	}
}
