package search

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/fake"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/store/sqlite"
)

var update = flag.Bool("update", false, "rewrite testdata/golden files")

// goldenVocab feeds the generated entries: enough "widget" chunks (over ListLimit) that both
// lists truncate, and shared words so the AND→OR retry and the diversity cap both fire.
var goldenVocab = []string{
	"widget", "gasket", "harbour", "lantern", "quokka", "zebracorn", "socket", "kernel",
	"tmux", "logout", "session", "pane", "docker", "volume", "bridge", "cache",
}

// goldenEntries is fixtures() plus twelve generated entries of four chunks each. Everything is
// derived by arithmetic, so the corpus (and its chunk ids) is the same on every run.
func goldenEntries() []testEntry {
	entries := fixtures()
	for i := 0; i < 12; i++ {
		e := testEntry{
			path:  fmt.Sprintf("gen/%02d.md", i),
			title: fmt.Sprintf("Generated %s %s", goldenVocab[i%len(goldenVocab)], goldenVocab[(i*5+3)%len(goldenVocab)]),
			tags:  []string{goldenVocab[(i*3)%len(goldenVocab)]},
		}
		for j := 0; j < 4; j++ {
			var words []string
			for w := 0; w < 6; w++ {
				words = append(words, goldenVocab[(i*7+j*3+w*w)%len(goldenVocab)])
			}
			if (i+j)%2 == 0 {
				words = append(words, "widget")
			}
			e.chunks = append(e.chunks, strings.Join(words, " "))
		}
		entries = append(entries, e)
	}
	return entries
}

// goldenSearcher indexes goldenEntries, in order, with the prefix-blind fake embedder.
func goldenSearcher(t *testing.T) *Searcher {
	t.Helper()
	return indexedSearcher(t, goldenEntries())
}

// indexedSearcher indexes entries, in order, with the prefix-blind fake embedder: chunk j > 0 gets
// the heading "section j".
func indexedSearcher(t testing.TB, entries []testEntry) *Searcher {
	t.Helper()
	ctx := context.Background()
	emb := prefixBlindEmbedder{fake.New("fake-embed", 768)}
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "kb.sqlite"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.EnsureEmbedMeta(ctx, store.EmbedMeta{Model: emb.Model(), Dim: emb.Dim()}); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		texts := make([]string, len(e.chunks))
		for j, text := range e.chunks {
			texts[j] = embed.DocPrefix + text
		}
		vecs, err := emb.Embed(ctx, texts)
		if err != nil {
			t.Fatal(err)
		}
		chunks := make([]store.ChunkInput, len(e.chunks))
		for j, text := range e.chunks {
			heading := ""
			if j > 0 {
				heading = fmt.Sprintf("section %d", j)
			}
			sum := sha256.Sum256([]byte(text))
			chunks[j] = store.ChunkInput{Ord: j, SourceFile: e.path, Heading: heading, Text: text,
				TextHash: hex.EncodeToString(sum[:]), Embedding: vecs[j]}
		}
		if _, err := st.ReplaceEntry(ctx, store.EntryInput{Path: e.path, Kind: kindOf(e.path), Title: e.title,
			Summary: "fixture", Tags: e.tags, BodyHash: "hash-" + e.path}, chunks); err != nil {
			t.Fatal(err)
		}
	}
	return &Searcher{Store: st, Log: st, Embedder: emb}
}

// goldenRequests cover every mode, the AND→OR retry, a tag filter (vector over-fetch), an
// unsanitisable query (vec only), both list limits and the diversity cap.
var goldenRequests = []Request{
	{Query: "widget", Mode: ModeHybrid, K: MaxK},
	{Query: "zebracorn", Mode: ModeHybrid, K: DefaultK},
	{Query: "quokka widget", Mode: ModeHybrid, K: DefaultK},
	{Query: "zebracorn quokka", Mode: ModeHybrid, K: DefaultK},
	{Query: "zebracorn nonexistentword", Mode: ModeHybrid, K: DefaultK},
	{Query: "widget enclosure", Mode: ModeFTS, K: MaxK},
	{Query: "container plumbing", Mode: ModeVec, K: 10},
	{Query: "widget", Mode: ModeHybrid, K: MaxK, Tag: "docker"},
	{Query: "widget", Mode: ModeVec, K: MaxK, Tag: "tmux"},
	{Query: "tmux logout session", Mode: ModeHybrid, K: MaxK},
	{Query: "draw-visual widget", Mode: ModeHybrid, K: 5},
	{Query: "&&", Mode: ModeHybrid, K: MinK},
	{Query: "gasket harbour lantern", Mode: ModeFTS, K: MaxK},
	{Query: "Gamma", Mode: ModeHybrid, K: DefaultK},
}

// renderOutcome prints everything a ranking decision produced: the fused list with both ranks,
// both raw scores and the RRF score (shortest exact float form, so equal text means equal bits)
// and the returned subsequence.
func renderOutcome(req Request, out *Outcome) string {
	var b strings.Builder
	f := func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
	fmt.Fprintf(&b, "== query=%q mode=%s k=%d tag=%q\n", req.Query, req.Mode, req.K, req.Tag)
	fmt.Fprintf(&b, "ran=%s n_fts=%d n_vec=%d", out.Mode, out.NFTS, out.NVec)
	if out.ExactMatch != "" { // only when the exact list ran, so two-list goldens are unchanged
		fmt.Fprintf(&b, " exact=%s n_exact=%d", out.ExactMatch, out.NExact)
	}
	b.WriteString("\n")
	for i, c := range out.Candidates {
		fr, fs, vr, vd := "-", "-", "-", "-"
		if c.FTSRank != nil {
			fr, fs = strconv.Itoa(*c.FTSRank), f(*c.FTSScore)
		}
		if c.VecRank != nil {
			vr, vd = strconv.Itoa(*c.VecRank), f(*c.VecDistance)
		}
		ex := ""
		if c.ExactRank != nil {
			ex = fmt.Sprintf(" exact=%d/%s", *c.ExactRank, f(*c.ExactScore))
		}
		fmt.Fprintf(&b, "cand %2d chunk=%d %s|%s fts=%s/%s vec=%s/%s%s rrf=%s\n",
			i+1, c.ChunkID, c.EntryPath, c.Heading, fr, fs, vr, vd, ex, f(c.RRFScore))
	}
	for _, h := range out.Result.Hits {
		fmt.Fprintf(&b, "hit %2d %s|%s rrf=%s\n", h.Rank, h.Path, h.Heading, f(h.RRFScore))
	}
	return b.String()
}

// TestSearchGoldenDefaults pins the full ranked output of the default settings over a fixed
// corpus: the fused candidates (ids, ranks, raw scores, RRF scores) and the returned hits. The
// golden was written before the search settings became configurable, so a pass proves the
// defaults are bit-identical to the old constants. Regenerate only for an intended ranking change
// (go test -tags fts5 ./internal/search -run TestSearchGoldenDefaults -update) and review the diff.
// Both a zero Searcher.Settings and Settings built from config.Default must match it.
func TestSearchGoldenDefaults(t *testing.T) {
	s := goldenSearcher(t)
	got := renderGolden(t, s)
	explicit, err := NewSettings(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	s.Settings = explicit
	if fromConfig := renderGolden(t, s); fromConfig != got {
		t.Errorf("Settings from config.Default rank differently from the zero Settings:\n%s", firstDiff(got, fromConfig))
	}
	// The one golden query with an identifier is draw-visual, which no chunk holds: its exact list
	// runs (the trace says so) but returns nothing, so turning the list off ranks every query
	// bit-identically. The two-list path is the pre-4.5 ranking.
	off := config.Default()
	off.Search.ExactList = false
	if s.Settings, err = NewSettings(off); err != nil {
		t.Fatal(err)
	}
	if noExact, want := renderGolden(t, s), strings.ReplaceAll(got, ` exact="draw-visual" n_exact=0`, ""); noExact != want {
		t.Errorf("exact_list=false ranks differently from the defaults on the golden corpus:\n%s", firstDiff(want, noExact))
	}
	checkGolden(t, "search_defaults.golden", got)
	// The independent oracle (W13 pass): with the phrase/NEAR tiers off the ranking is the
	// pre-4.6 one, bit for bit. search_untiered.golden is the defaults golden as it stood before
	// 4.6, so a tier change can never move an untiered ranking unnoticed.
	untiered := config.Default()
	untiered.Search.PhraseBoost = false
	if s.Settings, err = NewSettings(untiered); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "search_untiered.golden", renderGolden(t, s))
}

// checkGolden compares got with testdata/golden/name, rewriting it first under -update.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}
	if got != string(want) {
		t.Errorf("ranked output differs from %s:\n%s", path, firstDiff(string(want), got))
	}
}

// TestTierOnlyReordersFTS is the tiers' oracle against the untiered list (PLAN 4.6, W13 pass):
// on every golden query whose AND list is not cut at list_limit, phrase_boost changes only the
// order of the FTS list. The same chunks are in it, each with the same score (rescore puts back
// the AND query's bm25), and the vector list is untouched.
func TestTierOnlyReordersFTS(t *testing.T) {
	s := goldenSearcher(t)
	run := func(boost bool, req Request) *Outcome {
		cfg := config.Default()
		cfg.Search.PhraseBoost = boost
		st, err := NewSettings(cfg)
		if err != nil {
			t.Fatal(err)
		}
		s.Settings = st
		out, err := s.Search(context.Background(), req)
		if err != nil {
			t.Fatalf("Search(%+v): %v", req, err)
		}
		return out
	}
	scores := func(out *Outcome) (fts map[int64]float64, vec map[int64]int) {
		fts, vec = map[int64]float64{}, map[int64]int{}
		for _, c := range out.Candidates {
			if c.FTSRank != nil {
				fts[c.ChunkID] = *c.FTSScore
			}
			if c.VecRank != nil {
				vec[c.ChunkID] = *c.VecRank
			}
		}
		return fts, vec
	}
	tieredSeen := 0
	for _, req := range goldenRequests {
		on, off := run(true, req), run(false, req)
		if off.NFTS >= ListLimit {
			continue // a cut AND list: a phrase row below it may displace an AND row
		}
		if on.FTSTiers != nil {
			tieredSeen++
		}
		onFTS, onVec := scores(on)
		offFTS, offVec := scores(off)
		if !maps.Equal(onFTS, offFTS) || !maps.Equal(onVec, offVec) {
			t.Errorf("%q: tiers changed the lists' members or scores:\n on  fts %v vec %v\n off fts %v vec %v", req.Query, onFTS, onVec, offFTS, offVec)
		}
	}
	if tieredSeen == 0 {
		t.Fatal("no golden query ran tiered under its list limit: the property checked nothing")
	}
}

func renderGolden(t *testing.T, s *Searcher) string {
	t.Helper()
	var b strings.Builder
	for _, req := range goldenRequests {
		out, err := s.Search(context.Background(), req)
		if err != nil {
			t.Fatalf("Search(%+v): %v", req, err)
		}
		b.WriteString(renderOutcome(req, out))
	}
	return b.String()
}

// firstDiff shows the first differing line, which is enough to find a ranking change.
func firstDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return fmt.Sprintf("line %d\nwant: %s\n got: %s", i+1, wl, gl)
		}
	}
	return "(no line differs)"
}
