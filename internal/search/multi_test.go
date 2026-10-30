package search

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/fake"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/store/sqlite"
)

// --- Merge, pure: synthetic per-store outcomes ---------------------------------------------------

// cand is a fused-list candidate of one store: path#ord with that store's chunk id.
func cand(id int64, path string, ord int) Candidate {
	return Candidate{ChunkID: id, EntryPath: path, Ord: ord, Heading: path + "#" + fmt.Sprint(ord)}
}

// outcomeOf is a store's outcome whose fused list is cands, in order, with the default params.
func outcomeOf(k int, cands ...Candidate) *Outcome {
	s, _ := DefaultSettings()
	return &Outcome{Mode: ModeHybrid, K: k, Settings: s, Candidates: cands, Result: NewResult("q", ModeHybrid)}
}

// shown renders the returned hits as "label:path#ord" (label "" for the primary).
func shown(o *Outcome) []string {
	out := make([]string, len(o.Result.Hits))
	for i, h := range o.Result.Hits {
		out[i] = h.Store + ":" + h.Heading
	}
	return out
}

func rrf(ranks ...int) float64 {
	s := 0.0
	for _, r := range ranks {
		s += 1 / float64(RRFConstant+r)
	}
	return s
}

// TestMergeDecisions is the decision table for identity, ensemble, tagging and tie-break.
func TestMergeDecisions(t *testing.T) {
	primary := Part{Ref: "", Root: "/p"}
	sameLoc := Part{Ref: "adr@/p", Label: "adr", Root: "/p"}
	otherLoc := Part{Ref: "default@/q", Label: "notes", Root: "/q"}
	another := Part{Ref: "b@/r", Label: "b", Root: "/r"}
	tests := []struct {
		name   string
		parts  []Part
		outs   []*Outcome
		want   []string  // returned hits, label:path#ord
		scores []float64 // their merged scores
	}{
		{
			name:   "one store: its own order, rank-only scores",
			parts:  []Part{primary},
			outs:   []*Outcome{outcomeOf(8, cand(1, "a.md", 0), cand(2, "b.md", 0))},
			want:   []string{":a.md#0", ":b.md#0"},
			scores: []float64{rrf(1), rrf(2)},
		},
		{
			name:   "same path in different locations: two hits, never merged, each tagged",
			parts:  []Part{primary, otherLoc},
			outs:   []*Outcome{outcomeOf(8, cand(1, "x.md", 0)), outcomeOf(8, cand(1, "x.md", 0))},
			want:   []string{":x.md#0", "notes:x.md#0"},
			scores: []float64{rrf(1), rrf(1)},
		},
		{
			name:   "same file in two stores of one location: RRF terms add up, the primary keeps a tie",
			parts:  []Part{primary, sameLoc},
			outs:   []*Outcome{outcomeOf(8, cand(1, "x.md", 0), cand(2, "y.md", 0)), outcomeOf(8, cand(9, "x.md", 0))},
			want:   []string{":x.md#0", ":y.md#0"},
			scores: []float64{rrf(1, 1), rrf(2)},
		},
		{
			name:   "ensemble hit is tagged with the store that ranked it best",
			parts:  []Part{primary, sameLoc},
			outs:   []*Outcome{outcomeOf(8, cand(1, "y.md", 0), cand(2, "x.md", 0)), outcomeOf(8, cand(9, "x.md", 0))},
			want:   []string{"adr:x.md#0", ":y.md#0"},
			scores: []float64{rrf(2, 1), rrf(1)},
		},
		{
			name:   "same path, different ord in one location: two identities",
			parts:  []Part{primary, sameLoc},
			outs:   []*Outcome{outcomeOf(8, cand(1, "x.md", 0)), outcomeOf(8, cand(9, "x.md", 1))},
			want:   []string{":x.md#0", "adr:x.md#1"},
			scores: []float64{rrf(1), rrf(1)},
		},
		{
			name:   "ties: primary first, then canonical ref, whatever order the stores were named in",
			parts:  []Part{primary, otherLoc, another},
			outs:   []*Outcome{outcomeOf(8, cand(1, "p.md", 0)), outcomeOf(8, cand(1, "q.md", 0)), outcomeOf(8, cand(1, "r.md", 0))},
			want:   []string{":p.md#0", "b:r.md#0", "notes:q.md#0"},
			scores: []float64{rrf(1), rrf(1), rrf(1)},
		},
		{
			name:   "a failed extra store (nil outcome) takes no part",
			parts:  []Part{primary, otherLoc},
			outs:   []*Outcome{outcomeOf(8, cand(1, "a.md", 0)), nil},
			want:   []string{":a.md#0"},
			scores: []float64{rrf(1)},
		},
		{
			name:   "a candidate with no entry row is skipped",
			parts:  []Part{primary, otherLoc},
			outs:   []*Outcome{outcomeOf(8, Candidate{ChunkID: 5}), outcomeOf(8, cand(1, "a.md", 0))},
			want:   []string{"notes:a.md#0"},
			scores: []float64{rrf(1)},
		},
		{
			name:  "cap per (location, path): same path in two locations is capped separately",
			parts: []Part{primary, otherLoc},
			outs: []*Outcome{
				outcomeOf(8, cand(1, "x.md", 0), cand(2, "x.md", 1), cand(3, "x.md", 2), cand(4, "x.md", 3)),
				outcomeOf(8, cand(1, "x.md", 0), cand(2, "x.md", 1), cand(3, "x.md", 2), cand(4, "x.md", 3)),
			},
			want:   []string{":x.md#0", "notes:x.md#0", ":x.md#1", "notes:x.md#1", ":x.md#2", "notes:x.md#2"},
			scores: []float64{rrf(1), rrf(1), rrf(2), rrf(2), rrf(3), rrf(3)},
		},
		{
			name:  "cap per (location, path): two stores of one location share the cap",
			parts: []Part{primary, sameLoc},
			outs: []*Outcome{
				outcomeOf(8, cand(1, "x.md", 0), cand(2, "x.md", 1)),
				outcomeOf(8, cand(1, "x.md", 2), cand(2, "x.md", 3)),
			},
			want:   []string{":x.md#0", "adr:x.md#2", ":x.md#1"},
			scores: []float64{rrf(1), rrf(1), rrf(2)},
		},
		{
			name:   "k comes from the primary's outcome",
			parts:  []Part{primary, otherLoc},
			outs:   []*Outcome{outcomeOf(1, cand(1, "a.md", 0)), outcomeOf(1, cand(1, "b.md", 0))},
			want:   []string{":a.md#0"},
			scores: []float64{rrf(1)},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Merge(tc.parts, tc.outs)
			if !slices.Equal(shown(got), tc.want) {
				t.Fatalf("hits = %v, want %v", shown(got), tc.want)
			}
			for i, h := range got.Result.Hits {
				if !almostEqual(h.RRFScore, tc.scores[i]) || h.Rank != i+1 {
					t.Errorf("hit %d: rank %d score %v, want rank %d score %v", i, h.Rank, h.RRFScore, i+1, tc.scores[i])
				}
				if got.Returned[i].RRFScore != h.RRFScore {
					t.Errorf("Returned[%d] score %v != hit score %v", i, got.Returned[i].RRFScore, h.RRFScore)
				}
			}
		})
	}
}

// TestMergeLoggedStore: merged candidates carry the canonical ref (what the log records), hits
// carry the label (what the user typed); the primary's are "" in both.
func TestMergeLoggedStore(t *testing.T) {
	parts := []Part{{Root: "/p"}, {Ref: "default@/q", Label: "notes", Root: "/q"}}
	got := Merge(parts, []*Outcome{outcomeOf(8, cand(1, "a.md", 0)), outcomeOf(8, cand(7, "b.md", 0))})
	if len(got.Candidates) != 2 || got.Candidates[0].Store != "" || got.Candidates[1].Store != "default@/q" || got.Candidates[1].ChunkID != 7 {
		t.Errorf("candidates = %+v", got.Candidates)
	}
	logged := loggedHits(got.Returned)
	if logged[0].Store != "" || logged[1].Store != "default@/q" {
		t.Errorf("logged stores = %q, %q", logged[0].Store, logged[1].Store)
	}
	if got.Result.Hits[1].Store != "notes" {
		t.Errorf("hit label = %q, want notes", got.Result.Hits[1].Store)
	}
}

// TestMergeCandidateLimit: the merged list is cut to the primary's candidate_limit before the cap.
func TestMergeCandidateLimit(t *testing.T) {
	prim := outcomeOf(8)
	prim.Settings.Params.CandidateLimit = 3
	var a, b []Candidate
	for i := range 5 {
		a = append(a, cand(int64(i+1), fmt.Sprintf("a%d.md", i), 0))
		b = append(b, cand(int64(i+1), fmt.Sprintf("b%d.md", i), 0))
	}
	prim.Candidates = a
	got := Merge([]Part{{Root: "/p"}, {Ref: "x@/q", Label: "x", Root: "/q"}}, []*Outcome{prim, outcomeOf(8, b...)})
	if len(got.Candidates) != 3 || len(got.Returned) != 3 {
		t.Errorf("candidates %d, returned %d; want 3 and 3", len(got.Candidates), len(got.Returned))
	}
}

// randomOutcomes builds n stores' fused lists over a small path space, so identities collide.
func randomOutcomes(r *rand.Rand, n int) ([]Part, []*Outcome) {
	roots := []string{"/p", "/q"}
	parts := []Part{{Root: "/p"}}
	for i := 1; i < n; i++ {
		root := roots[r.Intn(len(roots))]
		ref := fmt.Sprintf("s%d@%s", i, root)
		parts = append(parts, Part{Ref: ref, Label: fmt.Sprintf("s%d", i), Root: root})
	}
	outs := make([]*Outcome, n)
	for i := range outs {
		seen := map[string]bool{}
		var cs []Candidate
		for j := range r.Intn(12) {
			path, ord := fmt.Sprintf("e%d.md", r.Intn(5)), r.Intn(3)
			key := fmt.Sprint(path, ord)
			if seen[key] {
				continue
			}
			seen[key] = true
			cs = append(cs, cand(int64(j+1), path, ord))
		}
		outs[i] = outcomeOf(1+r.Intn(MaxK), cs...)
		if i > 0 {
			outs[i].K = outs[0].K
		}
	}
	return parts, outs
}

// TestMergeStoreOrderProperty (TESTING.md §5.2, metamorphic): permuting the extra stores changes
// neither the returned hits, their order nor their scores, because the tie-break ends with the
// canonical ref, path and ord. It also checks the merged list's postconditions.
func TestMergeStoreOrderProperty(t *testing.T) {
	r := rand.New(rand.NewSource(32))
	for range 300 {
		parts, outs := randomOutcomes(r, 2+r.Intn(4))
		checkStoreOrderIndependent(t, r, parts, outs)
	}
}

func checkStoreOrderIndependent(t *testing.T, r *rand.Rand, parts []Part, outs []*Outcome) {
	t.Helper()
	base := Merge(parts, outs)
	perm := r.Perm(len(parts) - 1)
	p2, o2 := []Part{parts[0]}, []*Outcome{outs[0]}
	for _, i := range perm {
		p2, o2 = append(p2, parts[i+1]), append(o2, outs[i+1])
	}
	got := Merge(p2, o2)
	if fmt.Sprint(shown(got)) != fmt.Sprint(shown(base)) {
		t.Fatalf("store order changed the hits:\n%v\n%v", shown(base), shown(got))
	}
	for i := range got.Candidates {
		a, b := base.Candidates[i], got.Candidates[i]
		if a.Store != b.Store || a.ChunkID != b.ChunkID || a.RRFScore != b.RRFScore {
			t.Fatalf("candidate %d differs: %+v vs %+v", i, a, b)
		}
	}
	// Each store's own order is kept among the candidates only it returned.
	for i := range got.Candidates {
		if i > 0 && got.Candidates[i-1].RRFScore < got.Candidates[i].RRFScore {
			t.Fatalf("candidates not in score order at %d", i)
		}
	}
}

// FuzzMerge drives the store-order property and Merge's assertions with fuzzed store lists.
func FuzzMerge(f *testing.F) {
	for _, s := range []int64{0, 1, 32, 1 << 40} {
		f.Add(s, uint8(3))
	}
	f.Fuzz(func(t *testing.T, seed int64, n uint8) {
		r := rand.New(rand.NewSource(seed))
		parts, outs := randomOutcomes(r, 1+int(n%(MaxExtraStores+1)))
		checkStoreOrderIndependent(t, r, parts, outs)
	})
}

// --- SearchStores: real stores, fake embedders ----------------------------------------------------

// queryCounter counts Embed calls (query embeddings, here).
type queryCounter struct {
	embed.Embedder
	calls *atomic.Int64
}

func (e queryCounter) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	e.calls.Add(1)
	return e.Embedder.Embed(ctx, texts)
}

// storeWith indexes entries (path -> chunk texts) into a new store with emb (nil: FTS only).
func storeWith(t *testing.T, emb embed.Embedder, entries map[string][]string) *sqlite.Store {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "kb.sqlite"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	meta := store.EmbedMeta{Model: store.FTSOnlyModel}
	if emb != nil {
		meta = store.EmbedMeta{Model: emb.Model(), Dim: emb.Dim()}
	}
	if err := st.EnsureEmbedMeta(ctx, meta); err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(entries))
	for p := range entries {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, p := range paths {
		texts := entries[p]
		var vecs [][]float32
		if emb != nil {
			if vecs, err = emb.Embed(ctx, texts); err != nil {
				t.Fatal(err)
			}
		}
		cs := make([]store.ChunkInput, len(texts))
		for j, text := range texts {
			sum := sha256.Sum256([]byte(text))
			cs[j] = store.ChunkInput{Ord: j, SourceFile: p, Text: text, TextHash: hex.EncodeToString(sum[:])}
			if vecs != nil {
				cs[j].Embedding = vecs[j]
			}
		}
		if _, err := st.ReplaceEntry(ctx, store.EntryInput{Path: p, Kind: "file", Title: p, BodyHash: "h" + p}, cs); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

// TestSearchStoresDecisions is PLAN 3.2's table: {same location, different location} × {same
// model, different model, none}. It checks how often the query is embedded, which stores' hits
// merge, and which store each hit is tagged with.
func TestSearchStoresDecisions(t *testing.T) {
	corpus := map[string][]string{"data/x.md": {"zebracorn grooming notes"}, "data/y.md": {"unrelated widget text"}}
	tests := []struct {
		name       string
		sameLoc    bool
		model      string // the extra store's model: "same", "other" or "none"
		wantEmbeds int64
		wantHits   []string
	}{
		{"same location, same model", true, "same", 1, []string{":data/x.md#0"}},
		{"same location, different model", true, "other", 2, []string{":data/x.md#0"}},
		{"same location, none", true, "none", 1, []string{":data/x.md#0"}},
		{"different location, same model", false, "same", 1, []string{":data/x.md#0", "extra:data/x.md#0"}},
		{"different location, different model", false, "other", 2, []string{":data/x.md#0", "extra:data/x.md#0"}},
		{"different location, none", false, "none", 1, []string{":data/x.md#0", "extra:data/x.md#0"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			primEmb := queryCounter{fake.New("fake-a", 16), &calls}
			var extraEmb embed.Embedder
			mode := ""
			switch tc.model {
			case "same":
				extraEmb = queryCounter{fake.New("fake-a", 16), &calls}
			case "other":
				extraEmb = queryCounter{fake.New("fake-b", 16), &calls}
			case "none":
				mode = ModeFTS
			}
			settings, _ := DefaultSettings()
			memo := &QueryMemo{}
			prim := &Searcher{Store: storeWith(t, primEmb, corpus), Embedder: primEmb, Settings: settings, Queries: memo}
			extra := &Searcher{Store: storeWith(t, extraEmb, corpus), Embedder: extraEmb, Settings: settings, Queries: memo}
			root := "/q"
			if tc.sameLoc {
				root = "/p"
			}
			parts := []Part{{Root: "/p", Searcher: prim}, {Ref: "extra@" + root, Label: "extra", Root: root, Mode: mode, Searcher: extra}}
			calls.Store(0) // indexing embedded the documents; count only the query
			mo, err := SearchStores(context.Background(), parts, Request{Query: "zebracorn", Mode: ModeHybrid, K: 2})
			if err != nil {
				t.Fatal(err)
			}
			// Each store's hit list is cut by the cap and k=2: x.md first, then y.md (vector only).
			var got []string
			for _, h := range mo.Result.Hits {
				if h.Path == "data/x.md" {
					got = append(got, h.Store+":"+h.Path+"#0")
				}
			}
			if !slices.Equal(got, tc.wantHits) {
				t.Errorf("x.md hits = %v, want %v (all: %+v)", got, tc.wantHits, mo.Result.Hits)
			}
			if n := calls.Load(); n != tc.wantEmbeds {
				t.Errorf("query embedded %d times, want %d", n, tc.wantEmbeds)
			}
			if len(mo.Skipped) != 0 || len(mo.Degraded) != 0 || mo.Mode != ModeHybrid {
				t.Errorf("skipped %v, mode %s; want none skipped, hybrid", mo.Skipped, mo.Mode)
			}
		})
	}
}

// failingRetriever fails every call: a store whose database broke after it was opened.
type failingRetriever struct{ store.Retriever }

func (failingRetriever) HasVectors(context.Context) (bool, error) {
	return false, errs.New(errs.Conflict, "test", "database is damaged")
}
func (failingRetriever) FTS(context.Context, store.FTSQuery) (store.FTSResult, error) {
	return store.FTSResult{}, errs.New(errs.Conflict, "test", "database is damaged")
}

// TestSearchStoresFailures (CODING_STYLE §1.6): an extra store's failure is reported in Skipped
// with its ref and leaves the primary's hits; the primary's failure fails the search.
func TestSearchStoresFailures(t *testing.T) {
	corpus := map[string][]string{"a.md": {"zebracorn"}}
	settings, _ := DefaultSettings()
	ok := &Searcher{Store: storeWith(t, nil, corpus), Settings: settings}
	bad := &Searcher{Store: failingRetriever{}, Settings: settings}
	req := Request{Query: "zebracorn", Mode: ModeFTS, K: 5}

	mo, err := SearchStores(context.Background(), []Part{{Root: "/p", Searcher: ok}, {Ref: "bad@/q", Label: "bad", Root: "/q", Searcher: bad}}, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(mo.Skipped) != 1 || mo.Skipped[0].Part.Ref != "bad@/q" || !strings.Contains(mo.Skipped[0].Err.Error(), "bad@/q") || errs.KindOf(mo.Skipped[0].Err) != errs.Conflict {
		t.Errorf("skipped = %+v; want bad@/q with a Conflict naming it", mo.Skipped)
	}
	if len(mo.Result.Hits) != 1 || mo.Result.Hits[0].Store != "" {
		t.Errorf("hits = %+v; want the primary's one hit", mo.Result.Hits)
	}

	_, err = SearchStores(context.Background(), []Part{{Root: "/p", Searcher: bad}, {Ref: "ok@/q", Label: "ok", Root: "/q", Searcher: ok}}, req)
	if errs.KindOf(err) != errs.Conflict {
		t.Errorf("failed primary = %v, want its Conflict", err)
	}

	many := []Part{{Root: "/p", Searcher: ok}}
	for i := range MaxExtraStores + 1 {
		many = append(many, Part{Ref: fmt.Sprintf("s%d@/q", i), Root: "/q", Searcher: ok})
	}
	if _, err := SearchStores(context.Background(), many, req); errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), "9 extra stores") {
		t.Errorf("%d extra stores = %v, want Usage", MaxExtraStores+1, err)
	}
	if _, err := SearchStores(context.Background(), many[:MaxExtraStores+1], req); err != nil {
		t.Errorf("%d extra stores = %v, want ok", MaxExtraStores, err)
	}
}

// TestSearchStoresSingleIsPlainSearch: with no extra store the outcome is the store's own search,
// scores included (no rank-only rescoring).
func TestSearchStoresSingleIsPlainSearch(t *testing.T) {
	s, _ := testSearcher(t)
	req := Request{Query: "zebracorn widget", Mode: ModeHybrid, K: 5}
	want, err := s.Search(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	mo, err := SearchStores(context.Background(), []Part{{Searcher: s}}, req)
	if err != nil {
		t.Fatal(err)
	}
	for i, h := range mo.Result.Hits {
		if w := want.Result.Hits[i]; h.Path != w.Path || h.RRFScore != w.RRFScore || h.Store != "" {
			t.Errorf("hit %d = %+v, want %+v", i, h, w)
		}
	}
}

// TestRecordCrossStore: the primary logs the merged search with the clock it was given, each hit
// under its store (NULL for its own), and returned decided per (store, chunk id) even when two
// stores hand out the same chunk id.
func TestRecordCrossStore(t *testing.T) {
	corpus := map[string][]string{"a.md": {"zebracorn"}}
	settings, _ := DefaultSettings()
	primStore := storeWith(t, nil, corpus)
	at := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	prim := &Searcher{Store: primStore, Log: primStore, Settings: settings, Now: func() time.Time { return at }}
	extra := &Searcher{Store: storeWith(t, nil, corpus), Settings: settings}
	req := Request{Query: "zebracorn", Mode: ModeFTS, K: 5, Caller: "t"}
	mo, err := SearchStores(context.Background(), []Part{{Root: "/p", Searcher: prim}, {Ref: "notes@/q", Label: "notes", Root: "/q", Searcher: extra}}, req)
	if err != nil {
		t.Fatal(err)
	}
	id, err := prim.Record(context.Background(), req, mo.Outcome)
	if err != nil {
		t.Fatal(err)
	}
	tab, err := primStore.QueryLog(context.Background(), `SELECT r.rank, r.store, r.chunk_id, s.ts FROM search_results r JOIN searches s ON s.id = r.search_id WHERE s.id = ? ORDER BY r.rank`, id)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprint([][]any{{int64(1), nil, int64(1), "2026-10-30T09:00:00Z"}, {int64(2), "notes@/q", int64(1), "2026-10-30T09:00:00Z"}})
	if got := fmt.Sprint(tab.Rows); got != want {
		t.Errorf("search_results = %s, want %s", got, want)
	}
	tab, err = primStore.QueryLog(context.Background(), `SELECT count(*) FROM search_candidates WHERE search_id = ? AND returned = 1`, id)
	if err != nil || fmt.Sprint(tab.Rows) != "[[2]]" {
		t.Errorf("returned candidates = %v, %v; want 2", tab, err)
	}
}

// TestQueryMemo: one embedding per distinct (model, dim, text); a failure is shared too.
func TestQueryMemo(t *testing.T) {
	var calls atomic.Int64
	a := queryCounter{fake.New("m", 8), &calls}
	b := queryCounter{fake.New("m", 16), &calls}
	q := &QueryMemo{}
	ctx := context.Background()
	for _, tc := range []struct {
		e    embed.Embedder
		text string
		want int64
	}{{a, "x", 1}, {a, "x", 1}, {a, "y", 2}, {b, "x", 3}, {b, "x", 3}} {
		if _, err := q.embed(ctx, tc.e, tc.text); err != nil {
			t.Fatal(err)
		}
		if n := calls.Load(); n != tc.want {
			t.Errorf("after %s/%d %q: %d calls, want %d", tc.e.Model(), tc.e.Dim(), tc.text, n, tc.want)
		}
	}
	boom := errors.New("boom")
	f := failingEmbedder{Embedder: fake.New("f", 8), err: boom}
	_, err1 := q.embed(ctx, f, "x")
	_, err2 := q.embed(ctx, f, "x")
	if !errors.Is(err1, boom) || !errors.Is(err2, boom) {
		t.Errorf("errors = %v, %v; want boom twice", err1, err2)
	}
	if _, err := (*QueryMemo)(nil).embed(ctx, a, "x"); err != nil {
		t.Errorf("nil memo: %v", err)
	}
}

// TestMergedBefore pins Merge's total order pair by pair: score, then store in merge order, then
// path, then ord; it is irreflexive and antisymmetric.
func TestMergedBefore(t *testing.T) {
	m := func(score float64, part int, path string, ord int) *merged {
		return &merged{key: mergeKey{root: "/r", path: path, ord: ord}, score: score, bestPart: part}
	}
	for _, tc := range []struct {
		name string
		a, b *merged
	}{
		{"higher score first", m(0.2, 1, "z", 9), m(0.1, 0, "a", 0)},
		{"tie: earlier store first", m(0.1, 0, "z", 9), m(0.1, 1, "a", 0)},
		{"tie, same store: path", m(0.1, 1, "a", 9), m(0.1, 1, "b", 0)},
		{"tie, same store and path: ord", m(0.1, 1, "a", 0), m(0.1, 1, "a", 1)},
	} {
		if !mergedBefore(tc.a, tc.b) || mergedBefore(tc.b, tc.a) {
			t.Errorf("%s: before(a,b)=%v before(b,a)=%v; want true, false", tc.name, mergedBefore(tc.a, tc.b), mergedBefore(tc.b, tc.a))
		}
		if mergedBefore(tc.a, tc.a) {
			t.Errorf("%s: before(a,a) is true", tc.name)
		}
	}
}

// TestMergeResetsCapDropped: a store's own cap marks do not carry into the merged list, and
// the per-store list counts are summed.
func TestMergeResetsCapDropped(t *testing.T) {
	c := cand(1, "a.md", 0)
	c.CapDropped = true
	prim, extra := outcomeOf(8, c), outcomeOf(8, cand(2, "b.md", 0))
	prim.NFTS, prim.NVec, extra.NFTS, extra.NVec = 3, 4, 5, 7
	got := Merge([]Part{{Root: "/p"}, {Ref: "x@/q", Label: "x", Root: "/q"}}, []*Outcome{prim, extra})
	for _, c := range got.Candidates {
		if c.CapDropped {
			t.Errorf("merged candidate %+v keeps CapDropped", c)
		}
	}
	if got.NFTS != 8 || got.NVec != 11 {
		t.Errorf("NFTS, NVec = %d, %d; want the sums 8, 11", got.NFTS, got.NVec)
	}
}

// TestSearchStoresDegraded: an extra store whose embedder is unavailable takes part with its FTS
// list and is reported in Degraded; an FTS-only store run with Mode fts is not degraded.
func TestSearchStoresDegraded(t *testing.T) {
	corpus := map[string][]string{"a.md": {"zebracorn"}}
	settings, _ := DefaultSettings()
	emb := fake.New("m", 8)
	prim := &Searcher{Store: storeWith(t, emb, corpus), Embedder: emb, Settings: settings}
	down := failingEmbedder{Embedder: fake.New("m", 8), err: errs.Wrap(embed.ErrUnavailable, errs.Unavailable, "t", "model not downloaded")}
	broken := &Searcher{Store: storeWith(t, emb, corpus), Embedder: down, Settings: settings}
	ftsOnly := &Searcher{Store: storeWith(t, nil, corpus), Settings: settings}
	mo, err := SearchStores(context.Background(), []Part{{Root: "/p", Searcher: prim},
		{Ref: "b@/q", Label: "b", Root: "/q", Searcher: broken},
		{Ref: "f@/r", Label: "f", Root: "/r", Mode: ModeFTS, Searcher: ftsOnly}},
		Request{Query: "zebracorn", Mode: ModeHybrid, K: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(mo.Degraded) != 1 || mo.Degraded[0].Part.Ref != "b@/q" || errs.KindOf(mo.Degraded[0].Err) != errs.Unavailable {
		t.Errorf("degraded = %+v; want b@/q only", mo.Degraded)
	}
	if mo.Mode != ModeHybrid || len(mo.Result.Hits) != 3 {
		t.Errorf("mode %s, %d hits; want hybrid and one hit per store", mo.Mode, len(mo.Result.Hits))
	}
}

// TestSearchStoresPrimaryFallbackNotDegraded: the primary's own FTS fallback is its Outcome's
// (Mode, FallbackErr, the CLI's exit 2), never a Degraded extra.
func TestSearchStoresPrimaryFallbackNotDegraded(t *testing.T) {
	corpus := map[string][]string{"a.md": {"zebracorn"}}
	settings, _ := DefaultSettings()
	emb := fake.New("m", 8)
	down := failingEmbedder{Embedder: emb, err: errs.Wrap(embed.ErrUnavailable, errs.Unavailable, "t", "model not downloaded")}
	prim := &Searcher{Store: storeWith(t, emb, corpus), Embedder: down, Settings: settings}
	extra := &Searcher{Store: storeWith(t, emb, corpus), Embedder: emb, Settings: settings}
	mo, err := SearchStores(context.Background(), []Part{{Root: "/p", Searcher: prim}, {Ref: "x@/q", Label: "x", Root: "/q", Searcher: extra}},
		Request{Query: "zebracorn", Mode: ModeHybrid, K: 5})
	if err != nil {
		t.Fatal(err)
	}
	if mo.Mode != ModeFTSFallback || mo.FallbackErr == nil || len(mo.Degraded) != 0 {
		t.Errorf("mode %s, fallback %v, degraded %+v; want the primary's fallback and nothing degraded", mo.Mode, mo.FallbackErr, mo.Degraded)
	}
}

// panickingRetriever panics in FTS: a bug inside one store's search.
type panickingRetriever struct{ store.Retriever }

func (panickingRetriever) HasVectors(context.Context) (bool, error) { return false, nil }
func (panickingRetriever) FTS(context.Context, store.FTSQuery) (store.FTSResult, error) {
	panic(assert.Violation{Kind: "invariant", Msg: "boom in a store"})
}

// TestSearchStoresWorkerPanic (CODING_STYLE §1.5): a panic in any store's search goroutine,
// primary or extra, is raised again on the caller's goroutine as errs.WorkerPanic with the
// original value and the worker's stack, so cli.Execute's recover reports it (exit 3).
func TestSearchStoresWorkerPanic(t *testing.T) {
	corpus := map[string][]string{"a.md": {"zebracorn"}}
	settings, _ := DefaultSettings()
	ok := &Searcher{Store: storeWith(t, nil, corpus), Settings: settings}
	bad := &Searcher{Store: panickingRetriever{}, Settings: settings}
	req := Request{Query: "zebracorn", Mode: ModeFTS, K: 5}
	for _, tc := range []struct {
		name  string
		parts []Part
	}{
		{"single store", []Part{{Root: "/p", Searcher: bad}}},
		{"primary", []Part{{Root: "/p", Searcher: bad}, {Ref: "ok@/q", Root: "/q", Searcher: ok}}},
		{"extra store", []Part{{Root: "/p", Searcher: ok}, {Ref: "bad@/q", Root: "/q", Searcher: bad}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				wp, isWP := recover().(errs.WorkerPanic)
				v, isV := wp.Value.(assert.Violation)
				if !isWP || !isV || v.Msg != "boom in a store" || !strings.Contains(string(wp.Stack), "panickingRetriever") {
					t.Errorf("recovered %#v; want errs.WorkerPanic carrying the violation and the worker's stack", wp)
				}
			}()
			SearchStores(context.Background(), tc.parts, req)
			t.Error("SearchStores returned; want the worker's panic raised again")
		})
	}
}
