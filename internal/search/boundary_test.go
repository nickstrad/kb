package search

import (
	"context"
	"fmt"
	"github.com/nickstrad/kb/internal/store/sqlite"
	"path/filepath"
	"testing"

	"github.com/nickstrad/kb/internal/store"
)

// TestFuseCandidateLimitBoundary: the fused list is cut at CandidateLimit, not one below or above.
func TestFuseCandidateLimitBoundary(t *testing.T) {
	for _, tc := range []struct {
		name  string
		total int // distinct chunks across both lists
		want  int
	}{
		{"CandidateLimit-1", CandidateLimit - 1, CandidateLimit - 1},
		{"at CandidateLimit", CandidateLimit, CandidateLimit},
		{"CandidateLimit+1", CandidateLimit + 1, CandidateLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fts []FTSRow
			var vec []VecRow
			for i := 0; i < tc.total; i++ {
				if i%2 == 0 {
					fts = append(fts, FTSRow{ChunkID: int64(i + 1)})
				} else {
					vec = append(vec, VecRow{ChunkID: int64(i + 1)})
				}
			}
			if got := Fuse(fts, vec, DefaultParams()); len(got) != tc.want {
				t.Fatalf("len(Fuse) = %d, want %d", len(got), tc.want)
			}
		})
	}
}

// TestApplyCapBoundaries: PerEntryCap one below, at and above for one entry; k at MinK, MaxK and
// at/above the number of cappable candidates.
func TestApplyCapBoundaries(t *testing.T) {
	cands := func(n int) []Candidate {
		out := make([]Candidate, n)
		for i := range out {
			out[i] = Candidate{ChunkID: int64(i + 1)}
		}
		return out
	}
	oneEntry := func(n int) map[int64]string {
		m := make(map[int64]string, n)
		for i := 1; i <= n; i++ {
			m[int64(i)] = "a.md"
		}
		return m
	}
	distinct := func(n int) map[int64]string {
		m := make(map[int64]string, n)
		for i := 1; i <= n; i++ {
			m[int64(i)] = fmt.Sprintf("e%d.md", i)
		}
		return m
	}
	for _, tc := range []struct {
		name    string
		n       int
		entryOf map[int64]string
		k       int
		want    int
	}{
		{"one entry, PerEntryCap-1 chunks", PerEntryCap - 1, oneEntry(PerEntryCap - 1), MaxK, PerEntryCap - 1},
		{"one entry, at PerEntryCap", PerEntryCap, oneEntry(PerEntryCap), MaxK, PerEntryCap},
		{"one entry, PerEntryCap+1 chunks", PerEntryCap + 1, oneEntry(PerEntryCap + 1), MaxK, PerEntryCap},
		{"k = MinK", 5, distinct(5), MinK, MinK},
		{"k = MaxK, MaxK+1 cappable", MaxK + 1, distinct(MaxK + 1), MaxK, MaxK},
		{"k = MaxK, exactly MaxK cappable", MaxK, distinct(MaxK), MaxK, MaxK},
		{"k above what survives the cap", 4, distinct(4), MaxK, 4},
		{"empty candidates", 0, distinct(0), MaxK, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := applyCap(cands(tc.n), tc.entryOf, tc.k, PerEntryCap)
			if len(got) != tc.want {
				t.Fatalf("len(applyCap) = %d, want %d", len(got), tc.want)
			}
			for i, c := range got { // order preserved: a prefix of the input here
				if c.ChunkID != int64(i+1) {
					t.Fatalf("out[%d] = chunk %d, want %d", i, c.ChunkID, i+1)
				}
			}
		})
	}
}

// rankedEmbedder gives "chunk N ..." documents a vector whose distance to every query vector grows
// with N, so a test controls each chunk's vec rank exactly: rank N for chunk N.
type rankedEmbedder struct{}

const rankedDim = 768 // the vec0 column width in schema.sql

func (rankedEmbedder) Model() string { return "ranked" }
func (rankedEmbedder) Dim() int      { return rankedDim }
func (rankedEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, rankedDim)
		v[0] = 1
		var n int
		if _, err := fmt.Sscanf(t, "chunk %d", &n); err == nil {
			v[1] = float32(n) / 1000
		}
		out[i] = v
	}
	return out, nil
}

// rankedSearcher indexes chunks "chunk 1 widget" .. "chunk n widget" (vec rank = N), all in
// untagged.md except chunk tagged, which goes alone into tagged.md with tag "t" (0 = none).
func rankedSearcher(t *testing.T, n, tagged int) *Searcher {
	t.Helper()
	st, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "kb.sqlite"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	emb := rankedEmbedder{}
	if err := st.EnsureEmbedMeta(context.Background(), store.EmbedMeta{Model: emb.Model(), Dim: emb.Dim()}); err != nil {
		t.Fatal(err)
	}
	byPath := map[string][]store.ChunkInput{}
	for i := 1; i <= n; i++ {
		path := "untagged.md"
		if i == tagged {
			path = "tagged.md"
		}
		text := fmt.Sprintf("chunk %d widget", i)
		vecs, _ := emb.Embed(context.Background(), []string{text})
		byPath[path] = append(byPath[path], store.ChunkInput{
			Ord: len(byPath[path]), SourceFile: path, Text: text, TextHash: text, Embedding: vecs[0],
		})
	}
	for path, chunks := range byPath {
		var tags []string
		if path == "tagged.md" {
			tags = []string{"t"}
		}
		if _, err := st.ReplaceEntry(context.Background(), store.EntryInput{
			Path: path, Kind: "file", Title: path, Summary: "s", Tags: tags, BodyHash: path,
		}, chunks); err != nil {
			t.Fatal(err)
		}
	}
	return &Searcher{Store: st, Log: st, Embedder: emb}
}

// TestListLimitBoundary: each list holds at most ListLimit rows, for both lists.
func TestListLimitBoundary(t *testing.T) {
	for _, n := range []int{ListLimit - 1, ListLimit, ListLimit + 1} {
		t.Run(fmt.Sprintf("%d matching chunks", n), func(t *testing.T) {
			s := rankedSearcher(t, n, 0)
			out, err := s.Search(context.Background(), Request{Query: "widget", Mode: ModeHybrid, K: MaxK})
			if err != nil {
				t.Fatal(err)
			}
			want := min(n, ListLimit)
			if out.NFTS != want || out.NVec != want {
				t.Fatalf("n_fts=%d n_vec=%d, want %d each", out.NFTS, out.NVec, want)
			}
		})
	}
}

// TestVecOverFetchBoundary: with a tag filter the KNN fetches vecOverFetch rows before filtering,
// so a tagged chunk at vec rank vecOverFetch is found and one at vecOverFetch+1 is not. Without a
// tag, the same chunk past ListLimit is out of the list either way.
func TestVecOverFetchBoundary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rank     int
		wantNVec int
	}{
		{"tagged at ListLimit+1: found thanks to over-fetch", ListLimit + 1, 1},
		{"tagged at vecOverFetch-1", vecOverFetch - 1, 1},
		{"tagged at vecOverFetch", vecOverFetch, 1},
		{"tagged at vecOverFetch+1: beyond the over-fetch", vecOverFetch + 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rankedSearcher(t, vecOverFetch+5, tc.rank)
			out, err := s.Search(context.Background(), Request{Query: "q", Mode: ModeVec, K: MaxK, Tag: "t"})
			if err != nil {
				t.Fatal(err)
			}
			if out.NVec != tc.wantNVec {
				t.Fatalf("n_vec = %d, want %d", out.NVec, tc.wantNVec)
			}
		})
	}
}
