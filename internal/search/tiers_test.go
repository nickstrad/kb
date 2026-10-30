package search

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/store"
)

// TestTiered is the gate of the phrase and NEAR tiers: phrase_boost on and two or more groups.
func TestTiered(t *testing.T) {
	for _, tc := range []struct {
		name   string
		groups int
		boost  bool
		want   bool
	}{
		{"no groups", 0, true, false},
		{"one group is the AND query's own phrase", 1, true, false},
		{"two groups, boost on", 2, true, true},
		{"many groups, boost on", 5, true, true},
		{"two groups, boost off", 2, false, false},
		{"many groups, boost off", 5, false, false},
	} {
		if got := tiered(tc.groups, tc.boost); got != tc.want {
			t.Errorf("%s: tiered(%d, %v) = %v, want %v", tc.name, tc.groups, tc.boost, got, tc.want)
		}
	}
}

// TestCountGroups: the groups of a query are its fields that survive into the MATCH expression.
func TestCountGroups(t *testing.T) {
	for _, tc := range []struct {
		terms []string
		want  int
	}{
		{nil, 0},
		{[]string{"tmux"}, 1},
		{[]string{"tmux", "logout"}, 2},
		{[]string{"draw-visual"}, 1},           // one field, however many words
		{[]string{"tmux", "&&", "logout"}, 2},  // no letter or digit: dropped
		{[]string{"tmux", "AND", "logout"}, 2}, // bare operator word: dropped
		{[]string{"&&", "--"}, 0},
	} {
		if got := countGroups(tc.terms); got != tc.want {
			t.Errorf("countGroups(%q) = %d, want %d", tc.terms, got, tc.want)
		}
	}
}

func rows(ids ...int64) []store.FTSRow {
	out := make([]store.FTSRow, len(ids))
	for i, id := range ids {
		out[i] = store.FTSRow{ChunkID: id, Score: -float64(10*len(ids) - i)}
	}
	return out
}

// TestAssembleTiers: tier order, dedupe across tiers, the stamp on each row and the cut at limit.
func TestAssembleTiers(t *testing.T) {
	type idTier struct {
		id   int64
		tier string
	}
	for _, tc := range []struct {
		name  string
		tiers []tierSource
		limit int
		want  []idTier
	}{
		{"no tiers", nil, 5, nil},
		{"one tier keeps its order", []tierSource{{store.TierAnd, rows(3, 1, 2)}}, 5, []idTier{{3, "and"}, {1, "and"}, {2, "and"}}},
		{"tier order beats the order inside a tier",
			[]tierSource{{store.TierPhrase, rows(9)}, {store.TierNear, rows(5, 6)}, {store.TierAnd, rows(1, 2)}}, 10,
			[]idTier{{9, "phrase"}, {5, "near"}, {6, "near"}, {1, "and"}, {2, "and"}}},
		{"a chunk keeps its best tier",
			[]tierSource{{store.TierPhrase, rows(1)}, {store.TierNear, rows(2, 1)}, {store.TierAnd, rows(3, 2, 1)}}, 10,
			[]idTier{{1, "phrase"}, {2, "near"}, {3, "and"}}},
		{"cut at limit inside a tier", []tierSource{{store.TierPhrase, rows(1, 2, 3)}, {store.TierAnd, rows(4)}}, 2,
			[]idTier{{1, "phrase"}, {2, "phrase"}}},
		{"cut at limit between tiers", []tierSource{{store.TierPhrase, rows(1)}, {store.TierAnd, rows(2, 3)}}, 2,
			[]idTier{{1, "phrase"}, {2, "and"}}},
		{"exactly at limit", []tierSource{{store.TierPhrase, rows(1)}, {store.TierAnd, rows(2)}}, 2,
			[]idTier{{1, "phrase"}, {2, "and"}}},
		{"duplicates do not use up the limit", []tierSource{{store.TierPhrase, rows(1, 2)}, {store.TierAnd, rows(1, 2, 3)}}, 3,
			[]idTier{{1, "phrase"}, {2, "phrase"}, {3, "and"}}},
		{"empty tiers are skipped", []tierSource{{store.TierPhrase, nil}, {store.TierNear, nil}, {store.TierOr, rows(7)}}, 3,
			[]idTier{{7, "or"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []idTier
			for _, r := range assembleTiers(tc.tiers, tc.limit) {
				got = append(got, idTier{r.ChunkID, r.Tier})
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("assembleTiers = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRescore: a row with a score in the map takes it, the others keep their own.
func TestRescore(t *testing.T) {
	r := []store.FTSRow{{ChunkID: 1, Score: -1}, {ChunkID: 2, Score: -2}, {ChunkID: 3, Score: -3}}
	rescore(r, map[int64]float64{1: -10, 3: -30, 9: -90})
	want := []float64{-10, -2, -30}
	for i := range r {
		if r[i].Score != want[i] {
			t.Errorf("row %d score = %v, want %v", r[i].ChunkID, r[i].Score, want[i])
		}
	}
}

// tierCorpus: for the query "red apple" the chunk holding the words adjacent is the phrase tier,
// the ones with them apart (either order, within 8 tokens) the NEAR tier, and the one with them
// far apart the AND tier. The AND tier alone ranks the far chunk and the reversed one ahead of
// the phrase chunk because they repeat the words.
func tierCorpus() []testEntry {
	filler := strings.Repeat("and then some more ordinary words about nothing in particular ", 3)
	return []testEntry{
		{path: "a.md", title: "A", chunks: []string{
			"A: notes",
			"a red apple pie recipe",
			"red velvet cake with an apple tart",
			"apple apple apple apple apple red",
			"red red red " + filler + "apple apple apple apple",
		}},
		{path: "b.md", title: "B", chunks: []string{"B: unrelated", "a green pear and a yellow banana"}},
	}
}

func searchFTS(t *testing.T, s *Searcher, query string, boost bool, mutate ...func(*config.Config)) *Outcome {
	t.Helper()
	cfg := config.Default()
	cfg.Search.PhraseBoost = boost
	for _, m := range mutate {
		m(&cfg)
	}
	st, err := NewSettings(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.Settings = st
	out, err := s.Search(context.Background(), Request{Query: query, Mode: ModeFTS, K: MaxK})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// ftsOrder is the (text, tier) of each candidate that is in the FTS list, in fused order.
func ftsOrder(out *Outcome) (texts, tiers []string) {
	for _, c := range out.Candidates {
		if c.FTSRank != nil {
			texts = append(texts, c.Text)
			tiers = append(tiers, c.FTSTier)
		}
	}
	return texts, tiers
}

// TestTierOrdering: with phrase_boost the phrase chunk leads, then the NEAR ones, then the AND-only
// one; with it off the list is the plain AND list and carries no tier. Rank, score and tier are
// consistent: ranks are 1..n in order, and every score is the AND query's.
func TestTierOrdering(t *testing.T) {
	s := indexedSearcher(t, tierCorpus())
	on := searchFTS(t, s, "red apple", true)
	texts, tiers := ftsOrder(on)
	if len(texts) != 4 {
		t.Fatalf("FTS list = %q, want the 4 chunks with both words", texts)
	}
	if texts[0] != "a red apple pie recipe" || tiers[0] != store.TierPhrase {
		t.Errorf("first = %q tier %q, want the phrase chunk with tier phrase", texts[0], tiers[0])
	}
	if !slices.Equal(tiers, []string{"phrase", "near", "near", "and"}) {
		t.Errorf("tiers = %v, want [phrase near near and]", tiers)
	}
	if texts[3] != "red red red "+strings.Repeat("and then some more ordinary words about nothing in particular ", 3)+"apple apple apple apple" {
		t.Errorf("last = %q, want the far chunk (AND tier)", texts[3])
	}
	if got := on.FTSTiers; len(got) != 3 || got[0].Tier != "phrase" || got[0].Match != `"red apple"` ||
		got[1].Tier != "near" || got[1].Match != `NEAR("red" "apple", 8)` || got[2].Tier != "and" ||
		got[0].Rows != 1 || got[1].Rows != 3 || got[2].Rows != 4 {
		t.Errorf("FTSTiers = %+v", got)
	}
	if on.FTSMatch != `"red" AND "apple"` || on.FTSFallbackToOr {
		t.Errorf("FTSMatch = %q (OR %v), want the AND expression", on.FTSMatch, on.FTSFallbackToOr)
	}

	// Each row is scored by the AND query, whatever tier placed it.
	cfg := config.Default()
	st, _ := NewSettings(cfg)
	w := st.Params.BM25
	and, err := s.Store.FTS(context.Background(), store.FTSQuery{Terms: []string{"red", "apple"}, Op: store.OpAnd, Limit: 20, Weights: &w})
	if err != nil {
		t.Fatal(err)
	}
	andScore := map[int64]float64{}
	for _, r := range and.Rows {
		andScore[r.ChunkID] = r.Score
	}
	for _, c := range on.Candidates {
		if want, ok := andScore[c.ChunkID]; !ok || *c.FTSScore != want {
			t.Errorf("chunk %d (%s tier) score %v, want the AND query's %v", c.ChunkID, c.FTSTier, *c.FTSScore, want)
		}
	}

	off := searchFTS(t, s, "red apple", false)
	offTexts, offTiers := ftsOrder(off)
	if len(offTexts) != 4 || off.FTSTiers != nil {
		t.Errorf("boost off: %d rows, tiers %v; want the plain AND list, untiered", len(offTexts), off.FTSTiers)
	}
	if slices.Contains(offTiers, store.TierPhrase) || slices.Contains(offTiers, "and") {
		t.Errorf("boost off: tiers %v, want none", offTiers)
	}
	if offTexts[0] == "a red apple pie recipe" {
		t.Skip("the corpus no longer shows the tier changing the order: the phrase chunk leads the plain list too")
	}
}

// TestTierSingleGroupIsPlain: one query group never runs the phrase or NEAR tiers.
func TestTierSingleGroupIsPlain(t *testing.T) {
	s := indexedSearcher(t, tierCorpus())
	out := searchFTS(t, s, "apple", true)
	if out.FTSTiers != nil {
		t.Errorf("FTSTiers = %+v for a one-word query, want none", out.FTSTiers)
	}
	for _, c := range out.Candidates {
		if c.FTSTier != "" {
			t.Errorf("chunk %d has tier %q, want none", c.ChunkID, c.FTSTier)
		}
	}
	hyphen := searchFTS(t, s, "red-apple", true) // one field
	if hyphen.FTSTiers != nil {
		t.Errorf("FTSTiers = %+v for a one-field query", hyphen.FTSTiers)
	}
}

// TestTierORFallbackIsTagged: when AND matches nothing the OR retry is the only query and its
// rows carry the "or" tier; the phrase and NEAR queries do not run.
func TestTierORFallbackIsTagged(t *testing.T) {
	f := &faultyRetriever{ftsRows: nil, match: "", Retriever: indexedSearcher(t, tierCorpus()).Store}
	s := &Searcher{Store: f}
	ml, err := s.mainList(context.Background(), []string{"red", "zzzunknown"}, nil, "", DefaultParams())
	if err != nil {
		t.Fatal(err)
	}
	if !ml.retried || len(ml.res.Rows) == 0 {
		t.Fatalf("retried = %v rows = %d, want the OR retry to find the red chunks", ml.retried, len(ml.res.Rows))
	}
	for _, r := range ml.res.Rows {
		if r.Tier != store.TierOr {
			t.Errorf("row %d tier %q, want or", r.ChunkID, r.Tier)
		}
	}
	if !slices.Equal(f.ftsOps, []store.FTSOp{store.OpAnd, store.OpOr}) {
		t.Errorf("queries = %v, want AND then OR only (no phrase or NEAR on an empty AND)", f.ftsOps)
	}
	if len(ml.runs) != 1 || ml.runs[0].Tier != store.TierOr {
		t.Errorf("runs = %+v, want the one OR run", ml.runs)
	}
}

// TestTierQueryPlan: which queries run for each shape, in order.
func TestTierQueryPlan(t *testing.T) {
	base := indexedSearcher(t, tierCorpus()).Store
	for _, tc := range []struct {
		name  string
		terms []string
		boost bool
		want  []store.FTSOp
	}{
		{"one group", []string{"red"}, true, []store.FTSOp{store.OpAnd}},
		{"two groups, boost off", []string{"red", "apple"}, false, []store.FTSOp{store.OpAnd}},
		{"two groups, boost on: AND, phrase, NEAR", []string{"red", "apple"}, true,
			[]store.FTSOp{store.OpAnd, store.OpPhrase, store.OpNear}},
		{"AND empty: OR only", []string{"red", "zzzunknown"}, true, []store.FTSOp{store.OpAnd, store.OpOr}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &faultyRetriever{Retriever: base}
			s := &Searcher{Store: f}
			p := DefaultParams()
			p.PhraseBoost = tc.boost
			if _, err := s.mainList(context.Background(), tc.terms, nil, "", p); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(f.ftsOps, tc.want) {
				t.Errorf("queries = %v, want %v", f.ftsOps, tc.want)
			}
		})
	}
}

// TestTierSynonymsAndExact: the phrase and NEAR queries use the literal words, so a synonym is
// found by the AND tier only; a chunk with the literal phrase is the phrase tier. The exact list
// is not tiered.
func TestTierSynonymsAndExact(t *testing.T) {
	s := indexedSearcher(t, []testEntry{
		{path: "k.md", title: "K", chunks: []string{
			"K: notes",
			"the kubernetes cluster setup guide",
			"the k8s cluster setup guide",
			"plain text about foo_bar and cluster things",
		}},
	})
	s.Synonyms = ParseSynonyms([]byte("k8s, kubernetes\n"))
	out := searchFTS(t, s, "k8s cluster", true)
	texts, tiers := ftsOrder(out)
	if len(texts) != 2 || texts[0] != "the k8s cluster setup guide" || tiers[0] != "phrase" || texts[1] != "the kubernetes cluster setup guide" || tiers[1] != "and" {
		t.Errorf("FTS list = %q tiers %v, want the literal phrase chunk (phrase) then the synonym chunk (and)", texts, tiers)
	}
	if out.FTSTiers[0].Match != `"k8s cluster"` {
		t.Errorf("phrase match = %q, want the literal words", out.FTSTiers[0].Match)
	}
	if want := `("k8s" OR "kubernetes") AND "cluster"`; out.FTSMatch != want {
		t.Errorf("FTSMatch = %q, want the synonym-expanded AND %q", out.FTSMatch, want)
	}

	exact := searchFTS(t, s, "foo_bar cluster", true)
	if exact.ExactMatch == "" {
		t.Fatal("the exact list did not run")
	}
	for _, c := range exact.Candidates {
		if c.ExactRank != nil && c.FTSTier == "" && c.FTSRank != nil {
			t.Errorf("chunk %d is in the main list without a tier", c.ChunkID)
		}
	}
}

// TestTierSpellKeepsTiers: a spell rerun tiers like the first run, and a row of the first run the
// rerun pushed out keeps its tier.
func TestTierSpellKeepsTiers(t *testing.T) {
	s := indexedSearcher(t, tierCorpus())
	out := searchFTS(t, s, "pie appel", true) // AND finds nothing, OR finds the "pie" chunk; "appel" -> "apple"
	if len(out.Spell) == 0 {
		t.Fatalf("spell found no correction (FTSMatch %q)", out.FTSMatch)
	}
	texts, tiers := ftsOrder(out)
	if len(texts) < 2 || texts[0] == "" {
		t.Fatalf("list = %q, want the pie chunk and the corrected apple chunks", texts)
	}
	for i, tier := range tiers {
		if tier != store.TierOr {
			t.Errorf("row %d (%q) tier %q, want or: the corrected query is still the OR retry", i, texts[i], tier)
		}
	}
	if !out.FTSFallbackToOr || len(out.FTSTiers) != 1 || out.FTSTiers[0].Tier != store.TierOr {
		t.Errorf("OR %v FTSTiers %+v, want the one OR run", out.FTSFallbackToOr, out.FTSTiers)
	}
}

// TestTierNeverLosesAnANDRow is the property of the feature: for random corpora and queries, when
// the AND list is shorter than list_limit the tiered list holds exactly its chunks (the tiers
// reorder, never drop); when it is longer, both lists are full. Tiers appear in phrase, near,
// and order, and ranks are 1..n.
func TestTierNeverLosesAnANDRow(t *testing.T) {
	seed := int64(20261030)
	r := rand.New(rand.NewSource(seed))
	words := []string{"red", "apple", "pie", "tart", "cake", "green", "pear"}
	var chunks []string
	for i := 0; i < 40; i++ {
		n := 3 + r.Intn(10)
		ws := make([]string, n)
		for j := range ws {
			ws[j] = words[r.Intn(len(words))]
		}
		chunks = append(chunks, strings.Join(ws, " "))
	}
	s := indexedSearcher(t, []testEntry{{path: "r.md", title: "R", chunks: chunks}})
	for q := 0; q < 60; q++ {
		n := 2 + r.Intn(2)
		qs := make([]string, n)
		for j := range qs {
			qs[j] = words[r.Intn(len(words))]
		}
		query := strings.Join(qs, " ")
		for _, limit := range []int{5, 20, 60} {
			set := func(c *config.Config) { c.Search.ListLimit = limit }
			on := searchFTS(t, s, query, true, set)
			off := searchFTS(t, s, query, false, set)
			onIDs, offIDs := map[int64]bool{}, map[int64]bool{}
			order := map[string]int{"phrase": 0, "near": 1, "and": 2, "or": 3}
			last, rank := -1, 0
			var byRank []Candidate
			for _, c := range on.Candidates {
				if c.FTSRank != nil {
					onIDs[c.ChunkID] = true
					byRank = append(byRank, c)
				}
			}
			slices.SortFunc(byRank, func(a, b Candidate) int { return *a.FTSRank - *b.FTSRank })
			for _, c := range byRank {
				rank++
				if *c.FTSRank != rank {
					t.Fatalf("seed %d %q limit %d: FTS ranks are not 1..n", seed, query, limit)
				}
				if o, ok := order[c.FTSTier]; !ok || o < last {
					t.Fatalf("seed %d %q limit %d: tier %q at rank %d breaks phrase, near, and order", seed, query, limit, c.FTSTier, rank)
				}
				last = order[c.FTSTier]
			}
			for _, c := range off.Candidates {
				if c.FTSRank != nil {
					offIDs[c.ChunkID] = true
				}
			}
			if on.NFTS != off.NFTS {
				t.Fatalf("seed %d %q limit %d: %d tiered rows, %d plain rows", seed, query, limit, on.NFTS, off.NFTS)
			}
			if off.NFTS < limit && !reflect.DeepEqual(onIDs, offIDs) {
				t.Fatalf("seed %d %q limit %d: the AND list is complete (%d rows) but the tiered list differs:\n tiered %v\n plain  %v",
					seed, query, limit, off.NFTS, onIDs, offIDs)
			}
		}
	}
}

// TestTierExpressionForms pins the expression forms the log and kb explain show.
func TestTierExpressionForms(t *testing.T) {
	s := indexedSearcher(t, tierCorpus())
	out := searchFTS(t, s, "red  apple pie", true)
	var got []string
	for _, r := range out.FTSTiers {
		got = append(got, fmt.Sprintf("%s %s", r.Tier, r.Match))
	}
	want := []string{`phrase "red apple pie"`, `near NEAR("red" "apple" "pie", 8)`, `and "red" AND "apple" AND "pie"`}
	if len(out.FTSTiers) == 0 {
		t.Skip("AND matched nothing: the tiers did not run") // no chunk holds all three words
	}
	if !slices.Equal(got, want) {
		t.Errorf("tier expressions = %q, want %q", got, want)
	}
}

// TestLoggedHitsCarryTierAndMemory: the log record of a candidate holds its tier and its place in
// the query-memory list (Q135), and nothing for what it lacks.
func TestLoggedHitsCarryTierAndMemory(t *testing.T) {
	rank, u := 3, 0.5
	got := loggedHits([]Candidate{
		{ChunkID: 1, FTSTier: store.TierNear, MemoryRank: &rank, MemoryScore: &u},
		{ChunkID: 2},
	})
	if got[0].FTSTier != "near" || got[0].MemoryRank != &rank || got[0].MemoryScore != &u {
		t.Errorf("hit 0 = %+v, want tier near and memory 3/0.5", got[0])
	}
	if got[1].FTSTier != "" || got[1].MemoryRank != nil || got[1].MemoryScore != nil {
		t.Errorf("hit 1 = %+v, want no tier and no memory", got[1])
	}
}

// TestRecordLogsTier: a tiered search writes each candidate's tier to search_candidates and
// search_results; an untiered one writes NULL.
func TestRecordLogsTier(t *testing.T) {
	s := indexedSearcher(t, tierCorpus())
	ctx := context.Background()
	for _, tc := range []struct {
		boost bool
		want  []string
	}{
		{true, []string{"phrase", "near", "near", "and"}},
		{false, []string{"<nil>", "<nil>", "<nil>", "<nil>"}},
	} {
		out := searchFTS(t, s, "red apple", tc.boost)
		id, err := s.Record(ctx, Request{Query: "red apple", Mode: ModeFTS}, out)
		if err != nil {
			t.Fatal(err)
		}
		for _, table := range []struct{ sql string }{
			{"SELECT fts_tier FROM search_candidates WHERE search_id = ? ORDER BY fts_rank"},
			{"SELECT fts_tier FROM search_results WHERE search_id = ? ORDER BY rank"},
		} {
			tab, err := s.Store.(store.Store).QueryLog(ctx, table.sql, id) // the log's read surface, not raw SQL
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, row := range tab.Rows {
				got = append(got, fmt.Sprint(row[0]))
			}
			want := tc.want
			if strings.Contains(table.sql, "search_results") {
				want = want[:3] // the per-entry cap keeps three of the four chunks of a.md
			}
			if !slices.Equal(got, want) {
				t.Errorf("boost=%v %s = %v, want %v", tc.boost, table.sql, got, want)
			}
		}
	}
}

// TestRecordLogsMemoryRank (Q135): the memory list's rank and u are written with the candidate and
// the result, so eval and stats can see which hits memory placed.
func TestRecordLogsMemoryRank(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	logUseful(t, st, 2, "Calibration, sprocket!", "hidden.md", "Tuning")
	s := memSearcher(t, st, true, 2, 1)
	req := Request{Query: "calibration SPROCKET", Mode: ModeFTS, K: 5}
	out, err := s.Search(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Record(ctx, req, out)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"SELECT memory_rank, memory_score FROM search_candidates WHERE search_id = ? AND fused_rank = 1",
		"SELECT memory_rank, memory_score FROM search_results WHERE search_id = ? AND rank = 1",
	} {
		tab, err := st.QueryLog(ctx, q, id) // the log's read surface, not raw SQL
		if err != nil {
			t.Fatal(err)
		}
		if len(tab.Rows) != 1 || tab.Rows[0][0] != int64(1) || tab.Rows[0][1] != *out.Candidates[0].MemoryScore {
			t.Errorf("%s = %v, want memory rank 1 and u %v", q, tab.Rows, *out.Candidates[0].MemoryScore)
		}
	}
}

// TestFuseCarriesTier: the fused candidate keeps the tier of its FTS row; a chunk outside the FTS
// list has none.
func TestFuseCarriesTier(t *testing.T) {
	got := Fuse([]FTSRow{{ChunkID: 1, Tier: store.TierPhrase}, {ChunkID: 2, Tier: store.TierAnd}},
		[]VecRow{{ChunkID: 3}}, nil, DefaultParams())
	tier := map[int64]string{}
	for _, c := range got {
		tier[c.ChunkID] = c.FTSTier
	}
	if tier[1] != "phrase" || tier[2] != "and" || tier[3] != "" {
		t.Errorf("tiers = %v, want 1 phrase, 2 and, 3 none", tier)
	}
}

// opRetriever answers FTS per op: a configured error or result for an op, else the real store.
// calls records every query (the rescoring AND query has a ChunkIDs restriction).
type opRetriever struct {
	store.Retriever
	fail    map[store.FTSOp]error
	replace map[store.FTSOp]store.FTSResult
	calls   []store.FTSQuery
}

func (o *opRetriever) FTS(ctx context.Context, q store.FTSQuery) (store.FTSResult, error) {
	o.calls = append(o.calls, q)
	if err := o.fail[q.Op]; err != nil {
		return store.FTSResult{}, err
	}
	if r, ok := o.replace[q.Op]; ok {
		return r, nil
	}
	return o.Retriever.FTS(ctx, q)
}

// TestMainListFaults: a failure of the phrase, the NEAR or the rescoring query fails the list with
// that error (no half-tiered result), and a tier whose query ran nothing is left out of the runs.
func TestMainListFaults(t *testing.T) {
	base := indexedSearcher(t, tierCorpus()).Store
	terms := []string{"red", "apple"}
	p := DefaultParams()
	for _, op := range []store.FTSOp{store.OpPhrase, store.OpNear} {
		o := &opRetriever{Retriever: base, fail: map[store.FTSOp]error{op: errDriver}}
		if _, err := (&Searcher{Store: o}).mainList(context.Background(), terms, nil, "", p); !errors.Is(err, errDriver) {
			t.Errorf("op %v failing: err = %v, want errDriver", op, err)
		}
	}

	// Rescoring: with list_limit 1 the AND list holds one chunk, the phrase chunk is not in it, so
	// one more AND query (restricted to chunk ids) scores it, and the rows carry its scores.
	p.ListLimit = 1
	o := &opRetriever{Retriever: base}
	ml, err := (&Searcher{Store: o}).mainList(context.Background(), terms, nil, "", p)
	if err != nil {
		t.Fatal(err)
	}
	var rescored *store.FTSQuery
	for i := range o.calls {
		if o.calls[i].Op == store.OpAnd && o.calls[i].ChunkIDs != nil {
			rescored = &o.calls[i]
		}
	}
	if rescored == nil || rescored.Limit != len(rescored.ChunkIDs) {
		t.Fatalf("calls %+v: want one AND query restricted to the missing chunk ids, limit = their count", o.calls)
	}
	w := p.BM25
	want, err := base.FTS(context.Background(), store.FTSQuery{Terms: terms, Op: store.OpAnd, Limit: 20, Weights: &w})
	if err != nil {
		t.Fatal(err)
	}
	score := map[int64]float64{}
	for _, r := range want.Rows {
		score[r.ChunkID] = r.Score
	}
	for _, r := range ml.res.Rows {
		if r.Score != score[r.ChunkID] {
			t.Errorf("chunk %d (tier %s) score %v, want the AND query's %v", r.ChunkID, r.Tier, r.Score, score[r.ChunkID])
		}
	}
	// The rescoring query fails: the error is returned.
	fail := &failRescore{Retriever: base}
	if _, err := (&Searcher{Store: fail}).mainList(context.Background(), terms, nil, "", p); !errors.Is(err, errDriver) {
		t.Errorf("rescoring failure: err = %v, want errDriver", err)
	}

	// A tier whose query ran nothing (empty Match) is not a run.
	empty := &opRetriever{Retriever: base, replace: map[store.FTSOp]store.FTSResult{store.OpPhrase: {}}}
	p.ListLimit = 20
	ml, err = (&Searcher{Store: empty}).mainList(context.Background(), terms, nil, "", p)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range ml.runs {
		if r.Tier == store.TierPhrase {
			t.Errorf("runs = %+v, want no phrase run for a query that ran nothing", ml.runs)
		}
	}
}

// failRescore fails an AND query restricted to chunk ids (the rescoring query) and nothing else.
type failRescore struct{ store.Retriever }

func (f *failRescore) FTS(ctx context.Context, q store.FTSQuery) (store.FTSResult, error) {
	if q.Op == store.OpAnd && q.ChunkIDs != nil {
		return store.FTSResult{}, errDriver
	}
	return f.Retriever.FTS(ctx, q)
}
