package search

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/store/sqlite"
)

// supersedeStore holds old.md and new.md with identical text (so old.md, indexed first, wins the
// tie), plus other.md which also matches the query, a little less well.
func supersedeStore(t *testing.T, edges map[string][]store.EdgeInput) *sqlite.Store {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "kb.sqlite"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.EnsureEmbedMeta(ctx, store.EmbedMeta{Model: store.FTSOnlyModel}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"old.md", "new.md", "other.md"} {
		text := "sprocket calibration steps"
		if p == "other.md" {
			text = "sprocket notes and many unrelated words here to dilute the score of this chunk"
		}
		in := store.EntryInput{Path: p, Kind: "file", Title: p, Edges: edges[p]}
		if _, err := st.ReplaceEntry(ctx, in, []store.ChunkInput{{Ord: 0, SourceFile: p, Text: text, TextHash: p}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.ResolveEdges(ctx); err != nil {
		t.Fatal(err)
	}
	return st
}

func supersedes(dst string) []store.EdgeInput {
	return []store.EdgeInput{{DstPath: dst, Rel: "supersedes", Kind: store.EdgePath}}
}

func paths(out *Outcome) []string {
	var p []string
	for _, c := range out.Candidates {
		p = append(p, c.EntryPath)
	}
	return p
}

// TestSupersedeDemotion: after fusion, a chunk whose entry another entry supersedes scores
// penalty times its RRF score and the list is re-sorted; penalty 1 changes nothing.
func TestSupersedeDemotion(t *testing.T) {
	for _, tc := range []struct {
		name      string
		penalty   float64
		edges     map[string][]store.EdgeInput
		deleteSup bool // delete the superseder after indexing
		want      []string
		oldMarked bool
	}{
		{"no edge, default penalty", 0.5, nil, false, []string{"old.md", "new.md", "other.md"}, false},
		{"alive superseder demotes the target", 0.5, map[string][]store.EdgeInput{"new.md": supersedes("old.md")}, false, []string{"new.md", "other.md", "old.md"}, true},
		{"penalty 1 is a no-op", 1, map[string][]store.EdgeInput{"new.md": supersedes("old.md")}, false, []string{"old.md", "new.md", "other.md"}, false},
		{"dead superseder does not demote", 0.5, map[string][]store.EdgeInput{"new.md": supersedes("old.md")}, true, []string{"old.md", "other.md"}, false},
		{"penalty 0 sorts the target last", 0, map[string][]store.EdgeInput{"new.md": supersedes("old.md")}, false, []string{"new.md", "other.md", "old.md"}, true},
		{"a self-supersede is ignored", 0.5, map[string][]store.EdgeInput{"old.md": supersedes("old.md")}, false, []string{"old.md", "new.md", "other.md"}, false},
		{"a mild penalty marks the entry but keeps the order", 0.99, map[string][]store.EdgeInput{"new.md": supersedes("old.md")}, false, []string{"old.md", "new.md", "other.md"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := supersedeStore(t, tc.edges)
			if tc.deleteSup {
				if err := st.DeleteEntry(ctx, "new.md"); err != nil {
					t.Fatal(err)
				}
			}
			set := settingsWith(t, func(s *config.Search) { s.SupersedePenalty = tc.penalty })
			s := &Searcher{Store: st, Settings: set}
			out, err := s.Search(ctx, Request{Query: "sprocket", Mode: ModeFTS, K: 8})
			if err != nil {
				t.Fatal(err)
			}
			got := paths(out)
			if len(got) != len(tc.want) {
				t.Fatalf("candidates = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("candidates = %v, want %v", got, tc.want)
				}
			}
			for _, h := range out.Result.Hits {
				if h.Superseded != (h.Path == "old.md" && tc.oldMarked) {
					t.Errorf("hit %s Superseded = %v, want %v", h.Path, h.Superseded, h.Path == "old.md" && tc.oldMarked)
				}
			}
			if tc.oldMarked {
				// The score is the plain one times the penalty.
				plain, err := (&Searcher{Store: st, Settings: settingsWith(t, func(s *config.Search) { s.SupersedePenalty = 1 })}).Search(ctx, Request{Query: "sprocket", Mode: ModeFTS, K: 8})
				if err != nil {
					t.Fatal(err)
				}
				var base, demoted float64
				for _, c := range plain.Candidates {
					if c.EntryPath == "old.md" {
						base = c.RRFScore
					}
				}
				for _, c := range out.Candidates {
					if c.EntryPath == "old.md" {
						demoted = c.RRFScore
					}
				}
				if demoted != base*tc.penalty {
					t.Errorf("old.md score = %v, want %v * %v", demoted, base, tc.penalty)
				}
			}
		})
	}
}

// TestExplainShowsSupersede: kb explain reports the penalty on the superseded candidate only.
func TestExplainShowsSupersede(t *testing.T) {
	st := supersedeStore(t, map[string][]store.EdgeInput{"new.md": supersedes("old.md")})
	s := &Searcher{Store: st, Settings: settingsWith(t, func(c *config.Search) { c.SupersedePenalty = 0.25 })}
	ex, err := s.Explain(context.Background(), Request{Query: "sprocket", Mode: ModeFTS, K: 8})
	if err != nil {
		t.Fatal(err)
	}
	if ex.Settings.SupersedePenalty != 0.25 {
		t.Errorf("settings.supersede_penalty = %v, want 0.25", ex.Settings.SupersedePenalty)
	}
	n := 0
	for _, c := range ex.Candidates {
		if (c.Path == "old.md") != c.Superseded || (c.Superseded && c.SupersedePenalty != 0.25) {
			t.Errorf("candidate %s: superseded = %v, penalty = %v", c.Path, c.Superseded, c.SupersedePenalty)
		}
		if c.Superseded {
			n++
			var sum float64
			for _, term := range c.RRF {
				sum += term.Term
			}
			if c.RRFScore != sum*0.25 {
				t.Errorf("score %v, want terms %v * 0.25", c.RRFScore, sum)
			}
		}
	}
	if n != 1 {
		t.Errorf("%d superseded candidates, want 1", n)
	}
}

// TestParamsValidSupersede: a penalty outside 0..1 is not a valid Params.
func TestParamsValidSupersede(t *testing.T) {
	for _, tc := range []struct {
		v    float64
		want bool
	}{{0, true}, {0.5, true}, {1, true}, {-0.1, false}, {1.1, false}} {
		p := DefaultParams()
		p.SupersedePenalty = tc.v
		if p.valid() != tc.want {
			t.Errorf("penalty %v: valid = %v, want %v", tc.v, p.valid(), tc.want)
		}
	}
}

// askedRetriever records the paths Superseded was asked about.
type askedRetriever struct {
	store.Retriever
	asked [][]string
}

func (a *askedRetriever) Superseded(ctx context.Context, paths []string) (map[string]bool, error) {
	a.asked = append(a.asked, paths)
	return a.Retriever.Superseded(ctx, paths)
}

// TestSupersedeAsksOncePerEntry: one read for the candidates' entries, each path once and never
// empty (an entry with several chunks is asked about once); penalty 1 reads nothing.
func TestSupersedeAsksOncePerEntry(t *testing.T) {
	st := supersedeStore(t, nil)
	two := []store.ChunkInput{{Ord: 0, SourceFile: "multi.md", Text: "sprocket one", TextHash: "m0"}, {Ord: 1, SourceFile: "multi.md", Text: "sprocket two", TextHash: "m1"}}
	if _, err := st.ReplaceEntry(context.Background(), store.EntryInput{Path: "multi.md", Kind: "file", Title: "multi"}, two); err != nil {
		t.Fatal(err)
	}
	rec := &askedRetriever{Retriever: st}
	s := &Searcher{Store: rec, Settings: settingsWith(t, func(c *config.Search) { c.SupersedePenalty = 0.5 })}
	if _, err := s.Search(context.Background(), Request{Query: "sprocket", Mode: ModeFTS, K: 8}); err != nil {
		t.Fatal(err)
	}
	if len(rec.asked) != 1 || len(rec.asked[0]) != 4 {
		t.Fatalf("asked = %v, want one read of the 4 entries, multi.md once", rec.asked)
	}
	rec.asked = nil
	s.Settings = settingsWith(t, func(c *config.Search) { c.SupersedePenalty = 1 })
	if _, err := s.Search(context.Background(), Request{Query: "sprocket", Mode: ModeFTS, K: 8}); err != nil {
		t.Fatal(err)
	}
	if len(rec.asked) != 0 {
		t.Errorf("penalty 1 asked %v, want no read", rec.asked)
	}
}
