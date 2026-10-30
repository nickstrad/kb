// Package storetest is the contract every storage backend must meet (docs/TESTING.md §6). A
// backend's tests call Run with a Factory; behaviour specific to one backend (PRAGMAs,
// migrations, concurrent Open) stays in that backend's own tests.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
)

// Factory returns a fresh, empty store with embed_meta recorded for dim (0 = FTS only, model
// store.FTSOnlyModel; otherwise model Model(dim)). It registers its own cleanup.
type Factory func(t testing.TB, dim int) store.Store

// Model is the embed_meta model name a Factory records for dim > 0.
func Model(dim int) string { return fmt.Sprintf("storetest-%d", dim) }

// Meta is the embed_meta a Factory records for dim.
func Meta(dim int) store.EmbedMeta {
	if dim == 0 {
		return store.EmbedMeta{Model: store.FTSOnlyModel}
	}
	return store.EmbedMeta{Model: Model(dim), Dim: dim}
}

// Run runs the whole contract against stores from newStore, for an FTS-only and a vector index.
func Run(t *testing.T, newStore Factory) {
	for _, dim := range []int{0, 8} {
		t.Run(fmt.Sprint("dim=", dim), func(t *testing.T) {
			t.Run("replace writes entry, chunks, fts and vec rows", func(t *testing.T) { replaceWrites(t, newStore(t, dim), dim) })
			t.Run("replace replaces; no stale chunks", func(t *testing.T) { replaceReplaces(t, newStore(t, dim), dim) })
			t.Run("delete removes all rows", func(t *testing.T) { deleteRemoves(t, newStore(t, dim), dim) })
			t.Run("wrong-dimension vector is a Conflict, previous version intact", func(t *testing.T) { badVectors(t, newStore(t, dim), dim) })
			t.Run("embed_meta: same is ok, mismatch is a Conflict", func(t *testing.T) { embedMeta(t, newStore(t, dim), dim) })
			t.Run("FTS: AND/OR ops, tag filter, unmatched terms", func(t *testing.T) { fts(t, newStore(t, dim), dim) })
			t.Run("FTS Exact: identifiers are whole tokens", func(t *testing.T) { exactFTS(t, newStore(t, dim), dim) })
			t.Run("KNN: top-k ordered by distance, tag filter", func(t *testing.T) { knn(t, newStore(t, dim), dim) })
			t.Run("tags: kept as written, filtered case-insensitively", func(t *testing.T) { tags(t, newStore(t, dim), dim) })
			t.Run("aliases: round-trip, found as the title", func(t *testing.T) { aliases(t, newStore(t, dim), dim) })
			t.Run("edges: written, resolved, kept, dangling", func(t *testing.T) { edgesContract(t, newStore(t, dim), dim) })
			t.Run("related: both directions, cycles, depth bounds, supersede", func(t *testing.T) { related(t, newStore(t, dim), dim) })
			t.Run("ResetIndex empties the index, keeps the log", func(t *testing.T) { reset(t, newStore(t, dim), dim) })
			t.Run("SearchLog: log, feedback, LastSearch, RecordOpen", func(t *testing.T) { searchLog(t, newStore(t, dim), dim) })
			t.Run("SearchLog: settings, ts and hit stores round-trip", func(t *testing.T) { searchLogRoundTrip(t, newStore(t, dim)) })
			t.Run("EntriesByPath: one read, missing paths absent", func(t *testing.T) { entriesByPath(t, newStore(t, dim), dim) })
			t.Run("JudgedLog: verdict stores, opens by store, since bound", func(t *testing.T) { judgedLog(t, newStore(t, dim)) })
			t.Run("model test (seeded ops)", func(t *testing.T) {
				for _, seed := range ModelSeeds {
					RunModel(t, newStore(t, dim), dim, seed)
				}
			})
		})
	}
}

var ctx = context.Background()

// openWindow is the window the contract tests pass to RecordOpen.
const openWindow = 30 * time.Minute

// vec is a deterministic unit-ish vector for a text, so KNN order is predictable: the vector of
// "k<i>" is i on the first axis.
func vec(dim int, seed float32) []float32 {
	if dim == 0 {
		return nil
	}
	v := make([]float32, dim)
	v[0] = 1
	v[1] = seed
	return v
}

func chunks(dim int, texts ...string) []store.ChunkInput {
	out := make([]store.ChunkInput, len(texts))
	for i, text := range texts {
		heading := ""
		if i > 0 {
			heading = fmt.Sprint("h", i)
		}
		out[i] = store.ChunkInput{Ord: i, SourceFile: "f.md", Heading: heading, Text: text, TextHash: "hash:" + text,
			Embedding: vec(dim, float32(i))}
	}
	return out
}

func entryIn(path string, tags ...string) store.EntryInput {
	return store.EntryInput{Path: path, Kind: "file", Title: "Title " + path, Summary: "S", Tags: tags, BodyHash: "b:" + path}
}

func mustReplace(t testing.TB, st store.Store, e store.EntryInput, cs []store.ChunkInput) int64 {
	t.Helper()
	id, err := st.ReplaceEntry(ctx, e, cs)
	if err != nil {
		t.Fatalf("ReplaceEntry %s: %v", e.Path, err)
	}
	return id
}

func ftsIDs(t testing.TB, st store.Store, op store.FTSOp, tag string, terms ...string) []int64 {
	t.Helper()
	return queryIDs(t, st, store.FTSQuery{Terms: terms, Op: op, Tag: tag, Limit: 20})
}

// exactIDs is ftsIDs over the identifier index (FTSQuery.Exact).
func exactIDs(t testing.TB, st store.Store, terms ...string) []int64 {
	t.Helper()
	return queryIDs(t, st, store.FTSQuery{Terms: terms, Limit: 20, Exact: true})
}

func queryIDs(t testing.TB, st store.Store, q store.FTSQuery) []int64 {
	t.Helper()
	res, err := st.FTS(ctx, q)
	if err != nil {
		t.Fatalf("FTS %+v: %v", q, err)
	}
	ids := make([]int64, len(res.Rows))
	for i, r := range res.Rows {
		ids[i] = r.ChunkID
	}
	slices.Sort(ids)
	return ids
}

func chunkIDs(t testing.TB, st store.Store, entryID int64) []int64 {
	t.Helper()
	cs, err := st.Chunks(ctx, entryID)
	if err != nil {
		t.Fatalf("Chunks: %v", err)
	}
	ids := make([]int64, len(cs))
	for i, c := range cs {
		ids[i] = c.ID
	}
	slices.Sort(ids)
	return ids
}

func replaceWrites(t *testing.T, st store.Store, dim int) {
	id := mustReplace(t, st, entryIn("a.md", "go"), chunks(dim, "alpha zebracorn", "beta zebracorn"))
	e, err := st.Entry(ctx, "a.md")
	if err != nil || e == nil || e.ID != id || e.Title != "Title a.md" || !slices.Equal(e.Tags, []string{"go"}) || e.BodyHash != "b:a.md" {
		t.Fatalf("Entry = %+v, %v", e, err)
	}
	cs, err := st.Chunks(ctx, id)
	if err != nil || len(cs) != 2 || cs[0].Ord != 0 || cs[1].Text != "beta zebracorn" || cs[1].Heading != "h1" || cs[0].TextHash != "hash:alpha zebracorn" {
		t.Fatalf("Chunks = %+v, %v", cs, err)
	}
	if got := ftsIDs(t, st, store.OpAnd, "", "zebracorn"); !slices.Equal(got, chunkIDs(t, st, id)) {
		t.Errorf("FTS ids %v, want the entry's chunks", got)
	}
	if got := exactIDs(t, st, "zebracorn"); !slices.Equal(got, chunkIDs(t, st, id)) {
		t.Errorf("exact ids %v, want the entry's chunks", got)
	}
	meta, err := st.ChunkMeta(ctx, append(chunkIDs(t, st, id), 999999))
	want := store.ChunkMeta{EntryPath: "a.md", Heading: "h1", Text: "beta zebracorn", EntryID: id, Ord: 1, SourceFile: "f.md"}
	if err != nil || len(meta) != 2 || meta[cs[1].ID] != want {
		t.Errorf("ChunkMeta = %+v, %v; want the two chunks, the dangling id absent", meta, err)
	}
	dated := entryIn("d.md")
	dated.Updated = "2026-01-02"
	did := mustReplace(t, st, dated, chunks(dim, "dated chunk"))
	if meta, err := st.ChunkMeta(ctx, chunkIDs(t, st, did)); err != nil || len(meta) != 1 {
		t.Errorf("ChunkMeta(dated) = %+v, %v", meta, err)
	} else {
		for _, m := range meta {
			if m.EntryUpdated != "2026-01-02" || m.EntryID != did {
				t.Errorf("ChunkMeta(dated) = %+v; want EntryUpdated 2026-01-02, EntryID %d", m, did)
			}
		}
	}
	embs, err := st.Embeddings(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	has, err := st.HasVectors(ctx)
	if err != nil || has != (dim > 0) {
		t.Errorf("HasVectors = %v, %v; want %v", has, err, dim > 0)
	}
	if dim == 0 {
		if len(embs) != 0 {
			t.Errorf("FTS-only Embeddings = %v, want none", embs)
		}
		return
	}
	if len(embs) != 2 || !slices.Equal(embs["hash:beta zebracorn"], vec(dim, 1)) {
		t.Errorf("Embeddings = %v", embs)
	}
}

func replaceReplaces(t *testing.T, st store.Store, dim int) {
	id1 := mustReplace(t, st, entryIn("a.md"), chunks(dim, "oldword one", "oldword two", "oldword three"))
	id2 := mustReplace(t, st, entryIn("a.md"), chunks(dim, "newword"))
	if id1 != id2 {
		t.Errorf("entry id changed %d -> %d; the upsert is by path", id1, id2)
	}
	if got := ftsIDs(t, st, store.OpAnd, "", "oldword"); len(got) != 0 {
		t.Errorf("stale FTS rows %v", got)
	}
	if got := exactIDs(t, st, "oldword"); len(got) != 0 {
		t.Errorf("stale exact rows %v", got)
	}
	if got := exactIDs(t, st, "newword"); !slices.Equal(got, chunkIDs(t, st, id2)) {
		t.Errorf("exact rows %v, want the new chunk", got)
	}
	if got := chunkIDs(t, st, id2); len(got) != 1 {
		t.Errorf("chunks = %v, want 1", got)
	}
	if embs, _ := st.Embeddings(ctx, id2); dim > 0 && len(embs) != 1 {
		t.Errorf("stale vectors: %v", embs)
	}
	es, err := st.Entries(ctx)
	if err != nil || len(es) != 1 {
		t.Errorf("Entries = %v, %v", es, err)
	}
}

func deleteRemoves(t *testing.T, st store.Store, dim int) {
	id := mustReplace(t, st, entryIn("a.md"), chunks(dim, "gone word"))
	mustReplace(t, st, entryIn("b.md"), chunks(dim, "kept word"))
	if err := st.DeleteEntry(ctx, "a.md"); err != nil {
		t.Fatal(err)
	}
	if e, err := st.Entry(ctx, "a.md"); e != nil || err != nil {
		t.Errorf("Entry after delete = %v, %v", e, err)
	}
	if got := chunkIDs(t, st, id); len(got) != 0 {
		t.Errorf("chunks left: %v", got)
	}
	if got := ftsIDs(t, st, store.OpAnd, "", "gone"); len(got) != 0 {
		t.Errorf("FTS rows left: %v", got)
	}
	if got := exactIDs(t, st, "gone"); len(got) != 0 {
		t.Errorf("exact rows left: %v", got)
	}
	if got := exactIDs(t, st, "kept"); len(got) != 1 {
		t.Errorf("the other entry lost its exact rows: %v", got)
	}
	if got := ftsIDs(t, st, store.OpAnd, "", "kept"); len(got) != 1 {
		t.Errorf("the other entry lost its rows: %v", got)
	}
	if err := st.DeleteEntry(ctx, "never.md"); err != nil {
		t.Errorf("deleting a missing entry = %v, want nil", err)
	}
}

func badVectors(t *testing.T, st store.Store, dim int) {
	id := mustReplace(t, st, entryIn("a.md"), chunks(dim, "original text"))
	bad := chunks(dim, "replacement text")
	if dim == 0 {
		bad[0].Embedding = []float32{1}
	} else {
		bad[0].Embedding = bad[0].Embedding[:dim-1]
	}
	if _, err := st.ReplaceEntry(ctx, entryIn("a.md"), bad); errs.KindOf(err) != errs.Conflict {
		t.Fatalf("bad vector: err = %v (kind %v), want a Conflict", err, errs.KindOf(err))
	}
	if dim > 0 {
		bad[0].Embedding = nil
		if _, err := st.ReplaceEntry(ctx, entryIn("new.md"), bad); errs.KindOf(err) != errs.Conflict {
			t.Fatalf("missing vector: err = %v, want a Conflict", err)
		}
		if e, _ := st.Entry(ctx, "new.md"); e != nil {
			t.Error("a refused entry was written")
		}
	}
	if got := ftsIDs(t, st, store.OpAnd, "", "original"); !slices.Equal(got, chunkIDs(t, st, id)) || len(got) != 1 {
		t.Errorf("previous version damaged: fts %v", got)
	}
	if got := ftsIDs(t, st, store.OpAnd, "", "replacement"); len(got) != 0 {
		t.Errorf("refused chunks written: %v", got)
	}
	if got := exactIDs(t, st, "replacement"); len(got) != 0 {
		t.Errorf("refused chunks written to the exact index: %v", got)
	}
	if got := exactIDs(t, st, "original"); !slices.Equal(got, chunkIDs(t, st, id)) {
		t.Errorf("previous version damaged: exact %v", got)
	}
}

func embedMeta(t *testing.T, st store.Store, dim int) {
	m, ok, err := st.EmbedMeta(ctx)
	if err != nil || !ok || m != Meta(dim) {
		t.Fatalf("EmbedMeta = %+v, %v, %v; want %+v", m, ok, err, Meta(dim))
	}
	if err := st.EnsureEmbedMeta(ctx, Meta(dim)); err != nil {
		t.Errorf("same meta again = %v", err)
	}
	other := store.EmbedMeta{Model: "other-model", Dim: 16}
	err = st.EnsureEmbedMeta(ctx, other)
	var mm *store.EmbedMetaMismatchError
	if errs.KindOf(err) != errs.Conflict || !errors.As(err, &mm) || mm.StoredModel != Meta(dim).Model || mm.WantDim != 16 {
		t.Errorf("mismatch = %v (kind %v), want a Conflict wrapping EmbedMetaMismatchError", err, errs.KindOf(err))
	}
	if m2, _, _ := st.EmbedMeta(ctx); m2 != Meta(dim) {
		t.Errorf("a refused EnsureEmbedMeta changed the record to %+v", m2)
	}
}

func fts(t *testing.T, st store.Store, dim int) {
	a := mustReplace(t, st, entryIn("a.md", "docker"), chunks(dim, "quokka widget"))
	mustReplace(t, st, entryIn("b.md", "docker-compose"), chunks(dim, "quokka gadget"))
	aIDs := chunkIDs(t, st, a)
	if got := ftsIDs(t, st, store.OpAnd, "", "quokka", "widget"); !slices.Equal(got, aIDs) {
		t.Errorf("AND = %v, want only a.md", got)
	}
	if got := ftsIDs(t, st, store.OpOr, "", "widget", "gadget"); len(got) != 2 {
		t.Errorf("OR = %v, want both", got)
	}
	if got := ftsIDs(t, st, store.OpAnd, "docker", "quokka"); !slices.Equal(got, aIDs) {
		t.Errorf("tag docker = %v, want only a.md (not docker-compose)", got)
	}
	if got := ftsIDs(t, st, store.OpAnd, "%", "quokka"); len(got) != 0 {
		t.Errorf("tag %% = %v, want nothing: a tag is not a pattern", got)
	}
	res, err := st.FTS(ctx, store.FTSQuery{Terms: []string{"&&", "..."}, Limit: 5})
	if err != nil || res.Match != "" || len(res.Rows) != 0 {
		t.Errorf("unmatchable terms = %+v, %v; want no query run", res, err)
	}
	res, err = st.FTS(ctx, store.FTSQuery{Terms: []string{"quokka"}, Limit: 1})
	if err != nil || len(res.Rows) != 1 || res.Match == "" {
		t.Errorf("Limit 1 = %+v, %v", res, err)
	}
	ftsChunkIDs(t, st, aIDs)
	ftsWeights(t, st)
	commonTerm(t, st, dim)
}

// commonTerm: a term in all but one of N chunks has an idf near zero (FTS5 clamps it to a tiny
// positive value), so its bm25 scores are about -1e-6, but every matching chunk is still a row:
// a common term is ranked low, never dropped. Both indexes, one entry and one entry per chunk.
func commonTerm(t *testing.T, st store.Store, dim int) {
	for n := 2; n <= 5; n++ {
		for _, spread := range []bool{false, true} {
			if err := st.ResetIndex(ctx); err != nil {
				t.Fatal(err)
			}
			if err := st.EnsureEmbedMeta(ctx, Meta(dim)); err != nil {
				t.Fatal(err)
			}
			texts := make([]string, n)
			for i := range texts {
				texts[i] = fmt.Sprintf("Notes › S%d\n\ncommonterm chunk %d", i, i)
			}
			texts[n-1] = "Notes › Other\n\nnothing here"
			var want []int64
			if spread {
				for i, text := range texts {
					id := mustReplace(t, st, entryIn(fmt.Sprintf("e%d.md", i)), chunks(dim, text))
					if i < n-1 {
						want = append(want, chunkIDs(t, st, id)...)
					}
				}
			} else {
				id := mustReplace(t, st, entryIn("a.md"), chunks(dim, texts...))
				want = chunkIDs(t, st, id)[:n-1]
			}
			slices.Sort(want)
			if got := ftsIDs(t, st, store.OpAnd, "", "commonterm"); !slices.Equal(got, want) {
				t.Errorf("n=%d spread=%v: FTS commonterm = %v, want all %d matching chunks %v", n, spread, got, n-1, want)
			}
			if got := exactIDs(t, st, "commonterm"); !slices.Equal(got, want) {
				t.Errorf("n=%d spread=%v: exact commonterm = %v, want %v", n, spread, got, want)
			}
		}
	}
}

// ftsChunkIDs: a non-nil ChunkIDs restricts the rows to those ids (with or without a tag); an
// empty non-nil one runs nothing but still reports the expression.
func ftsChunkIDs(t *testing.T, st store.Store, aIDs []int64) {
	for _, tc := range []struct {
		name string
		ids  []int64
		tag  string
		want int
	}{
		{"restricted to a.md", aIDs, "", len(aIDs)},
		{"restricted to a.md and a dangling id", append(slices.Clone(aIDs), 999999), "", len(aIDs)},
		{"restricted to a.md, tag docker", aIDs, "docker", len(aIDs)},
		{"restricted to a.md, tag docker-compose", aIDs, "docker-compose", 0},
		{"empty set", []int64{}, "", 0},
	} {
		res, err := st.FTS(ctx, store.FTSQuery{Terms: []string{"quokka"}, Tag: tc.tag, Limit: 20, ChunkIDs: tc.ids})
		if err != nil || len(res.Rows) != tc.want || res.Match == "" {
			t.Errorf("%s: FTS = %+v, %v; want %d rows and the expression", tc.name, res, err, tc.want)
		}
		for _, r := range res.Rows {
			if !slices.Contains(tc.ids, r.ChunkID) {
				t.Errorf("%s: row %d is outside the restriction %v", tc.name, r.ChunkID, tc.ids)
			}
		}
	}
}

// ftsWeights: a column of weight 0 adds nothing to the score. "quokka" occurs only in chunk text
// and "Title" only in entry titles, so each scores 0 exactly when its column's weight is 0.
func ftsWeights(t *testing.T, st store.Store) {
	score := func(term string, w *store.BM25Weights) float64 {
		t.Helper()
		res, err := st.FTS(ctx, store.FTSQuery{Terms: []string{term}, Limit: 1, Weights: w})
		if err != nil || len(res.Rows) != 1 {
			t.Fatalf("FTS %q %+v = %+v, %v", term, w, res, err)
		}
		return res.Rows[0].Score
	}
	for _, tc := range []struct {
		term     string
		w        store.BM25Weights
		wantZero bool
	}{
		{"quokka", store.BM25Weights{Text: 1}, false},
		{"quokka", store.BM25Weights{Heading: 1, Title: 1}, true},
		{"Title", store.BM25Weights{Title: 1}, false},
		{"Title", store.BM25Weights{Text: 1, Heading: 1}, true},
		{"quokka", store.BM25Weights{}, true},
	} {
		if got := score(tc.term, &tc.w); (got == 0) != tc.wantZero {
			t.Errorf("score(%q, %+v) = %v, want zero=%v", tc.term, tc.w, got, tc.wantZero)
		}
	}
	def := store.DefaultBM25()
	if a, b := score("quokka", nil), score("quokka", &def); a != b {
		t.Errorf("nil weights score %v, DefaultBM25 %v; want equal", a, b)
	}
}

func knn(t *testing.T, st store.Store, dim int) {
	if dim == 0 {
		if has, err := st.HasVectors(ctx); has || err != nil {
			t.Errorf("FTS-only HasVectors = %v, %v", has, err)
		}
		return
	}
	id := mustReplace(t, st, entryIn("a.md", "x"), chunks(dim, "c0", "c1", "c2", "c3"))
	mustReplace(t, st, entryIn("b.md", "y"), chunks(dim, "d0"))
	rows, err := st.KNN(ctx, store.KNNQuery{Vec: vec(dim, 3), K: 3})
	if err != nil || len(rows) != 3 {
		t.Fatalf("KNN = %v, %v", rows, err)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].Distance < rows[i-1].Distance {
			t.Errorf("KNN not ordered by distance: %v", rows)
		}
	}
	ids := chunkIDs(t, st, id)
	if rows[0].ChunkID != ids[3] {
		t.Errorf("nearest = %d, want chunk c3 (%d)", rows[0].ChunkID, ids[3])
	}
	tagged, err := st.KNN(ctx, store.KNNQuery{Vec: vec(dim, 0), K: 2, Tag: "y", OverFetch: 10})
	if err != nil || len(tagged) != 1 {
		t.Errorf("KNN tag y = %v, %v; want only b.md's chunk", tagged, err)
	}
	if rows, err := st.KNN(ctx, store.KNNQuery{K: 3}); len(rows) != 0 || err != nil {
		t.Errorf("empty query vector = %v, %v", rows, err)
	}
}

func reset(t *testing.T, st store.Store, dim int) {
	withLinks := entryIn("a.md", "resettag")
	withLinks.Edges = []store.EdgeInput{edge("a.md", "links", store.EdgePath)}
	mustReplace(t, st, withLinks, chunks(dim, "resetword"))
	sid, err := st.LogSearch(ctx, store.SearchRecord{Query: "q", Mode: "fts", K: 5, Caller: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ResetIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if es, _ := st.Entries(ctx); len(es) != 0 {
		t.Errorf("entries after reset: %v", es)
	}
	if _, ok, _ := st.EmbedMeta(ctx); ok {
		t.Error("embed_meta survived reset")
	}
	if has, _ := st.HasVectors(ctx); has {
		t.Error("vectors survived reset")
	}
	if ls, err := st.LastSearch(ctx, "c"); err != nil || ls.ID != sid {
		t.Errorf("search log lost by reset: %+v, %v", ls, err)
	}
	if err := st.EnsureEmbedMeta(ctx, Meta(dim)); err != nil {
		t.Fatalf("re-record after reset: %v", err)
	}
	if got := ftsIDs(t, st, store.OpAnd, "", "resetword"); len(got) != 0 {
		t.Errorf("FTS rows survived reset: %v", got)
	}
	if got := exactIDs(t, st, "resetword"); len(got) != 0 {
		t.Errorf("exact rows survived reset: %v", got)
	}
	if got := edgeStrings(t, st); len(got) != 0 {
		t.Errorf("edges survived reset: %q", got)
	}
	mustReplace(t, st, entryIn("b.md"), chunks(dim, "afterreset"))
	if got := ftsIDs(t, st, store.OpAnd, "resettag", "afterreset"); len(got) != 0 {
		t.Errorf("tag rows survived reset: %v", got)
	}
}

// exactFTS: the identifier index keeps `_ - . : /` inside a token, does not stem and does not fold
// diacritics (it does fold case), so an identifier matches only itself, never its words split
// apart or a longer identifier that contains it. The main index, for contrast, matches the split
// words as a phrase.
func exactFTS(t *testing.T, st store.Store, dim int) {
	long := strings.Repeat("x", 400) + "_tail"
	docs := []string{
		"use foo_bar here",                    // 0
		"pass --embed-model to kb",            // 1
		"edit kb.toml now",                    // 2
		"path a/b:c done",                     // 3
		"café naïve straße",                   // 4
		"the " + long + " token",              // 5
		"foo bar kb toml embed model a b c",   // 6: the same words, split
		"run reindex_test and pkg.Func calls", // 7
	}
	id := mustReplace(t, st, entryIn("a.md", "go"), chunks(dim, docs...))
	other := mustReplace(t, st, entryIn("b.md"), chunks(dim, "foo_bar elsewhere"))
	ids := chunkIDs(t, st, id)
	only := func(i ...int) []int64 {
		out := make([]int64, len(i))
		for j, k := range i {
			out[j] = ids[k]
		}
		slices.Sort(out)
		return out
	}
	for _, tc := range []struct {
		name  string
		terms []string
		want  []int64
	}{
		{"snake_case", []string{"foo_bar"}, append(only(0), chunkIDs(t, st, other)...)},
		{"case folds", []string{"FOO_BAR"}, append(only(0), chunkIDs(t, st, other)...)},
		{"a word of a snake_case identifier is not a token", []string{"foo"}, only(6)},
		{"--flag", []string{"--embed-model"}, only(1)},
		{"flag without its dashes is another token", []string{"embed-model"}, nil},
		{"dotted file name", []string{"kb.toml"}, only(2)},
		{"pkg.Func", []string{"pkg.Func"}, only(7)},
		{"underscore test name", []string{"reindex_test"}, only(7)},
		{"path with colon", []string{"a/b:c"}, only(3)},
		{"a prefix of a path is not a token", []string{"a/b"}, nil},
		{"diacritics kept", []string{"café"}, only(4)},
		{"diacritics not folded", []string{"cafe"}, nil},
		{"no stemming", []string{"calls"}, only(7)},
		{"no stemming: stem", []string{"call"}, nil},
		{"very long token", []string{long}, only(5)},
		{"surrounding punctuation is not part of the token", []string{"(kb.toml),"}, only(2)},
		{"two identifiers AND", []string{"reindex_test", "pkg.Func"}, only(7)},
	} {
		if got := exactIDs(t, st, tc.terms...); !slices.Equal(got, tc.want) {
			t.Errorf("%s: exact %v = %v, want %v", tc.name, tc.terms, got, tc.want)
		}
	}
	// The main index splits identifiers into words and matches them as a phrase.
	if got := ftsIDs(t, st, store.OpAnd, "", "foo_bar"); !slices.Contains(got, ids[6]) || !slices.Contains(got, ids[0]) {
		t.Errorf("main FTS foo_bar = %v, want the split words (chunk %d) as well as %d", got, ids[6], ids[0])
	}
	if got := ftsIDs(t, st, store.OpAnd, "", "cafe"); !slices.Equal(got, only(4)) {
		t.Errorf("main FTS cafe = %v, want the folded café", got)
	}
	// Tag filter, chunk restriction and weights work as on the main index.
	if got := queryIDs(t, st, store.FTSQuery{Terms: []string{"foo_bar"}, Tag: "go", Limit: 20, Exact: true}); !slices.Equal(got, only(0)) {
		t.Errorf("exact foo_bar tag go = %v, want only a.md's chunk", got)
	}
	if got := queryIDs(t, st, store.FTSQuery{Terms: []string{"foo_bar"}, Limit: 20, Exact: true, ChunkIDs: chunkIDs(t, st, other)}); !slices.Equal(got, chunkIDs(t, st, other)) {
		t.Errorf("exact foo_bar restricted to b.md = %v", got)
	}
	res, err := st.FTS(ctx, store.FTSQuery{Terms: []string{"foo_bar"}, Limit: 1, Exact: true, Weights: &store.BM25Weights{Title: 5}})
	if err != nil || len(res.Rows) != 1 || res.Rows[0].Score != 0 || res.Match == "" {
		t.Errorf("exact with only a title weight = %+v, %v; want one row scoring 0 (no title column)", res, err)
	}
	// The heading is the exact index's second column: its weight alone scores a heading-only
	// term, the text weight alone does not. chunks() names chunk 1's heading "h1".
	for _, tc := range []struct {
		w        store.BM25Weights
		wantZero bool
	}{{store.BM25Weights{Heading: 1}, false}, {store.BM25Weights{Text: 1, Title: 1}, true}} {
		res, err := st.FTS(ctx, store.FTSQuery{Terms: []string{"h1"}, Limit: 1, Exact: true, Weights: &tc.w, ChunkIDs: only(1)})
		if err != nil || len(res.Rows) != 1 || (res.Rows[0].Score == 0) != tc.wantZero {
			t.Errorf("exact heading term with %+v = %+v, %v; want zero=%v", tc.w, res.Rows, err, tc.wantZero)
		}
	}
	res, err = st.FTS(ctx, store.FTSQuery{Terms: []string{"&&"}, Limit: 5, Exact: true})
	if err != nil || res.Match != "" || len(res.Rows) != 0 {
		t.Errorf("unmatchable exact terms = %+v, %v; want no query run", res, err)
	}
}

func searchLog(t *testing.T, st store.Store, dim int) {
	ms := int64(3)
	hit := store.LoggedHit{ChunkID: 1, EntryPath: "a.md", RRFScore: 0.5, FTSRank: new(int)}
	rec := store.SearchRecord{Query: "q1", Mode: "fts", K: 5, FTSMs: &ms, TotalMs: 4, Caller: "alice",
		Results:    []store.LoggedHit{hit, {ChunkID: 2, EntryPath: "b.md", RRFScore: 0.4}},
		Candidates: []store.LoggedHit{hit, {ChunkID: 2, EntryPath: "b.md"}, {ChunkID: 3, EntryPath: "c.md"}}}
	id1, err := st.LogSearch(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	rec.Query, rec.Results, rec.Candidates = "q2", nil, nil
	id2, err := st.LogSearch(ctx, rec)
	if err != nil || id2 <= id1 {
		t.Fatalf("second LogSearch = %d, %v", id2, err)
	}
	if ls, err := st.LastSearch(ctx, "alice"); err != nil || ls.ID != id2 || ls.Query != "q2" || ls.NReturned != 0 || ls.TS == "" {
		t.Errorf("LastSearch = %+v, %v", ls, err)
	}
	if _, err := st.LastSearch(ctx, "nobody"); errs.KindOf(err) != errs.NotFound {
		t.Errorf("LastSearch(nobody) = %v, want NotFound", err)
	}
	for _, tc := range []struct {
		id   int64
		rank int
		kind errs.Kind
	}{{id1, 0, errs.Usage}, {id1, 3, errs.Usage}, {id2, 1, errs.Usage}, {id2 + 100, 1, errs.NotFound}} {
		if err := st.Feedback(ctx, tc.id, tc.rank, true, "", time.Time{}); errs.KindOf(err) != tc.kind {
			t.Errorf("Feedback(%d, %d) = %v, want kind %v", tc.id, tc.rank, err, tc.kind)
		}
	}
	if err := st.FeedbackNone(ctx, id1, "none", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := st.Feedback(ctx, id1, 2, true, "useful after all", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := st.FeedbackNone(ctx, id1, "", time.Time{}); errs.KindOf(err) != errs.Conflict {
		t.Errorf("--none after useful = %v, want a Conflict", err)
	}
	if err := st.FeedbackNone(ctx, id2, "", time.Time{}); err != nil {
		t.Errorf("--none on a zero-result search = %v", err)
	}
	if err := st.FeedbackNone(ctx, id2+100, "", time.Time{}); errs.KindOf(err) != errs.NotFound {
		t.Errorf("--none on a missing search = %v, want NotFound", err)
	}
	now := time.Now()
	for _, tc := range []struct {
		path string
		want bool
	}{{"a.md", true}, {"a.md", false}, {"c.md", false}} {
		if got, err := st.RecordOpen(ctx, store.OpenRecord{EntryPath: tc.path, At: now, Window: openWindow}); err != nil || got != tc.want {
			t.Errorf("RecordOpen(%s) = %v, %v; want %v (c.md was only a candidate)", tc.path, got, err, tc.want)
		}
	}
	if got, err := st.RecordOpen(ctx, store.OpenRecord{EntryPath: "b.md", At: now.Add(openWindow + time.Minute), Window: openWindow}); err != nil || got {
		t.Errorf("RecordOpen outside the window = %v, %v", got, err)
	}
	// b.md was returned by this store, not by another one: an open of another store's b.md does
	// not attach to it.
	if got, err := st.RecordOpen(ctx, store.OpenRecord{Store: "notes@/elsewhere", EntryPath: "b.md", At: now, Window: openWindow}); err != nil || got {
		t.Errorf("RecordOpen(notes@/elsewhere b.md) = %v, %v; want false (only this store returned b.md)", got, err)
	}
}

// searchLogRoundTrip: what LogSearch writes is what the log holds, read back with QueryLog in
// plain SQL: the settings, their hash and the FTS expression (NULL when not recorded), the
// caller's timestamp, and each hit's store (NULL for this store's own hits). Two stores may hand
// out the same chunk id, so returned is decided per (store, chunk id).
func searchLogRoundTrip(t *testing.T, st store.Store) {
	at := time.Date(2026, 10, 30, 9, 15, 42, 0, time.FixedZone("x", 3600))
	other := "notes@/Users/someone/knowledge"
	own := store.LoggedHit{ChunkID: 7, EntryPath: "a.md", Heading: "H", RRFScore: 0.03}
	theirs := store.LoggedHit{Store: other, ChunkID: 7, EntryPath: "a.md", RRFScore: 0.02}
	dropped := store.LoggedHit{Store: other, ChunkID: 8, EntryPath: "b.md", RRFScore: 0.01}
	rec := store.SearchRecord{TS: at, Query: "q", Mode: "hybrid", K: 5, Caller: "c",
		Settings: `{"search":{"k":5}}`, SettingsHash: "abc123", FTSMatch: `"q"`,
		Results:    []store.LoggedHit{own, theirs},
		Candidates: []store.LoggedHit{own, theirs, dropped}}
	id, err := st.LogSearch(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	bare, err := st.LogSearch(ctx, store.SearchRecord{TS: at, Query: "bare", Mode: "fts", K: 5, Caller: "c"})
	if err != nil {
		t.Fatal(err)
	}
	tab, err := st.QueryLog(ctx, "SELECT id, ts, settings, settings_hash, fts_query FROM searches WHERE id IN (?, ?) ORDER BY id", id, bare)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]any{{id, "2026-10-30T08:15:42Z", rec.Settings, "abc123", `"q"`}, {bare, "2026-10-30T08:15:42Z", nil, nil, nil}}
	if fmt.Sprint(tab.Rows) != fmt.Sprint(want) {
		t.Errorf("searches = %v, want %v", tab.Rows, want)
	}
	tab, err = st.QueryLog(ctx, "SELECT rank, store, chunk_id FROM search_results WHERE search_id = ? ORDER BY rank", id)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(tab.Rows), fmt.Sprint([][]any{{int64(1), nil, int64(7)}, {int64(2), other, int64(7)}}); got != want {
		t.Errorf("search_results = %s, want %s", got, want)
	}
	tab, err = st.QueryLog(ctx, "SELECT fused_rank, store, chunk_id, returned FROM search_candidates WHERE search_id = ? ORDER BY fused_rank", id)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(tab.Rows), fmt.Sprint([][]any{{int64(1), nil, int64(7), int64(1)}, {int64(2), other, int64(7), int64(1)}, {int64(3), other, int64(8), int64(0)}}); got != want {
		t.Errorf("search_candidates = %s, want %s", got, want)
	}
	// An open of the other store's a.md attaches to this search; so does this store's own a.md:
	// one row per (store, path), with the store (NULL = this one). A repeat is already on record.
	now := at.Add(time.Minute)
	for _, tc := range []struct {
		store, path string
		want        bool
	}{
		{other, "a.md", true},
		{"", "a.md", true},
		{other, "a.md", false},
		{"", "a.md", false},
		{other, "b.md", false}, // only a candidate
	} {
		if got, err := st.RecordOpen(ctx, store.OpenRecord{Store: tc.store, EntryPath: tc.path, At: now, Window: openWindow}); err != nil || got != tc.want {
			t.Errorf("RecordOpen(%q %s) = %v, %v; want %v", tc.store, tc.path, got, err, tc.want)
		}
	}
	tab, err = st.QueryLog(ctx, "SELECT store, entry_path FROM search_opens WHERE search_id = ? ORDER BY rowid", id)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(tab.Rows), fmt.Sprint([][]any{{other, "a.md"}, {nil, "a.md"}}); got != want {
		t.Errorf("search_opens = %s, want %s", got, want)
	}
	exactRoundTrip(t, st, at)
	priorRoundTrip(t, st, at)
	tierMemoryRoundTrip(t, st, at)
}

// tierMemoryRoundTrip: a hit's tier in the main FTS list (PLAN 4.6) and its place in the
// query-memory list (Q135) are logged on the result and candidate rows, NULL when absent.
func tierMemoryRoundTrip(t *testing.T, st store.Store, at time.Time) {
	rank, u := 2, 0.625
	full := store.LoggedHit{ChunkID: 3, EntryPath: "x.md", RRFScore: 0.02, FTSTier: store.TierNear, MemoryRank: &rank, MemoryScore: &u}
	bare := store.LoggedHit{ChunkID: 4, EntryPath: "y.md", RRFScore: 0.01}
	id, err := st.LogSearch(ctx, store.SearchRecord{TS: at, Query: "tier memory", Mode: "fts", K: 5, Caller: "c",
		Results: []store.LoggedHit{full}, Candidates: []store.LoggedHit{full, bare}})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		want [][]any
	}{
		{"SELECT fts_tier, memory_rank, memory_score FROM search_results WHERE search_id = ? ORDER BY rank", [][]any{{"near", int64(2), 0.625}}},
		{"SELECT fts_tier, memory_rank, memory_score FROM search_candidates WHERE search_id = ? ORDER BY fused_rank", [][]any{{"near", int64(2), 0.625}, {nil, nil, nil}}},
	} {
		tab, err := st.QueryLog(ctx, q.sql, id)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := fmt.Sprint(tab.Rows), fmt.Sprint(q.want); got != want {
			t.Errorf("%s = %s, want %s", q.sql, got, want)
		}
	}
}

// exactRoundTrip: a hit's place in the exact list is logged with it, NULL when it was not there.
func exactRoundTrip(t *testing.T, st store.Store, at time.Time) {
	rank, score := 1, -2.5
	hit := store.LoggedHit{ChunkID: 3, EntryPath: "x.md", RRFScore: 0.02, ExactRank: &rank, ExactScore: &score}
	plain := store.LoggedHit{ChunkID: 4, EntryPath: "y.md", RRFScore: 0.01}
	id, err := st.LogSearch(ctx, store.SearchRecord{TS: at, Query: "foo_bar", Mode: "fts", K: 5, Caller: "c",
		Results: []store.LoggedHit{hit}, Candidates: []store.LoggedHit{hit, plain}})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		want [][]any
	}{
		{"SELECT exact_rank, exact_score FROM search_results WHERE search_id = ? ORDER BY rank", [][]any{{int64(1), -2.5}}},
		{"SELECT exact_rank, exact_score FROM search_candidates WHERE search_id = ? ORDER BY fused_rank", [][]any{{int64(1), -2.5}, {nil, nil}}},
	} {
		tab, err := st.QueryLog(ctx, q.sql, id)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := fmt.Sprint(tab.Rows), fmt.Sprint(q.want); got != want {
			t.Errorf("%s = %s, want %s", q.sql, got, want)
		}
	}
}

// priorRoundTrip: the feedback prior's multiplier is logged on the result and candidate rows, NULL
// (not 1) when the prior was inactive for the hit.
func priorRoundTrip(t *testing.T, st store.Store, at time.Time) {
	mult := 1.125
	active := store.LoggedHit{ChunkID: 3, EntryPath: "x.md", RRFScore: 0.02, Prior: &mult}
	inactive := store.LoggedHit{ChunkID: 4, EntryPath: "y.md", RRFScore: 0.01}
	id, err := st.LogSearch(ctx, store.SearchRecord{TS: at, Query: "prior", Mode: "fts", K: 5, Caller: "c",
		Results: []store.LoggedHit{active}, Candidates: []store.LoggedHit{active, inactive}})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		want [][]any
	}{
		{"SELECT prior FROM search_results WHERE search_id = ? ORDER BY rank", [][]any{{1.125}}},
		{"SELECT prior FROM search_candidates WHERE search_id = ? ORDER BY fused_rank", [][]any{{1.125}, {nil}}},
	} {
		tab, err := st.QueryLog(ctx, q.sql, id)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := fmt.Sprint(tab.Rows), fmt.Sprint(q.want); got != want {
			t.Errorf("%s = %s, want %s", q.sql, got, want)
		}
	}
}

func entriesByPath(t *testing.T, st store.Store, dim int) {
	mustReplace(t, st, entryIn("a.md"), chunks(dim, "alpha"))
	dated := entryIn("d.md")
	dated.Updated, dated.Summary = "2026-01-02", "dated summary"
	mustReplace(t, st, dated, chunks(dim, "delta"))
	got, err := st.EntriesByPath(ctx, []string{"d.md", "missing.md", "a.md", "d.md"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["a.md"].Title != "Title a.md" || got["d.md"].Updated != "2026-01-02" || got["d.md"].Summary != "dated summary" {
		t.Errorf("EntriesByPath = %+v; want a.md and d.md only", got)
	}
	if _, ok := got["missing.md"]; ok {
		t.Error("a missing path is in the map")
	}
	if empty, err := st.EntriesByPath(ctx, nil); err != nil || len(empty) != 0 {
		t.Errorf("EntriesByPath(nil) = %v, %v; want an empty map", empty, err)
	}
	if one, err := st.EntriesByPath(ctx, []string{"a.md"}); err != nil || len(one) != 1 || one["a.md"].Path != "a.md" {
		t.Errorf("EntriesByPath(one path) = %v, %v; want a.md", one, err)
	}
}

// judgedLog is the read side of the log (kb eval), for a backend that has one. A cross-store
// search logs other stores' hits: each verdict carries its hit's store, and each open the store it
// was recorded with, so one path opened from two stores is in Opens and OtherOpens (Q77, Q82).
// Since is inclusive at the log's one-second resolution.
func judgedLog(t *testing.T, st store.Store) {
	jl, ok := st.(store.JudgedLog)
	if !ok {
		t.Skip("no store.JudgedLog")
	}
	const notes, adr = "default@/notes", "adr@/loc"
	hit := func(ref, path string) store.LoggedHit {
		return store.LoggedHit{Store: ref, ChunkID: 7, EntryPath: path, Heading: "H", RRFScore: 1}
	}
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	if _, err := st.LogSearch(ctx, store.SearchRecord{TS: at, Query: "q", Mode: "fts", K: 8, Caller: "c", Results: []store.LoggedHit{
		hit("", "a.md"), hit(notes, "a.md"), hit(notes, "n.md"), hit(adr, "n.md"), hit(notes, "only.md"),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Feedback(ctx, 1, 2, true, "", at); err != nil { // notes' a.md, not this store's
		t.Fatal(err)
	}
	if err := st.Feedback(ctx, 1, 5, false, "", at); err != nil {
		t.Fatal(err)
	}
	for _, o := range []store.OpenRecord{
		{Store: notes, EntryPath: "a.md"}, // this store returned a.md too: both opens count
		{EntryPath: "a.md"},
		{Store: adr, EntryPath: "n.md"}, // notes returned n.md too, but adr's was opened
		{Store: notes, EntryPath: "only.md"},
	} {
		o.At, o.Window = at, time.Hour
		if ok, err := st.RecordOpen(ctx, o); err != nil || !ok {
			t.Fatalf("RecordOpen %+v: %v %v", o, ok, err)
		}
	}
	got, err := jl.Judged(ctx, store.JudgedFilter{})
	if err != nil {
		t.Fatal(err)
	}
	want := []store.JudgedSearch{{ID: 1, TS: "2026-10-30T12:00:00Z", Query: "q", Mode: "fts", K: 8, Caller: "c",
		Verdicts: []store.Verdict{
			{Rank: 2, Store: notes, Useful: true, At: at, EntryPath: "a.md", Heading: "H"},
			{Rank: 5, Store: notes, At: at, EntryPath: "only.md", Heading: "H"},
		},
		Opens:      []string{"a.md"},
		OpenedAt:   map[string]time.Time{"a.md": at},
		OtherOpens: []store.StorePath{{Store: adr, Path: "n.md"}, {Store: notes, Path: "a.md"}, {Store: notes, Path: "only.md"}},
	}}
	if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
		t.Errorf("Judged:\n got %+v\nwant %+v", got, want)
	}
	for _, tc := range []struct {
		since time.Time
		n     int
	}{
		{at.Add(-time.Second), 1},
		{at, 1},
		{at.Add(time.Second), 0},
		{at.In(time.FixedZone("x", 3600)), 1}, // the same instant in another zone
	} {
		got, err := jl.Judged(ctx, store.JudgedFilter{Since: tc.since})
		if err != nil || len(got) != tc.n {
			t.Errorf("since %v: %d searches (%v), want %d", tc.since, len(got), err, tc.n)
		}
	}
}
