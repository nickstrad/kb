// Package storetest is the contract every storage backend must meet (docs/TESTING.md §6). A
// backend's tests call Run with a Factory; behaviour specific to one backend (PRAGMAs,
// migrations, concurrent Open) stays in that backend's own tests.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
			t.Run("KNN: top-k ordered by distance, tag filter", func(t *testing.T) { knn(t, newStore(t, dim), dim) })
			t.Run("ResetIndex empties the index, keeps the log", func(t *testing.T) { reset(t, newStore(t, dim), dim) })
			t.Run("SearchLog: log, feedback, LastSearch, RecordOpen", func(t *testing.T) { searchLog(t, newStore(t, dim), dim) })
			t.Run("model test (seeded ops)", func(t *testing.T) {
				for _, seed := range ModelSeeds {
					RunModel(t, newStore(t, dim), dim, seed)
				}
			})
		})
	}
}

var ctx = context.Background()

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
	res, err := st.FTS(ctx, store.FTSQuery{Terms: terms, Op: op, Tag: tag, Limit: 20})
	if err != nil {
		t.Fatalf("FTS %v: %v", terms, err)
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
	meta, err := st.ChunkMeta(ctx, append(chunkIDs(t, st, id), 999999))
	if err != nil || len(meta) != 2 || meta[cs[1].ID] != (store.ChunkMeta{EntryPath: "a.md", Heading: "h1", Text: "beta zebracorn"}) {
		t.Errorf("ChunkMeta = %+v, %v; want the two chunks, the dangling id absent", meta, err)
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
	mustReplace(t, st, entryIn("a.md"), chunks(dim, "resetword"))
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
		if err := st.Feedback(ctx, tc.id, tc.rank, true, ""); errs.KindOf(err) != tc.kind {
			t.Errorf("Feedback(%d, %d) = %v, want kind %v", tc.id, tc.rank, err, tc.kind)
		}
	}
	if err := st.FeedbackNone(ctx, id1, "none"); err != nil {
		t.Fatal(err)
	}
	if err := st.Feedback(ctx, id1, 2, true, "useful after all"); err != nil {
		t.Fatal(err)
	}
	if err := st.FeedbackNone(ctx, id1, ""); errs.KindOf(err) != errs.Conflict {
		t.Errorf("--none after useful = %v, want a Conflict", err)
	}
	if err := st.FeedbackNone(ctx, id2, ""); err != nil {
		t.Errorf("--none on a zero-result search = %v", err)
	}
	if err := st.FeedbackNone(ctx, id2+100, ""); errs.KindOf(err) != errs.NotFound {
		t.Errorf("--none on a missing search = %v, want NotFound", err)
	}
	now := time.Now()
	for _, tc := range []struct {
		path string
		want bool
	}{{"a.md", true}, {"a.md", false}, {"c.md", false}} {
		if got, err := st.RecordOpen(ctx, tc.path, now, store.OpenWindow); err != nil || got != tc.want {
			t.Errorf("RecordOpen(%s) = %v, %v; want %v (c.md was only a candidate)", tc.path, got, err, tc.want)
		}
	}
	if got, err := st.RecordOpen(ctx, "b.md", now.Add(store.OpenWindow+time.Minute), store.OpenWindow); err != nil || got {
		t.Errorf("RecordOpen outside the window = %v, %v", got, err)
	}
}
