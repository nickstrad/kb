package search

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
)

// TestEditDistance is the distance table: 0 to 3 edits of each kind, transpositions, runes,
// empties and the OSA rule (no substring edited twice).
func TestEditDistance(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"kubernetes", "kubernetes", 0},
		{"", "abc", 3},
		{"abc", "", 3},
		{"kubernetes", "kuberntes", 1}, // deletion
		{"kuberntes", "kubernetes", 1}, // insertion
		{"widget", "widgat", 1},        // substitution
		{"widget", "widgte", 1},        // transposition
		{"zebracorn", "zebracron", 1},  // transposition
		{"widget", "wigdte", 2},        // two transpositions
		{"widget", "wxdgxx", 3},        // three substitutions
		{"ca", "abc", 3},               // OSA, not full Damerau (2)
		{"café", "cafe", 1},            // one rune, two bytes
		{"straße", "strasse", 2},       // ß -> s, insert s
		{"abcdef", "badcfe", 3},        // three transpositions
		{"kitten", "sitting", 3},       // the classic
		{"flaw", "lawn", 2},            // delete + insert
		{"a", "b", 1},
	} {
		if got := EditDistance(tc.a, tc.b); got != tc.want {
			t.Errorf("EditDistance(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
		if got := EditDistance(tc.b, tc.a); got != tc.want {
			t.Errorf("EditDistance(%q, %q) = %d, want %d (symmetry)", tc.b, tc.a, got, tc.want)
		}
	}
}

// TestEditDistanceWithin: the bounded form agrees with the full distance at and around the bound,
// and reports a distance above it as not within.
func TestEditDistanceWithin(t *testing.T) {
	for _, tc := range []struct {
		a, b   string
		maxd   int
		want   int
		within bool
	}{
		{"widget", "widget", 0, 0, true},
		{"widget", "widgte", 0, 1, false},
		{"widget", "widgte", 1, 1, true},
		{"widget", "wigdte", 1, 2, false},
		{"widget", "wigdte", 2, 2, true},
		{"widget", "wxdgxx", 2, 3, false},
		{"widget", "wxdgxx", 3, 3, true},
		{"ab", "abcde", 2, 3, false}, // length difference alone exceeds the bound
		{"ab", "abcd", 2, 2, true},
	} {
		d, within := editDistanceWithin([]rune(tc.a), []rune(tc.b), tc.maxd)
		if within != tc.within || (within && d != tc.want) || (!within && d <= tc.maxd) {
			t.Errorf("editDistanceWithin(%q, %q, %d) = %d, %v; want %d, %v", tc.a, tc.b, tc.maxd, d, within, tc.want, tc.within)
		}
	}
}

// TestMaxEdits is the length boundary table: 1-3 letters never, 4-7 one edit, 8+ two.
func TestMaxEdits(t *testing.T) {
	for n, want := range map[int]int{0: 0, 1: 0, 2: 0, 3: 0, 4: 1, 5: 1, 7: 1, 8: 2, 9: 2, 30: 2} {
		if got := MaxEdits(n); got != want {
			t.Errorf("MaxEdits(%d) = %d, want %d", n, got, want)
		}
	}
}

// TestSpellWord: which query fields spell may correct (never identifiers, numbers, short words).
func TestSpellWord(t *testing.T) {
	for _, tc := range []struct {
		field string
		want  string // "" = not a word
	}{
		{"kuberntes", "kuberntes"},
		{"Kuberntes,", "kuberntes"}, // punctuation and case
		{"(kuberntes).", "kuberntes"},
		{"abc", ""},       // 3 letters
		{"abcd", "abcd"},  // 4 letters
		{"foo_bar", ""},   // identifier
		{"kb.toml", ""},   // identifier
		{"zebraCorn", ""}, // camelCase identifier
		{"v1beta", ""},    // identifier (digit then letter)
		{"2024", ""},      // number
		{"abc1", ""},      // digit
		{"café", "café"},  // letters with an accent
		{"ÉCOLE", "école"},
		{"--", ""},
		{"", ""},
	} {
		got, ok := spellWord(tc.field)
		if ok != (tc.want != "") || got != tc.want {
			t.Errorf("spellWord(%q) = %q, %v; want %q", tc.field, got, ok, tc.want)
		}
	}
}

// TestCorrect is the candidate-ranking table: the distance bound by length, the document-count
// floor, and the tie-breaks (fewer edits, then more documents, then the smaller term).
func TestCorrect(t *testing.T) {
	v := func(pairs ...any) []store.VocabTerm {
		var out []store.VocabTerm
		for i := 0; i < len(pairs); i += 2 {
			out = append(out, store.VocabTerm{Term: pairs[i].(string), Doc: pairs[i+1].(int)})
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		word  string
		vocab []store.VocabTerm
		want  string // "" = no correction
		dist  int
	}{
		{"distance 1 found", "widgte", v("widget", 5), "widget", 1},
		{"distance 0 is not a correction", "widget", v("widget", 5), "", 0},
		{"distance 2 too far for 6 letters", "wigdte", v("widget", 5), "", 0},
		{"distance 2 allowed from 8 letters", "zebrcaron", v("zebracorn", 5), "zebracorn", 2},
		{"distance 3 never", "zxbrcaron", v("zebracorn", 5), "", 0},
		{"doc 1 is too rare", "widgte", v("widget", 1), "", 0},
		{"doc 2 is enough", "widgte", v("widget", 2), "widget", 1},
		{"3-letter word never corrected", "teh", v("the", 99), "", 0},
		{"4-letter word gets one edit", "gaet", v("gate", 2), "gate", 1},
		{"7 letters: one edit only", "enclsre", v("enclosure", 9), "", 0},
		{"fewer edits beat more documents", "zebracron", v("zebracorn", 2, "zebrafrom", 50), "zebracorn", 1},
		{"equal edits: more documents", "widgxt", v("widget", 2, "widgat", 9), "widgat", 1},
		{"equal edits and documents: smaller term", "widgxt", v("widgyt", 4, "widget", 4), "widget", 1},
		{"non-letter vocabulary term skipped", "kbtoml", v("kb.toml", 9, "kbtoms", 2), "kbtoms", 1},
		{"empty vocabulary", "widgte", nil, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fix, ok := Correct(tc.word, tc.vocab)
			if ok != (tc.want != "") || fix.Correction != tc.want || (ok && fix.Distance != tc.dist) {
				t.Errorf("Correct(%q) = %+v, %v; want %q at %d", tc.word, fix, ok, tc.want, tc.dist)
			}
			// The vocabulary order never matters.
			rev := slices.Clone(tc.vocab)
			slices.Reverse(rev)
			if again, _ := Correct(tc.word, rev); again != fix {
				t.Errorf("reversed vocabulary gave %+v, want %+v", again, fix)
			}
		})
	}
}

// TestSpellWanted is the trigger decision table.
func TestSpellWanted(t *testing.T) {
	for _, tc := range []struct {
		name     string
		on       bool
		hasVocab bool
		match    string
		rows     int
		want     bool
	}{
		{"0 rows", true, true, `"x"`, 0, true},
		{"2 rows", true, true, `"x"`, 2, true},
		{"3 rows", true, true, `"x"`, 3, false},
		{"4 rows", true, true, `"x"`, 4, false},
		{"spell off", false, true, `"x"`, 0, false},
		{"store without a vocabulary", true, false, `"x"`, 0, false},
		{"no FTS list ran", true, true, "", 0, false},
	} {
		if got := spellWanted(tc.on, tc.hasVocab, tc.match, tc.rows); got != tc.want {
			t.Errorf("%s: spellWanted = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestKeepOriginal: the first run's rows survive the rerun's limit, in place of new rows.
func TestKeepOriginal(t *testing.T) {
	rows := func(ids ...int64) []FTSRow {
		out := make([]FTSRow, len(ids))
		for i, id := range ids {
			out[i] = FTSRow{ChunkID: id}
		}
		return out
	}
	ids := func(rs []FTSRow) []int64 {
		out := make([]int64, len(rs))
		for i, r := range rs {
			out[i] = r.ChunkID
		}
		return out
	}
	for _, tc := range []struct {
		name               string
		first, rerun, want []FTSRow
		limit              int
	}{
		{"rerun holds them all", rows(1, 2), rows(9, 1, 2), rows(9, 1, 2), 20},
		{"one pushed out", rows(1, 2), rows(9, 8, 1), rows(9, 1, 2), 3},
		{"both pushed out", rows(1, 2), rows(9, 8, 7), rows(9, 1, 2), 3},
		{"limit equals first", rows(1, 2), rows(9, 8), rows(1, 2), 2},
		{"nothing first", nil, rows(9, 8), rows(9, 8), 2},
		{"empty rerun", rows(1), nil, rows(1), 5},
	} {
		got := keepOriginal(tc.first, tc.rerun, tc.limit)
		if !slices.Equal(ids(got), ids(tc.want)) || len(got) > tc.limit {
			t.Errorf("%s: keepOriginal = %v, want %v", tc.name, ids(got), ids(tc.want))
		}
	}
}

// spellSearcher is testSearcher with spell on (the default) and the given params edit.
func spellSearcher(t testing.TB, edit func(*config.Search)) *Searcher {
	s, _ := testSearcher(t)
	s.Settings = settingsWith(t, edit)
	return s
}

// TestSearchSpell is the integration decision table against the fixtures: the trigger (rows
// 0/2/3, spell off, vec mode), what is corrected (identifiers and rare terms are not) and the
// rewritten expression the log records.
func TestSearchSpell(t *testing.T) {
	off := func(c *config.Search) { c.Spell = false }
	for _, tc := range []struct {
		name      string
		edit      func(*config.Search)
		query     string
		mode      string
		wantFix   string // "term->correction", "" = none
		wantMatch string
		wantOr    bool
		minHits   int
	}{
		{"typo, 0 rows: corrected", nil, "zebracron", ModeFTS, "zebracron->zebracorn", `("zebracron" OR "zebracorn")`, false, 3},
		{"typo with punctuation keeps the field as typed", nil, "Zebracron,", ModeFTS, "Zebracron,->zebracorn", `("Zebracron," OR "zebracorn")`, false, 3},
		{"OR retry with 2 rows: corrected, stays OR", nil, "enclosure zebracron", ModeFTS, "zebracron->zebracorn", `"enclosure" OR ("zebracron" OR "zebracorn")`, true, 3},
		// The AND of the rewrite would match (herding and zebracorn share a chunk) but narrow
		// the OR list the typed query found, so the rerun stays OR.
		{"OR retry with 1 row: rerun stays OR even when AND would match", nil, "herding zebracron", ModeFTS, "zebracron->zebracorn", `"herding" OR ("zebracron" OR "zebracorn")`, true, 3},
		{"OR retry with 3 rows: not corrected", nil, "about zebracron", ModeFTS, "", `"about" OR "zebracron"`, true, 3},
		{"spell off", off, "zebracron", ModeFTS, "", `"zebracron"`, false, 0},
		{"vec mode runs no FTS list", nil, "zebracron", ModeVec, "", "", false, 1},
		{"identifier never corrected", nil, "zebra_cron", ModeFTS, "", `"zebra_cron"`, false, 0},
		{"rare correction (doc 1) not used", nil, "quokak", ModeFTS, "", `"quokak"`, false, 0},
		{"known word with few rows not corrected", nil, "quokka", ModeFTS, "", `"quokka"`, false, 1},
		{"short word never corrected", nil, "gat", ModeFTS, "", `"gat"`, false, 0},
		{"distance 2 for a long word", nil, "enclsoure", ModeFTS, "enclsoure->enclosure", `("enclsoure" OR "enclosure")`, false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			edit := tc.edit
			if edit == nil {
				edit = func(*config.Search) {}
			}
			s := spellSearcher(t, edit)
			out, err := s.Search(context.Background(), Request{Query: tc.query, Mode: tc.mode, K: 8})
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if len(out.Spell) > 0 {
				got = out.Spell[0].Term + "->" + out.Spell[0].Correction
			}
			if got != tc.wantFix || len(out.Spell) > 1 {
				t.Errorf("spell = %+v, want %q", out.Spell, tc.wantFix)
			}
			if out.FTSMatch != tc.wantMatch || out.FTSFallbackToOr != tc.wantOr {
				t.Errorf("match = %q or=%v, want %q or=%v", out.FTSMatch, out.FTSFallbackToOr, tc.wantMatch, tc.wantOr)
			}
			if len(out.Result.Hits) < tc.minHits {
				t.Errorf("hits = %d, want at least %d", len(out.Result.Hits), tc.minHits)
			}
			if tc.wantFix == "" && out.FTSSynonyms != nil {
				t.Errorf("FTSSynonyms = %v with no synonyms", out.FTSSynonyms)
			}
		})
	}
}

// TestSpellExpandsCorrectionSynonyms: the correction brings its synonyms, and a synonym of the
// typed word stays (the 5.5 status note: spell goes through Searcher.Synonyms and Alts).
func TestSpellExpandsCorrectionSynonyms(t *testing.T) {
	s := spellSearcher(t, func(*config.Search) {})
	s.Synonyms = ParseSynonyms([]byte("zebracorn, unicorn\nzebracron, wombat"))
	out, err := s.Search(context.Background(), Request{Query: "zebracron", Mode: ModeFTS, K: 8})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"wombat", "zebracorn", "unicorn"}; !slices.Equal(out.FTSAlts["zebracron"], want) {
		t.Errorf("alts = %v, want %v", out.FTSAlts["zebracron"], want)
	}
	if want := []string{"wombat"}; !slices.Equal(out.FTSSynonyms["zebracron"], want) {
		t.Errorf("FTSSynonyms = %v, want only the typed word's synonyms %v", out.FTSSynonyms, want)
	}
}

// TestExplainShowsSpell: the explanation carries the fix and its column scores use the rewritten
// expression (the corrected hit is re-scored).
func TestExplainShowsSpell(t *testing.T) {
	s := spellSearcher(t, func(*config.Search) {})
	ex, err := s.Explain(context.Background(), Request{Query: "zebracron", Mode: ModeFTS, K: 8})
	if err != nil {
		t.Fatal(err)
	}
	if ex.FTS == nil || len(ex.FTS.Spell) != 1 || ex.FTS.Spell[0].Correction != "zebracorn" || ex.FTS.Spell[0].Doc != 5 {
		t.Fatalf("explain fts = %+v, want the zebracorn fix with 5 chunks", ex.FTS)
	}
	if len(ex.Candidates) == 0 || ex.Candidates[0].Columns == nil || ex.Candidates[0].Columns.Text == nil {
		t.Errorf("candidate has no per-column scores: %+v", ex.Candidates)
	}
}

// TestSpellLogsRewrite: the logged fts_query is the rewritten expression.
func TestSpellLogsRewrite(t *testing.T) {
	s := spellSearcher(t, func(*config.Search) {})
	req := Request{Query: "zebracron", Mode: ModeFTS, K: 8, Caller: "t"}
	out, err := s.Search(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Record(context.Background(), req, out)
	if err != nil {
		t.Fatal(err)
	}
	tbl, err := s.Store.(store.SearchLog).QueryLog(context.Background(), `SELECT fts_query FROM searches WHERE id = ?`, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(tbl.Rows[0][0]); got != `("zebracron" OR "zebracorn")` {
		t.Errorf("logged fts_query = %s", got)
	}
}

// vocabFailStore is a Retriever with a vocabulary that fails.
type vocabFailStore struct {
	store.Retriever
	err error
}

func (v vocabFailStore) VocabTerms(context.Context, store.VocabQuery) ([]store.VocabTerm, error) {
	return nil, v.err
}
func (v vocabFailStore) DocFreq(context.Context, []string) (map[string]int, error) {
	return nil, v.err
}

// TestSpellVocabError: a vocabulary read failure fails the search with its kind (it is the
// database failing, like the FTS query would).
func TestSpellVocabError(t *testing.T) {
	s, _ := testSearcher(t)
	boom := errs.New(errs.Internal, "test", "vocab broke")
	s.Store = vocabFailStore{Retriever: s.Store, err: boom}
	_, err := s.Search(context.Background(), Request{Query: "zebracron", Mode: ModeFTS, K: 8})
	if !errors.Is(err, boom) || errs.KindOf(err) != errs.Internal {
		t.Errorf("err = %v, want the vocabulary error", err)
	}
	// A query with nothing to correct never reads the vocabulary.
	if _, err := s.Search(context.Background(), Request{Query: "gat", Mode: ModeFTS, K: 8}); err != nil {
		t.Errorf("short word read the vocabulary: %v", err)
	}
}

// typo makes one random edit to w (an insertion, deletion, substitution or transposition).
func typo(r *rand.Rand, w string) string {
	rs := []rune(w)
	i := r.Intn(len(rs))
	switch r.Intn(4) {
	case 0:
		return string(rs[:i]) + string(rune('a'+r.Intn(26))) + string(rs[i:])
	case 1:
		return string(rs[:i]) + string(rs[i+1:])
	case 2:
		rs[i] = rune('a' + r.Intn(26))
		return string(rs)
	default:
		if i+1 < len(rs) {
			rs[i], rs[i+1] = rs[i+1], rs[i]
		}
		return string(rs)
	}
}

// fusedIDs is the set of chunk ids in a search's fused list.
func fusedIDs(out *Outcome) map[int64]bool {
	set := map[int64]bool{}
	for _, c := range out.Candidates {
		set[c.ChunkID] = true
	}
	return set
}

// TestSpellNeverLosesResults is the property: with random queries of fixture words and typos,
// every chunk the search finds with spell off is still found with it on, also when the list
// limit is small enough that the rerun's new rows could crowd the first ones out.
func TestSpellNeverLosesResults(t *testing.T) {
	words := []string{"zebracorn", "widget", "enclosure", "about", "gate", "latch", "bridge", "networks", "quokka", "mounts", "entry", "grooming"}
	r := rand.New(rand.NewSource(56))
	for _, limit := range []int{2, 3, 20} {
		on := spellSearcher(t, func(c *config.Search) { c.ListLimit = limit })
		off := spellSearcher(t, func(c *config.Search) { c.ListLimit = limit; c.Spell = false })
		fired := 0
		for i := 0; i < 150; i++ {
			var fields []string
			for n := 1 + r.Intn(3); n > 0; n-- {
				w := words[r.Intn(len(words))]
				if r.Intn(2) == 0 {
					w = typo(r, w)
				}
				fields = append(fields, w)
			}
			req := Request{Query: strings.Join(fields, " "), Mode: ModeFTS, K: MaxK}
			a, err := off.Search(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			b, err := on.Search(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if len(b.Spell) > 0 {
				fired++
			}
			got := fusedIDs(b)
			for id := range fusedIDs(a) {
				if !got[id] {
					t.Fatalf("limit %d query %q: chunk %d found with spell off, lost with it on (%+v)", limit, req.Query, id, b.Spell)
				}
			}
		}
		if fired == 0 {
			t.Errorf("limit %d: spell never fired; the property tested nothing", limit)
		}
	}
}

// FuzzSpell: the distance is a metric's basics (zero only on equal strings, symmetric, within the
// length bounds, bounded form consistent), and Correct never returns a correction that breaks its
// own rules, whatever the input bytes.
func FuzzSpell(f *testing.F) {
	f.Add("kuberntes", "kubernetes")
	f.Add("café", "cafe")
	f.Add("", "abc")
	f.Add("\xff\xfe", "ab")
	f.Add("ca", "abc")
	f.Fuzz(func(t *testing.T, a, b string) {
		ra, rb := []rune(a), []rune(b)
		if len(ra) > 64 || len(rb) > 64 {
			return
		}
		d := EditDistance(a, b)
		if d != EditDistance(b, a) {
			t.Fatalf("asymmetric: %q %q", a, b)
		}
		if (d == 0) != (string(ra) == string(rb)) {
			t.Fatalf("EditDistance(%q, %q) = %d", a, b, d)
		}
		lo, hi := len(ra)-len(rb), max(len(ra), len(rb))
		if lo < 0 {
			lo = -lo
		}
		if d < lo || d > hi {
			t.Fatalf("EditDistance(%q, %q) = %d outside [%d, %d]", a, b, d, lo, hi)
		}
		for maxd := 0; maxd <= 3; maxd++ {
			got, within := editDistanceWithin(ra, rb, maxd)
			if within != (d <= maxd) || (within && got != d) {
				t.Fatalf("editDistanceWithin(%q, %q, %d) = %d, %v; full %d", a, b, maxd, got, within, d)
			}
		}
		word := strings.ToLower(a)
		fix, ok := Correct(word, []store.VocabTerm{{Term: b, Doc: 2}, {Term: strings.ToLower(b), Doc: 3}})
		if ok {
			// The range [1, MaxEdits] is Correct's own assert.Post; these are what it does not check.
			if fix.Distance != EditDistance(word, fix.Correction) || !onlyLetters(fix.Correction) {
				t.Fatalf("Correct(%q) = %+v breaks the rules", word, fix)
			}
		}
	})
}

// recordingVocab is a store.Vocab double that returns fixed counts and terms and records the
// vocabulary query.
type recordingVocab struct {
	df    map[string]int
	terms []store.VocabTerm
	got   []store.VocabQuery
	err   error // returned by VocabTerms only (DocFreq keeps working)
}

func (r *recordingVocab) VocabTerms(_ context.Context, q store.VocabQuery) ([]store.VocabTerm, error) {
	r.got = append(r.got, q)
	return r.terms, r.err
}
func (r *recordingVocab) DocFreq(_ context.Context, terms []string) (map[string]int, error) {
	out := map[string]int{}
	for _, t := range terms {
		if n, ok := r.df[t]; ok {
			out[t] = n
		}
	}
	return out, nil
}

// TestPlanSpell: the vocabulary read is bounded by the corrected words' length window, a word
// with even one document is left alone, and nothing to correct reads no vocabulary.
func TestPlanSpell(t *testing.T) {
	vocab := []store.VocabTerm{{Term: "widget", Doc: 5}, {Term: "zebracorn", Doc: 5}, {Term: "gate", Doc: 3}}
	for _, tc := range []struct {
		name   string
		fields []string
		df     map[string]int
		want   []store.VocabQuery
		fixes  string
	}{
		{"one 6-letter word: 5..7", []string{"widgte"}, nil, []store.VocabQuery{{MinDoc: 2, MinLen: 5, MaxLen: 7}}, "widgte->widget"},
		{"6 and 9 letters: 5..11", []string{"widgte", "zebracron"}, nil, []store.VocabQuery{{MinDoc: 2, MinLen: 5, MaxLen: 11}}, "widgte->widget zebracron->zebracorn"},
		{"9 then 4 letters: 3..11", []string{"zebracron", "gaet"}, nil, []store.VocabQuery{{MinDoc: 2, MinLen: 3, MaxLen: 11}}, "zebracron->zebracorn gaet->gate"},
		{"a word with one document is not corrected", []string{"widgte", "gaet"}, map[string]int{"widgte": 1}, []store.VocabQuery{{MinDoc: 2, MinLen: 3, MaxLen: 5}}, "gaet->gate"},
		{"nothing to correct reads no vocabulary", []string{"widgte"}, map[string]int{"widgte": 1}, nil, ""},
		{"no words at all", []string{"foo_bar", "ab"}, nil, nil, ""},
		{"a repeated field is corrected once", []string{"gaet", "gaet"}, nil, []store.VocabQuery{{MinDoc: 2, MinLen: 3, MaxLen: 5}}, "gaet->gate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := &recordingVocab{df: tc.df, terms: vocab}
			plan, err := (&Searcher{}).planSpell(context.Background(), v, tc.fields, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(v.got, tc.want) {
				t.Errorf("vocabulary queries = %+v, want %+v", v.got, tc.want)
			}
			var fixes []string
			for _, f := range plan.fixes {
				fixes = append(fixes, f.Term+"->"+f.Correction)
			}
			if strings.Join(fixes, " ") != tc.fixes {
				t.Errorf("fixes = %v, want %q", fixes, tc.fixes)
			}
		})
	}
}

// TestWithAlternatives: new alternatives are appended without repeats or the word itself, up to
// MaxAlternatives, and the input is never modified.
func TestWithAlternatives(t *testing.T) {
	have := []string{"a", "b"}
	if got := withAlternatives(have, "w", []string{"b", "w", "c"}); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("got %v", got)
	}
	if !slices.Equal(have, []string{"a", "b"}) {
		t.Errorf("input modified: %v", have)
	}
	var many []string
	for i := 0; i < MaxAlternatives+2; i++ {
		many = append(many, fmt.Sprintf("x%d", i))
	}
	got := withAlternatives(many[:MaxAlternatives-1], "w", many[MaxAlternatives-1:])
	if len(got) != MaxAlternatives || got[MaxAlternatives-1] != many[MaxAlternatives-1] {
		t.Errorf("cap: %d alternatives (last %q), want %d", len(got), got[len(got)-1], MaxAlternatives)
	}
}

// TestPlanSpellVocabTermsError: DocFreq answers but the vocabulary read fails; planSpell returns
// that error with its kind and no plan.
func TestPlanSpellVocabTermsError(t *testing.T) {
	boom := errs.New(errs.Internal, "test", "vocab terms broke")
	v := &recordingVocab{err: boom}
	plan, err := (&Searcher{}).planSpell(context.Background(), v, []string{"widgte"}, nil)
	if !errors.Is(err, boom) || errs.KindOf(err) != errs.Internal || len(plan.fixes) != 0 {
		t.Errorf("plan %+v, err %v; want the VocabTerms error and no fixes", plan, err)
	}
	if len(v.got) != 1 {
		t.Errorf("vocabulary reads = %d, want 1 (the failing one)", len(v.got))
	}
}

// vocabFaulty is a faultyRetriever that also serves the vocabulary of the real store, so spell
// correction is reachable while an FTS call is made to fail.
type vocabFaulty struct {
	*faultyRetriever
	v store.Vocab
}

func (f vocabFaulty) VocabTerms(ctx context.Context, q store.VocabQuery) ([]store.VocabTerm, error) {
	return f.v.VocabTerms(ctx, q)
}
func (f vocabFaulty) DocFreq(ctx context.Context, terms []string) (map[string]int, error) {
	return f.v.DocFreq(ctx, terms)
}

// TestSpellRerunError: the spelled list's second FTS call fails; the search fails with that error
// (and without it the same query corrects, so the failing call is the rerun).
func TestSpellRerunError(t *testing.T) {
	s := spellSearcher(t, func(*config.Search) {})
	real := s.Store
	req := Request{Query: "zebracron", Mode: ModeFTS, K: 8}
	ok := &faultyRetriever{Retriever: real}
	s.Store = vocabFaulty{ok, real.(store.Vocab)}
	out, err := s.Search(context.Background(), req)
	if err != nil || len(out.Spell) != 1 || len(ok.ftsOps) != 2 {
		t.Fatalf("control: err %v spell %v FTS calls %d; want a correction after 2 FTS calls", err, out.Spell, len(ok.ftsOps))
	}
	s.Store = vocabFaulty{&faultyRetriever{Retriever: real, failOn: "FTS#2"}, real.(store.Vocab)}
	if _, err := s.Search(context.Background(), req); !errors.Is(err, errDriver) {
		t.Errorf("err = %v, want the driver error from the rerun", err)
	}
}
