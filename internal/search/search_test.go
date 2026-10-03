package search

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"knowledge/kb/internal/embed"
	"knowledge/kb/internal/embed/fake"
	"knowledge/kb/internal/store"
)

// TestClampK pins the documented 1..20 range, including the explicit -k 0 case.
func TestClampK(t *testing.T) {
	for _, tc := range []struct{ in, want int }{
		{-5, MinK}, {0, MinK}, {1, 1}, {8, 8}, {20, MaxK}, {21, MaxK}, {1000, MaxK},
	} {
		if got := ClampK(tc.in); got != tc.want {
			t.Errorf("ClampK(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestResultHitsNeverNull is the --json contract: an empty result serialises hits as [], not null.
func TestResultHitsNeverNull(t *testing.T) {
	data, err := json.Marshal(NewResult("anything", ModeHybrid))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"search_id":0,"query":"anything","mode":"hybrid","hits":[]}`
	if string(data) != want {
		t.Errorf("json = %s, want %s", data, want)
	}
}

// TestSanitizeFTS covers the queries from the plan's validation list plus the syntax characters
// that make raw FTS5 throw: quotes, colons, parentheses, a leading dash, and the operator words.
func TestSanitizeFTS(t *testing.T) {
	for _, tc := range []struct {
		name, in, wantAnd, wantOr string
	}{
		{
			name:    "plain sentence",
			in:      "daemon dies when I log out of ssh",
			wantAnd: `"daemon" AND "dies" AND "when" AND "I" AND "log" AND "out" AND "of" AND "ssh"`,
			wantOr:  `"daemon" OR "dies" OR "when" OR "I" OR "log" OR "out" OR "of" OR "ssh"`,
		},
		{
			name:    "leading dash flag",
			in:      "set -e loop counter exits",
			wantAnd: `"set" AND "-e" AND "loop" AND "counter" AND "exits"`,
			wantOr:  `"set" OR "-e" OR "loop" OR "counter" OR "exits"`,
		},
		{
			name:    "embedded quotes and a bare NOT",
			in:      `grpcurl says "server does not support reflection"`,
			wantAnd: `"grpcurl" AND "says" AND "server" AND "does" AND "support" AND "reflection"`,
			wantOr:  `"grpcurl" OR "says" OR "server" OR "does" OR "support" OR "reflection"`,
		},
		{
			name:    "colon and parentheses",
			in:      "foo:bar (baz)",
			wantAnd: `"foo:bar" AND "(baz)"`,
			wantOr:  `"foo:bar" OR "(baz)"`,
		},
		{
			name:    "hyphenated name stays one phrase",
			in:      "draw-visual go-build",
			wantAnd: `"draw-visual" AND "go-build"`,
			wantOr:  `"draw-visual" OR "go-build"`,
		},
		{
			name:    "punctuation-only fields are dropped, not left as empty phrases",
			in:      "quokka ... && — widget",
			wantAnd: `"quokka" AND "widget"`,
			wantOr:  `"quokka" OR "widget"`,
		},
		{
			name:    "letters SQLite does not tokenize are dropped like punctuation",
			in:      "quokka ᳳ ᦳ",
			wantAnd: `"quokka"`,
			wantOr:  `"quokka"`,
		},
		{
			name:    "NUL byte breaks the word",
			in:      "a\x00b",
			wantAnd: `"a b"`,
			wantOr:  `"a b"`,
		},
		{
			name:    "operator word inside a hyphenated name is kept",
			in:      "not-found",
			wantAnd: `"not-found"`,
			wantOr:  `"not-found"`,
		},
		{
			name:    "operator word alone",
			in:      "NOT a",
			wantAnd: `"a"`,
			wantOr:  `"a"`,
		},
		{
			name:    "every operator word",
			in:      "and OR Not near",
			wantAnd: "",
			wantOr:  "",
		},
		{
			name:    "empty",
			in:      "",
			wantAnd: "",
			wantOr:  "",
		},
		{
			name:    "whitespace only",
			in:      "   \t\n  ",
			wantAnd: "",
			wantOr:  "",
		},
		{
			name:    "only punctuation",
			in:      `"" ** (( )) :: ^^ -- ++ {}`,
			wantAnd: "",
			wantOr:  "",
		},
		{
			name:    "prefix star stripped",
			in:      "post*gres",
			wantAnd: `"postgres"`,
			wantOr:  `"postgres"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotAnd, gotOr := SanitizeFTS(tc.in)
			if gotAnd != tc.wantAnd {
				t.Errorf("and = %s, want %s", gotAnd, tc.wantAnd)
			}
			if gotOr != tc.wantOr {
				t.Errorf("or = %s, want %s", gotOr, tc.wantOr)
			}
		})
	}
}

// TestSanitizeFTSAcceptedBySQLite is the point of the sanitiser: the output has to parse. Every
// raw query above goes through a real FTS5 MATCH.
func TestSanitizeFTSAcceptedBySQLite(t *testing.T) {
	s, _ := testSearcher(t)
	for _, raw := range []string{
		"daemon dies when I log out of ssh",
		"set -e loop counter exits",
		`grpcurl says "server does not support reflection"`,
		"foo:bar (baz)",
		"draw-visual not-found",
		"quokka ... && — widget",
		"a\x00b c",
		"NOT a",
		"widget^2",
		"a AND b NEAR c",
	} {
		andQuery, orQuery := SanitizeFTS(raw)
		if andQuery == "" {
			continue
		}
		for _, q := range []string{andQuery, orQuery} {
			if _, err := s.runFTS(context.Background(),
				"SELECT rowid, bm25(chunks_fts, 1.0, 2.0, 3.0) AS s FROM chunks_fts WHERE chunks_fts MATCH ? ORDER BY s LIMIT 20",
				[]any{q}); err != nil {
				t.Errorf("query %q sanitised to %s: %v", raw, q, err)
			}
		}
	}
}

// TestFuse covers the RRF arithmetic, the deterministic tie-break and the degenerate lists.
func TestFuse(t *testing.T) {
	fts := func(ids ...int64) []FTSRow {
		out := make([]FTSRow, len(ids))
		for i, id := range ids {
			out[i] = FTSRow{ChunkID: id, Score: -float64(10 - i)}
		}
		return out
	}
	vec := func(ids ...int64) []VecRow {
		out := make([]VecRow, len(ids))
		for i, id := range ids {
			out[i] = VecRow{ChunkID: id, Distance: float64(i) / 10}
		}
		return out
	}

	t.Run("both lists beat one list", func(t *testing.T) {
		got := Fuse(fts(1, 2), vec(1, 3))
		wantOrder := []int64{1, 2, 3}
		assertOrder(t, got, wantOrder)
		if want := 1/float64(61) + 1/float64(61); !almostEqual(got[0].RRFScore, want) {
			t.Errorf("chunk 1 score = %v, want %v", got[0].RRFScore, want)
		}
		if got[0].FTSRank == nil || *got[0].FTSRank != 1 {
			t.Errorf("chunk 1 fts rank = %v, want 1", got[0].FTSRank)
		}
		if got[0].VecRank == nil || *got[0].VecRank != 1 {
			t.Errorf("chunk 1 vec rank = %v, want 1", got[0].VecRank)
		}
		if got[0].FTSScore == nil || *got[0].FTSScore != -10 {
			t.Errorf("chunk 1 fts score = %v, want -10", got[0].FTSScore)
		}
		if got[0].VecDistance == nil || *got[0].VecDistance != 0 {
			t.Errorf("chunk 1 vec distance = %v, want 0", got[0].VecDistance)
		}
	})

	t.Run("tie broken by fts rank then vec rank then id", func(t *testing.T) {
		// 7 is fts#1/vec#2, 8 is fts#2/vec#1: identical scores, 7 wins on fts rank.
		got := Fuse(fts(7, 8), vec(8, 7))
		assertOrder(t, got, []int64{7, 8})
		if !almostEqual(got[0].RRFScore, got[1].RRFScore) {
			t.Fatalf("expected tied scores, got %v and %v", got[0].RRFScore, got[1].RRFScore)
		}

		// Two chunks seen only by vec at the same rank cannot happen, but two chunks with no
		// fts rank and distinct vec ranks must still order by vec rank.
		got = Fuse(nil, vec(9, 4))
		assertOrder(t, got, []int64{9, 4})

		// Equal scores and no ranks to separate them falls through to the chunk id.
		got = Fuse(fts(5), fts5ToVec(fts(3)))
		if !almostEqual(got[0].RRFScore, got[1].RRFScore) {
			t.Fatalf("expected tied scores, got %v and %v", got[0].RRFScore, got[1].RRFScore)
		}
		assertOrder(t, got, []int64{5, 3}) // 5 has an fts rank, 3 only a vec rank
	})

	t.Run("vec only", func(t *testing.T) {
		got := Fuse(nil, vec(42))
		assertOrder(t, got, []int64{42})
		if got[0].FTSRank != nil || got[0].FTSScore != nil {
			t.Errorf("fts fields = %v/%v, want nil", got[0].FTSRank, got[0].FTSScore)
		}
		if got[0].VecRank == nil || *got[0].VecRank != 1 {
			t.Errorf("vec rank = %v, want 1", got[0].VecRank)
		}
		if want := 1 / float64(61); !almostEqual(got[0].RRFScore, want) {
			t.Errorf("score = %v, want %v", got[0].RRFScore, want)
		}
	})

	t.Run("fts only", func(t *testing.T) {
		got := Fuse(fts(11), nil)
		assertOrder(t, got, []int64{11})
		if got[0].VecRank != nil || got[0].VecDistance != nil {
			t.Errorf("vec fields = %v/%v, want nil", got[0].VecRank, got[0].VecDistance)
		}
	})

	t.Run("empty", func(t *testing.T) {
		if got := Fuse(nil, nil); len(got) != 0 {
			t.Errorf("Fuse(nil, nil) = %v, want empty", got)
		}
	})

	t.Run("truncated to CandidateLimit", func(t *testing.T) {
		var f []FTSRow
		var v []VecRow
		for i := int64(1); i <= 20; i++ {
			f = append(f, FTSRow{ChunkID: i})
			v = append(v, VecRow{ChunkID: 100 + i})
		}
		f = append(f, FTSRow{ChunkID: 999}) // a 21st, as if the limit had not been applied
		if got := Fuse(f, v); len(got) != CandidateLimit {
			t.Errorf("len = %d, want %d", len(got), CandidateLimit)
		}
	})
}

// fts5ToVec reuses an fts list as a vec list so a test can force a score tie between two chunks
// that share no list.
func fts5ToVec(rows []FTSRow) []VecRow {
	out := make([]VecRow, len(rows))
	for i, r := range rows {
		out[i] = VecRow{ChunkID: r.ChunkID}
	}
	return out
}

func assertOrder(t *testing.T, got []Candidate, want []int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d candidates, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ChunkID != want[i] {
			t.Fatalf("candidate %d = chunk %d, want %d (full order %v)", i, got[i].ChunkID, want[i], chunkIDs(got))
		}
	}
}

func chunkIDs(cands []Candidate) []int64 {
	out := make([]int64, len(cands))
	for i, c := range cands {
		out[i] = c.ChunkID
	}
	return out
}

func almostEqual(a, b float64) bool {
	d := a - b
	return d < 1e-12 && d > -1e-12
}

// TestApplyCap checks the diversity cap in isolation: at most PerEntryCap per entry, at most k in
// total, order preserved, and unknown chunks skipped.
func TestApplyCap(t *testing.T) {
	cands := []Candidate{
		{ChunkID: 1}, {ChunkID: 2}, {ChunkID: 3}, {ChunkID: 4}, {ChunkID: 5}, {ChunkID: 6},
	}
	entryOf := map[int64]string{1: "a.md", 2: "a.md", 3: "a.md", 4: "a.md", 5: "b.md"}

	got := applyCap(cands, entryOf, 8)
	if want := []int64{1, 2, 3, 5}; !equalIDs(chunkIDs(got), want) {
		t.Errorf("applyCap = %v, want %v (chunk 4 capped, chunk 6 unknown)", chunkIDs(got), want)
	}
	if got := applyCap(cands, entryOf, 2); len(got) != 2 {
		t.Errorf("applyCap with k=2 returned %d, want 2", len(got))
	}
	if got := applyCap(nil, entryOf, 5); len(got) != 0 {
		t.Errorf("applyCap(nil) = %v, want empty", got)
	}
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- integration tests against a real database ------------------------------------------------

// prefixBlindEmbedder is the fake embedder with the nomic prefixes removed before hashing, so a
// query whose text equals a chunk's text produces exactly that chunk's stored vector. The real
// model is asymmetric and cannot be asserted on that way; this keeps the vec-list test about
// plumbing (serialisation, k, ordering) rather than about relevance.
type prefixBlindEmbedder struct{ *fake.Embedder }

func (e prefixBlindEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	stripped := make([]string, len(texts))
	for i, t := range texts {
		t = strings.TrimPrefix(t, embed.DocPrefix)
		stripped[i] = strings.TrimPrefix(t, embed.QueryPrefix)
	}
	return e.Embedder.Embed(ctx, stripped)
}

// testEntry is the fixture shape: one entry and the chunk texts to index for it.
type testEntry struct {
	path    string
	title   string
	tags    []string
	chunks  []string // heading is derived: "" for the first, "section N" after
	entryID int64
}

// fixtures are three entries. alpha has five chunks that all match "zebracorn" (so the diversity
// cap has something to cap), beta owns the one distinctive word "quokka", and every chunk in
// every entry contains "widget" so a tag filter has something to narrow.
func fixtures() []testEntry {
	return []testEntry{
		{
			path:  "alpha.md",
			title: "Alpha Entry",
			tags:  []string{"linux", "tmux"},
			chunks: []string{
				"Alpha Entry: a widget about zebracorn herding on this droplet",
				"zebracorn widget grooming schedules and the morning routine",
				"zebracorn widget stabling, bedding and the winter feed plan",
				"zebracorn widget transport permits and the paperwork involved",
				"zebracorn widget veterinary checks, hooves first, then the horn",
			},
		},
		{
			path:  "beta.md",
			title: "Beta Entry",
			tags:  []string{"postgres"},
			chunks: []string{
				"Beta Entry: a widget about the marsupial enclosure",
				"the quokka widget enclosure needs a shade cloth by midday",
				"widget fencing heights and the gate latch that keeps failing",
			},
		},
		{
			path:  "gamma/README.md",
			title: "Gamma Entry",
			tags:  []string{"docker"},
			chunks: []string{
				"Gamma Entry: a widget about container plumbing",
				"widget bridge networks and why the default one has no DNS",
				"widget volume mounts and the ownership surprise on bind mounts",
				"widget image layers and how the build cache decides to reuse one",
			},
		},
	}
}

// testSearcher builds a temp database, indexes the fixtures with the prefix-blind fake embedder,
// and returns a Searcher wired to it plus the fixtures with their entry ids filled in.
func testSearcher(t testing.TB) (*Searcher, []testEntry) {
	t.Helper()

	emb := prefixBlindEmbedder{fake.New("fake-embed", 768)}
	st, err := store.Open(filepath.Join(t.TempDir(), "kb.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.EnsureEmbedMeta(emb.Model(), emb.Dim()); err != nil {
		t.Fatalf("embed meta: %v", err)
	}

	ctx := context.Background()
	entries := fixtures()
	for i := range entries {
		e := &entries[i]
		texts := make([]string, len(e.chunks))
		for j, text := range e.chunks {
			texts[j] = embed.DocPrefix + text
		}
		vecs, err := emb.Embed(ctx, texts)
		if err != nil {
			t.Fatalf("embed %s: %v", e.path, err)
		}
		chunks := make([]store.ChunkInput, len(e.chunks))
		for j, text := range e.chunks {
			heading := ""
			if j > 0 {
				heading = fmt.Sprintf("section %d", j)
			}
			sum := sha256.Sum256([]byte(text))
			chunks[j] = store.ChunkInput{
				Ord:        j,
				SourceFile: e.path,
				Heading:    heading,
				Text:       text,
				TextHash:   hex.EncodeToString(sum[:]),
				Embedding:  vecs[j],
			}
		}
		id, err := st.ReplaceEntryChunks(ctx, store.EntryInput{
			Path:     e.path,
			Kind:     kindOf(e.path),
			Title:    e.title,
			Summary:  "fixture",
			Tags:     e.tags,
			BodyHash: "hash-" + e.path,
		}, chunks)
		if err != nil {
			t.Fatalf("index %s: %v", e.path, err)
		}
		e.entryID = id
	}

	return &Searcher{DB: st.DB(), Embedder: emb}, entries
}

func kindOf(path string) string {
	if strings.Contains(path, "/") {
		return "dir"
	}
	return "file"
}

// TestSearchFTSDistinctiveWord: a word that occurs in exactly one chunk puts that chunk at rank 1.
func TestSearchFTSDistinctiveWord(t *testing.T) {
	s, _ := testSearcher(t)
	out, err := s.Search(context.Background(), Request{Query: "quokka", Mode: ModeFTS, K: 8})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if out.Mode != ModeFTS {
		t.Errorf("mode = %s, want %s", out.Mode, ModeFTS)
	}
	if len(out.Result.Hits) == 0 {
		t.Fatal("no hits")
	}
	hit := out.Result.Hits[0]
	if hit.Rank != 1 {
		t.Errorf("rank = %d, want 1", hit.Rank)
	}
	if hit.Path != "beta.md" || !strings.Contains(hit.Text, "quokka") {
		t.Errorf("hit 1 = %s %q, want the beta.md quokka chunk", hit.Path, hit.Text)
	}
	if hit.FTSRank == nil || *hit.FTSRank != 1 {
		t.Errorf("fts_rank = %v, want 1", hit.FTSRank)
	}
	if hit.VecRank != nil {
		t.Errorf("vec_rank = %v, want nil in fts mode", *hit.VecRank)
	}
	if out.NVec != 0 || out.EmbedMs != 0 || out.EmbedModel != "" {
		t.Errorf("fts mode touched the embedder: n_vec=%d embed_ms=%d model=%q", out.NVec, out.EmbedMs, out.EmbedModel)
	}
}

// TestSearchVecOnly: with the prefix-blind fake, embedding a chunk's own text as the query lands
// exactly on its stored vector, so it comes back first at distance ~0.
func TestSearchVecOnly(t *testing.T) {
	s, entries := testSearcher(t)
	want := entries[2].chunks[2] // "widget volume mounts ..."

	out, err := s.Search(context.Background(), Request{Query: want, Mode: ModeVec, K: 5})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if out.Mode != ModeVec {
		t.Errorf("mode = %s, want %s", out.Mode, ModeVec)
	}
	if out.NFTS != 0 {
		t.Errorf("n_fts = %d, want 0 in vec mode", out.NFTS)
	}
	if out.NVec == 0 {
		t.Fatal("vec list is empty")
	}
	if len(out.Result.Hits) == 0 {
		t.Fatal("no hits")
	}
	hit := out.Result.Hits[0]
	if hit.Text != want {
		t.Errorf("hit 1 text = %q, want %q", hit.Text, want)
	}
	if hit.Path != "gamma/README.md" {
		t.Errorf("hit 1 path = %s, want gamma/README.md", hit.Path)
	}
	if hit.VecRank == nil || *hit.VecRank != 1 {
		t.Errorf("vec_rank = %v, want 1", hit.VecRank)
	}
	if hit.FTSRank != nil {
		t.Errorf("fts_rank = %v, want nil in vec mode", *hit.FTSRank)
	}
	if out.Candidates[0].VecDistance == nil || *out.Candidates[0].VecDistance > 1e-4 {
		t.Errorf("distance = %v, want ~0 for an exact vector match", out.Candidates[0].VecDistance)
	}
	if out.EmbedModel != "fake-embed" {
		t.Errorf("embed_model = %q, want fake-embed", out.EmbedModel)
	}
}

// TestSearchHybrid runs both lists and checks a chunk found by both carries both ranks.
func TestSearchHybrid(t *testing.T) {
	s, entries := testSearcher(t)
	want := entries[1].chunks[1] // the quokka chunk: matched lexically and identical as a vector

	out, err := s.Search(context.Background(), Request{Query: want, Mode: ModeHybrid, K: 8})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if out.Mode != ModeHybrid {
		t.Errorf("mode = %s, want %s", out.Mode, ModeHybrid)
	}
	if out.NFTS == 0 || out.NVec == 0 {
		t.Fatalf("hybrid ran one list only: n_fts=%d n_vec=%d", out.NFTS, out.NVec)
	}
	hit := out.Result.Hits[0]
	if hit.Text != want {
		t.Errorf("hit 1 = %q, want %q", hit.Text, want)
	}
	if hit.FTSRank == nil || hit.VecRank == nil {
		t.Errorf("hit 1 ranks = %v/%v, want both set", hit.FTSRank, hit.VecRank)
	}
	if out.Result.Mode != ModeHybrid {
		t.Errorf("result mode = %s, want %s", out.Result.Mode, ModeHybrid)
	}
	for i, h := range out.Result.Hits {
		if h.Rank != i+1 {
			t.Errorf("hit %d has rank %d", i, h.Rank)
		}
	}
}

// TestSearchHybridFallsBackWithoutEmbedder: hybrid with no embedder is not an error, it is an FTS
// search recorded as fts-fallback.
func TestSearchHybridFallsBackWithoutEmbedder(t *testing.T) {
	s, _ := testSearcher(t)
	s.Embedder = nil

	out, err := s.Search(context.Background(), Request{Query: "quokka widget", Mode: ModeHybrid, K: 8})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if out.Mode != ModeFTSFallback {
		t.Errorf("mode = %s, want %s", out.Mode, ModeFTSFallback)
	}
	if out.Result.Mode != ModeFTSFallback {
		t.Errorf("result mode = %s, want %s", out.Result.Mode, ModeFTSFallback)
	}
	if out.FallbackErr == nil || !errors.Is(out.FallbackErr, ErrEmbedderUnavailable) {
		t.Errorf("fallback err = %v, want one wrapping ErrEmbedderUnavailable", out.FallbackErr)
	}
	if out.NVec != 0 {
		t.Errorf("n_vec = %d, want 0", out.NVec)
	}
	if len(out.Result.Hits) == 0 {
		t.Error("fallback returned no hits; the FTS list should still have run")
	}
}

// TestSearchVecWithoutEmbedderIsAnError: --mode vec has nothing to fall back to.
func TestSearchVecWithoutEmbedderIsAnError(t *testing.T) {
	s, _ := testSearcher(t)
	s.Embedder = nil

	if _, err := s.Search(context.Background(), Request{Query: "quokka", Mode: ModeVec, K: 8}); err == nil {
		t.Fatal("want an error, got nil")
	} else if !errors.Is(err, ErrEmbedderUnavailable) {
		t.Errorf("err = %v, want one wrapping ErrEmbedderUnavailable", err)
	}
}

// unavailableEmbedder fails every call with an error that only IsUnavailable can recognise.
type unavailableEmbedder struct{ *fake.Embedder }

var errServiceDown = errors.New("connection refused")

func (e unavailableEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errServiceDown
}

// TestSearchFallbackViaIsUnavailable: an embedder error that does not wrap ErrEmbedderUnavailable
// still degrades hybrid when the caller's hook recognises it (this is how the CLI maps the Ollama
// client's own ErrUnavailable).
func TestSearchFallbackViaIsUnavailable(t *testing.T) {
	s, _ := testSearcher(t)
	s.Embedder = unavailableEmbedder{fake.New("fake-embed", 768)}

	// Without the hook the error is fatal, because a refusal is not an outage.
	if _, err := s.Search(context.Background(), Request{Query: "widget", Mode: ModeHybrid, K: 4}); err == nil {
		t.Fatal("want an error without IsUnavailable, got nil")
	}

	s.IsUnavailable = func(err error) bool { return errors.Is(err, errServiceDown) }
	out, err := s.Search(context.Background(), Request{Query: "widget", Mode: ModeHybrid, K: 4})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if out.Mode != ModeFTSFallback {
		t.Errorf("mode = %s, want %s", out.Mode, ModeFTSFallback)
	}
	if !errors.Is(out.FallbackErr, errServiceDown) {
		t.Errorf("fallback err = %v, want the embedder's error", out.FallbackErr)
	}
}

// TestSearchDiversityCap: alpha has five chunks matching "zebracorn"; all five stay in the
// candidate list, only PerEntryCap of them are returned.
func TestSearchDiversityCap(t *testing.T) {
	s, _ := testSearcher(t)
	out, err := s.Search(context.Background(), Request{Query: "zebracorn", Mode: ModeFTS, K: 8})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(out.Candidates) != 5 {
		t.Errorf("candidates = %d, want 5 (the cap must not touch them)", len(out.Candidates))
	}
	if len(out.Returned) != PerEntryCap {
		t.Errorf("returned = %d, want %d", len(out.Returned), PerEntryCap)
	}
	if len(out.Result.Hits) != PerEntryCap {
		t.Errorf("hits = %d, want %d", len(out.Result.Hits), PerEntryCap)
	}
	for _, h := range out.Result.Hits {
		if h.Path != "alpha.md" {
			t.Errorf("hit path = %s, want alpha.md", h.Path)
		}
	}
}

// TestSearchTagFilter: "widget" is in every chunk of every entry, so only the tag can narrow it.
func TestSearchTagFilter(t *testing.T) {
	s, _ := testSearcher(t)
	for _, mode := range []string{ModeFTS, ModeVec, ModeHybrid} {
		t.Run(mode, func(t *testing.T) {
			out, err := s.Search(context.Background(), Request{
				Query: "widget", Mode: mode, K: 20, Tag: "postgres",
			})
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if len(out.Result.Hits) == 0 {
				t.Fatal("no hits")
			}
			for _, h := range out.Result.Hits {
				if h.Path != "beta.md" {
					t.Errorf("hit from %s, want only beta.md", h.Path)
				}
			}
			if out.TagFilter != "postgres" {
				t.Errorf("tag filter = %q, want postgres", out.TagFilter)
			}
		})
	}
}

// TestSearchTagFilterIsExact: the LIKE pattern quotes the tag, so "post" must not match
// "postgres", and it escapes LIKE's wildcards, so neither "%" (which would disable the filter
// entirely) nor "docke_" (which would match "docker") lets anything through. Every mode is
// checked because the FTS list and the vector list carry their own LIKE.
func TestSearchTagFilterIsExact(t *testing.T) {
	s, _ := testSearcher(t)
	for _, mode := range []string{ModeFTS, ModeVec, ModeHybrid} {
		for _, tag := range []string{"post", "%", "_", "docke_", `%"docker"%`, "docker%"} {
			out, err := s.Search(context.Background(), Request{
				Query: "widget", Mode: mode, K: 20, Tag: tag,
			})
			if err != nil {
				t.Fatalf("%s search with tag %q: %v", mode, tag, err)
			}
			if len(out.Result.Hits) != 0 {
				t.Errorf("%s search with tag %q returned %d hits, want 0", mode, tag, len(out.Result.Hits))
			}
		}
		// The exact tag still works, so the escaping has not broken the filter outright.
		out, err := s.Search(context.Background(), Request{Query: "widget", Mode: mode, K: 20, Tag: "docker"})
		if err != nil {
			t.Fatalf("%s search with tag docker: %v", mode, err)
		}
		if len(out.Result.Hits) == 0 {
			t.Errorf("%s search with tag docker returned nothing", mode)
		}
		for _, h := range out.Result.Hits {
			if h.Path != "gamma/README.md" {
				t.Errorf("%s search with tag docker returned %s", mode, h.Path)
			}
		}
	}
}

// TestTagPatternEscapes pins the escaping itself, so a future reader can see what the SQL sees.
func TestTagPatternEscapes(t *testing.T) {
	for _, tc := range []struct{ tag, want string }{
		{"docker", `%"docker"%`},
		{"%", `%"\%"%`},
		{"docke_", `%"docke\_"%`},
		{`a\b`, `%"a\\b"%`},
	} {
		if got := tagPattern(tc.tag); got != tc.want {
			t.Errorf("tagPattern(%q) = %q, want %q", tc.tag, got, tc.want)
		}
	}
}

// TestSearchBlankQuery: a blank query is an error in every mode. It must not reach the embedder,
// which would happily embed the bare "search_query: " prefix and hand back k arbitrary chunks for
// P3.3 to log as a real search.
func TestSearchBlankQuery(t *testing.T) {
	s, _ := testSearcher(t)
	for _, mode := range []string{ModeFTS, ModeVec, ModeHybrid, ""} {
		for _, q := range []string{"", "   ", "\t\n "} {
			out, err := s.Search(context.Background(), Request{Query: q, Mode: mode, K: 8})
			if err == nil {
				t.Errorf("mode %q query %q returned %d hits, want an error", mode, q, len(out.Result.Hits))
			}
		}
	}
}

// TestSearchUnsanitisableQueryStillRunsVec is the other half of the blank-query rule: the user did
// type something, so the vector list runs even though no FTS token survives.
func TestSearchUnsanitisableQueryStillRunsVec(t *testing.T) {
	s, _ := testSearcher(t)
	out, err := s.Search(context.Background(), Request{Query: `(((  )))`, Mode: ModeHybrid, K: 8})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if out.NFTS != 0 {
		t.Errorf("n_fts = %d, want 0 (nothing survives the sanitiser)", out.NFTS)
	}
	if out.NVec == 0 {
		t.Error("n_vec = 0, want the vector list to have run anyway")
	}
	if out.Mode != ModeHybrid {
		t.Errorf("mode = %s, want %s", out.Mode, ModeHybrid)
	}
}

// TestBaseRuneMatchesSQLite checks ftsBaseRune against the linked SQLite for every assigned code
// point it accepts: each one, alone in a document, must give the unicode61 tokenizer at least one
// token. A failure lists code points to add to ftsSkewNotToken, typically after a SQLite upgrade.
func TestBaseRuneMatchesSQLite(t *testing.T) {
	s, _ := testSearcher(t)
	ctx := context.Background()
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		t.Fatalf("dedicated connection: %v", err)
	}
	defer conn.Close()
	for _, stmt := range []string{
		`CREATE VIRTUAL TABLE temp.base_doc USING fts5(x, tokenize='porter unicode61')`,
		`CREATE VIRTUAL TABLE temp.base_vocab USING fts5vocab(temp, base_doc, instance)`,
		`BEGIN`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	checked := 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !utf8.ValidRune(r) || unicode.Is(unicode.Cn, r) || !ftsBaseRune(r) {
			continue
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO temp.base_doc(rowid, x) VALUES (?, ?)`, int64(r), string(r)); err != nil {
			t.Fatalf("insert U+%04X: %v", r, err)
		}
		checked++
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		t.Fatalf("commit: %v", err)
	}
	rows, err := conn.QueryContext(ctx,
		`SELECT rowid FROM temp.base_doc WHERE rowid NOT IN (SELECT doc FROM temp.base_vocab) ORDER BY rowid`)
	if err != nil {
		t.Fatalf("find untokenized: %v", err)
	}
	defer rows.Close()
	var bad []string
	for rows.Next() {
		var r int64
		if err := rows.Scan(&r); err != nil {
			t.Fatalf("scan: %v", err)
		}
		bad = append(bad, fmt.Sprintf("U+%04X", r))
	}
	if checked < 100000 {
		t.Fatalf("checked only %d code points; the enumeration is broken", checked)
	}
	if len(bad) > 0 {
		t.Errorf("ftsBaseRune accepts %d code points SQLite does not tokenize: %v", len(bad), bad)
	}
}

// TestQuotedPunctuationMatchesLikeSplitWords backs the TestSanitizeFTS expectations that keep
// punctuation inside a phrase ("foo:bar", "(baz)", "-e"): FTS5 must tokenize such a phrase into
// the same adjacent words, so each pair below has to match exactly the same rows.
func TestQuotedPunctuationMatchesLikeSplitWords(t *testing.T) {
	s, _ := testSearcher(t)
	ctx := context.Background()
	match := func(q string) []int64 {
		rows, err := s.runFTS(ctx,
			"SELECT rowid, bm25(chunks_fts, 1.0, 2.0, 3.0) AS s FROM chunks_fts WHERE chunks_fts MATCH ? ORDER BY rowid",
			[]any{q})
		if err != nil {
			t.Fatalf("match %s: %v", q, err)
		}
		ids := make([]int64, len(rows))
		for i, r := range rows {
			ids[i] = r.ChunkID
		}
		return ids
	}
	for _, pair := range [][2]string{
		{`"zebracorn-widget"`, `"zebracorn widget"`},
		{`"zebracorn:widget"`, `"zebracorn widget"`},
		{`"(quokka)"`, `"quokka"`},
		{`"-quokka"`, `"quokka"`},
		{`"Zebracorn-WIDGET"`, `"zebracorn widget"`},
	} {
		got, want := match(pair[0]), match(pair[1])
		if len(want) == 0 {
			t.Fatalf("%s matched nothing; the pair proves nothing", pair[1])
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s matched %v, %s matched %v; want the same rows", pair[0], got, pair[1], want)
		}
	}
}

// TestSearchHyphenatedQuery: unicode61 indexes "zebracorn widget" as two tokens, so a hyphenated
// query must match them as an adjacent pair. Deleting the hyphen used to search for the single
// token "zebracornwidget" and return nothing. The mixed case checks that FTS5, not the sanitiser,
// folds case.
func TestSearchHyphenatedQuery(t *testing.T) {
	s, entries := testSearcher(t)
	out, err := s.Search(context.Background(), Request{Query: "Zebracorn-WIDGET", Mode: ModeFTS, K: 8})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(out.Result.Hits) == 0 {
		t.Fatal("hyphenated query returned no hits")
	}
	for _, h := range out.Result.Hits {
		if h.Path != entries[0].path {
			t.Errorf("hit from %s, want only %s", h.Path, entries[0].path)
		}
	}
}

// TestSearchANDThenORRetry: one unknown word must not empty the result. The AND query matches
// nothing, the OR retry finds the quokka chunk.
func TestSearchANDThenORRetry(t *testing.T) {
	s, _ := testSearcher(t)
	ctx := context.Background()

	andQuery, orQuery := SanitizeFTS("quokka fluffernutter")
	andOnly, _, err := s.ftsList(ctx, andQuery, "", "")
	if err != nil {
		t.Fatalf("fts AND: %v", err)
	}
	if len(andOnly) != 0 {
		t.Fatalf("AND query matched %d rows, want 0 (the fixture must not contain 'fluffernutter')", len(andOnly))
	}
	retried, _, err := s.ftsList(ctx, andQuery, orQuery, "")
	if err != nil {
		t.Fatalf("fts AND/OR: %v", err)
	}
	if len(retried) != 1 {
		t.Fatalf("OR retry matched %d rows, want 1", len(retried))
	}

	out, err := s.Search(ctx, Request{Query: "quokka fluffernutter", Mode: ModeFTS, K: 8})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if out.NFTS != 1 || len(out.Result.Hits) != 1 {
		t.Fatalf("n_fts=%d hits=%d, want 1 and 1", out.NFTS, len(out.Result.Hits))
	}
	if !strings.Contains(out.Result.Hits[0].Text, "quokka") {
		t.Errorf("hit = %q, want the quokka chunk", out.Result.Hits[0].Text)
	}
}

// TestSearchTimings: every stage is non-negative and the total covers the stages.
func TestSearchTimings(t *testing.T) {
	s, _ := testSearcher(t)
	out, err := s.Search(context.Background(), Request{Query: "widget zebracorn", Mode: ModeHybrid, K: 8})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for name, ms := range map[string]int64{
		"fts_ms": out.FTSMs, "vec_ms": out.VecMs, "embed_ms": out.EmbedMs, "total_ms": out.TotalMs,
	} {
		if ms < 0 {
			t.Errorf("%s = %d, want >= 0", name, ms)
		}
	}
	if out.TotalMs < out.FTSMs {
		t.Errorf("total_ms %d < fts_ms %d", out.TotalMs, out.FTSMs)
	}
	if out.TotalMs < out.VecMs {
		t.Errorf("total_ms %d < vec_ms %d", out.TotalMs, out.VecMs)
	}
	if out.TotalMs < out.EmbedMs {
		t.Errorf("total_ms %d < embed_ms %d", out.TotalMs, out.EmbedMs)
	}
}

// TestSearchEmptyQuery: a query with nothing indexable in it is an empty result, not an error and
// not a nil Hits slice.
func TestSearchEmptyQuery(t *testing.T) {
	s, _ := testSearcher(t)
	out, err := s.Search(context.Background(), Request{Query: `(((  )))`, Mode: ModeFTS, K: 8})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(out.Result.Hits) != 0 || out.Result.Hits == nil {
		t.Errorf("hits = %v, want an empty non-nil slice", out.Result.Hits)
	}
	if len(out.Candidates) != 0 || out.Candidates == nil {
		t.Errorf("candidates = %v, want an empty non-nil slice", out.Candidates)
	}
	if out.NFTS != 0 {
		t.Errorf("n_fts = %d, want 0", out.NFTS)
	}
}

// TestSearchUnknownMode rejects a mode the CLI should never send.
func TestSearchUnknownMode(t *testing.T) {
	s, _ := testSearcher(t)
	if _, err := s.Search(context.Background(), Request{Query: "widget", Mode: "sideways"}); err == nil {
		t.Fatal("want an error for an unknown mode")
	}
}

// TestSearchClampsK: K is clamped, and the Outcome reports the clamped value for the log.
func TestSearchClampsK(t *testing.T) {
	s, _ := testSearcher(t)
	out, err := s.Search(context.Background(), Request{Query: "widget", Mode: ModeFTS, K: 0})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if out.K != MinK {
		t.Errorf("K = %d, want %d", out.K, MinK)
	}
	if len(out.Result.Hits) != MinK {
		t.Errorf("hits = %d, want %d", len(out.Result.Hits), MinK)
	}
}
