package search

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/store"
)

// sliceLog is a judged log held in memory that is not an *Evidence: Search reads it once per
// search and prepares it fresh, as it does the sqlite log.
type sliceLog []store.JudgedSearch

func (l sliceLog) Judged(context.Context, store.JudgedFilter) ([]store.JudgedSearch, error) {
	return l, nil
}

// evidenceSearcher searches memStore with [prior] on at min_judged 0 (the gate is open whatever
// the count), max_boost 0.2 and the query memory at weight 2, reading prior from src.
func evidenceSearcher(t testing.TB, st store.Retriever, src store.JudgedLog) *Searcher {
	t.Helper()
	cfg := config.Default()
	cfg.Prior = config.Prior{Enabled: true, MinJudged: 0, HalfLifeDays: 90, MaxBoost: 0.2, QueryMemory: true}
	cfg.Search.ListWeights.Memory = 2
	settings, err := NewSettings(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &Searcher{Store: st, Prior: src, Settings: settings, Now: func() time.Time { return priorNow }}
}

// sameOutcome fails unless two searches agree on the prior and memory states and on every
// candidate's chunk, score, prior and memory rank, in order.
func sameOutcome(t *testing.T, label string, a, b *Outcome) {
	t.Helper()
	if a.Prior != b.Prior || a.Memory != b.Memory {
		t.Fatalf("%s: prior %+v memory %+v; fresh read gives %+v %+v", label, a.Prior, a.Memory, b.Prior, b.Memory)
	}
	type row struct {
		id    int64
		score float64
		prior float64
		mem   int
	}
	rows := func(o *Outcome) []row {
		var out []row
		for _, c := range o.Candidates {
			r := row{id: c.ChunkID, score: c.RRFScore, prior: -1, mem: -1}
			if c.Prior != nil {
				r.prior = *c.Prior
			}
			if c.MemoryRank != nil {
				r.mem = *c.MemoryRank
			}
			out = append(out, r)
		}
		return out
	}
	if ra, rb := rows(a), rows(b); !reflect.DeepEqual(ra, rb) {
		t.Fatalf("%s: candidates %+v; fresh read gives %+v", label, ra, rb)
	}
}

// TestEvidenceLeakage is the replay's leakage table at the search (PLAN 6.4): S is the replayed
// search (asOf = its time T, ExcludeEvidence = S), O an earlier search with the same memory key.
// A verdict or an open counts for the prior and the query memory only when it is on another
// search and recorded strictly before T; at T, after T, or on S itself (at any time) it does not.
// Each row also runs through a fresh read of the log (sliceLog), which must agree.
func TestEvidenceLeakage(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	T := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	const sID, oID = 10, 5
	for _, tc := range []struct {
		name   string
		on     int64     // the search carrying the evidence
		open   bool      // an open of hidden.md, else a useful verdict on hidden.md › Tuning
		at     time.Time // when it was recorded
		judged int       // judged searches the prior counts
	}{
		{"verdict on O before T", oID, false, T.Add(-time.Second), 1},
		{"verdict on O at T", oID, false, T, 0},
		{"verdict on O after T", oID, false, T.Add(time.Second), 0},
		{"open on O before T", oID, true, T.Add(-time.Second), 1},
		{"open on O at T", oID, true, T, 0},
		{"open on O after T", oID, true, T.Add(time.Second), 0},
		{"verdict on S before T (excluded)", sID, false, T.Add(-time.Second), 0},
		{"verdict on S after T", sID, false, T.Add(time.Second), 0},
		{"open on S before T (excluded)", sID, true, T.Add(-time.Second), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := []store.JudgedSearch{
				{ID: oID, TS: T.Add(-time.Hour).Format(time.RFC3339), Query: "Calibration sprocket"},
				{ID: sID, TS: T.Format(time.RFC3339), Query: memQuery},
			}
			j := &log[0]
			if tc.on == sID {
				j = &log[1]
			}
			if tc.open {
				j.Opens, j.OpenedAt = []string{"hidden.md"}, map[string]time.Time{"hidden.md": tc.at}
			} else {
				j.Verdicts = []store.Verdict{{Rank: 1, Useful: true, At: tc.at, EntryPath: "hidden.md", Heading: "Tuning"}}
			}
			req := Request{Query: memQuery, Mode: ModeFTS, AsOf: T, ExcludeEvidence: []int64{sID}}
			out, err := evidenceSearcher(t, st, NewEvidence(log)).Search(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			if out.Prior.Judged != tc.judged || !out.Prior.Active {
				t.Errorf("Prior = %+v, want active with %d judged", out.Prior, tc.judged)
			}
			wantItems := tc.judged // the evidence names hidden.md, which only memory retrieves
			if out.Memory.Items != wantItems || out.Memory.Searches != tc.judged {
				t.Errorf("Memory = %+v, want %d items from %d searches", out.Memory, wantItems, tc.judged)
			}
			if got := slices.Contains(paths(out), "hidden.md"); got != (tc.judged > 0) {
				t.Errorf("hidden.md among the candidates = %v, want %v: %v", got, tc.judged > 0, paths(out))
			}
			fresh, err := evidenceSearcher(t, st, sliceLog(log)).Search(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			sameOutcome(t, tc.name, out, fresh)
		})
	}
}

// randomLog is n judged searches over memStore's entries: queries from a few memory keys, verdicts
// and opens at random times around base (some before their search, as a damaged clock could).
func randomLog(rng *rand.Rand, n int, base time.Time) []store.JudgedSearch {
	queries := []string{memQuery, "Calibration, sprocket", "sprocket", "gizmo tuning", "steps"}
	targets := []struct{ path, heading string }{{"old.md", ""}, {"new.md", ""}, {"other.md", ""}, {"hidden.md", "Tuning"}, {"hidden.md", ""}, {"gone.md", "X"}}
	at := func() time.Time { return base.Add(time.Duration(rng.Intn(200)-100) * time.Hour) }
	var log []store.JudgedSearch
	for id := range n {
		j := store.JudgedSearch{ID: int64(id + 1), TS: at().Format(time.RFC3339), Query: queries[rng.Intn(len(queries))]}
		for r := range rng.Intn(3) {
			tg := targets[rng.Intn(len(targets))]
			v := store.Verdict{Rank: r + 1 + rng.Intn(5), Useful: rng.Intn(3) > 0, At: at(), EntryPath: tg.path, Heading: tg.heading}
			if rng.Intn(6) == 0 {
				v.Store = "other@/x"
			}
			j.Verdicts = append(j.Verdicts, v)
		}
		if rng.Intn(3) == 0 {
			tg := targets[rng.Intn(len(targets))]
			j.Opens, j.OpenedAt = []string{tg.path}, map[string]time.Time{tg.path: at()}
		}
		log = append(log, j)
	}
	return log
}

// before is the log as a fresh read would see it at asOf without the excluded searches: those
// searches dropped, and every verdict and open recorded at or after asOf removed.
func before(log []store.JudgedSearch, asOf time.Time, exclude []int64) []store.JudgedSearch {
	var out []store.JudgedSearch
	for _, j := range log {
		if slices.Contains(exclude, j.ID) {
			continue
		}
		k := store.JudgedSearch{ID: j.ID, TS: j.TS, Query: j.Query, OpenedAt: map[string]time.Time{}}
		for _, v := range j.Verdicts {
			if v.At.Before(asOf) {
				k.Verdicts = append(k.Verdicts, v)
			}
		}
		for _, p := range j.Opens {
			if j.OpenedAt[p].Before(asOf) {
				k.Opens = append(k.Opens, p)
				k.OpenedAt[p] = j.OpenedAt[p]
			}
		}
		out = append(out, k)
	}
	return out
}

// TestEvidenceSliceEqualsFreshRead is the slicing property (Q133), seeded: for random logs, one
// Evidence sliced as of many instants and exclusions gives the same search (prior and memory
// states, every candidate's score, multiplier and memory rank) as a fresh read of only the
// events before that instant, without the excluded searches.
func TestEvidenceSliceEqualsFreshRead(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	rng := rand.New(rand.NewSource(613))
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	queries := []string{memQuery, "sprocket", "gizmo tuning"}
	for round := range 30 {
		log := randomLog(rng, 1+rng.Intn(40), base)
		ev := NewEvidence(log)
		for range 5 {
			asOf := base.Add(time.Duration(rng.Intn(240)-120) * time.Hour)
			var exclude []int64
			for range rng.Intn(4) {
				exclude = append(exclude, int64(1+rng.Intn(len(log)+2)))
			}
			q := queries[rng.Intn(len(queries))]
			sliced, err := evidenceSearcher(t, st, ev).Search(ctx, Request{Query: q, Mode: ModeFTS, AsOf: asOf, ExcludeEvidence: exclude})
			if err != nil {
				t.Fatal(err)
			}
			fresh, err := evidenceSearcher(t, st, sliceLog(before(log, asOf, exclude))).Search(ctx, Request{Query: q, Mode: ModeFTS, AsOf: asOf})
			if err != nil {
				t.Fatal(err)
			}
			sameOutcome(t, fmt.Sprintf("round %d, %q as of %v without %v", round, q, asOf, exclude), sliced, fresh)
		}
	}
}

// TestEvidenceSingleRead (Q133): a log prepared once serves any number of searches without
// another read; a Prior that is not an *Evidence is read once per search.
func TestEvidenceSingleRead(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	logUseful(t, st, 2, memQuery, "hidden.md", "Tuning")
	log := &countingLog{JudgedLog: st}
	judged, err := log.Judged(ctx, store.JudgedFilter{})
	if err != nil {
		t.Fatal(err)
	}
	pre := evidenceSearcher(t, st, NewEvidence(judged))
	for i := range 5 {
		out, err := pre.Search(ctx, Request{Query: memQuery, Mode: ModeFTS, AsOf: priorNow.Add(time.Duration(i) * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		if !out.Prior.Active || out.Memory.Items != 1 {
			t.Fatalf("search %d: prior %+v memory %+v, want active with the remembered hit", i, out.Prior, out.Memory)
		}
	}
	if got := log.n.Load(); got != 1 {
		t.Errorf("Judged read %d times for 5 searches on prepared evidence, want 1", got)
	}
	perSearch := evidenceSearcher(t, st, log)
	for range 3 {
		if _, err := perSearch.Search(ctx, Request{Query: memQuery, Mode: ModeFTS}); err != nil {
			t.Fatal(err)
		}
	}
	if got := log.n.Load(); got != 4 {
		t.Errorf("Judged read %d times in all, want 1 + 3 (one per search on the store's log)", got)
	}
}

// TestEvidenceJudged: *Evidence is a store.JudgedLog over the prepared searches; the filter keeps
// a caller's and those at or after Since; a search whose time does not parse is kept only
// without Since.
func TestEvidenceJudged(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	log := []store.JudgedSearch{
		{ID: 1, TS: at.Format(time.RFC3339), Caller: "a"},
		{ID: 2, TS: at.Add(time.Hour).Format(time.RFC3339), Caller: "b"},
		{ID: 3, TS: "yesterday", Caller: "a"},
	}
	ev := NewEvidence(log)
	for _, tc := range []struct {
		name string
		f    store.JudgedFilter
		want []int64
	}{
		{"no filter", store.JudgedFilter{}, []int64{1, 2, 3}},
		{"caller a", store.JudgedFilter{Caller: "a"}, []int64{1, 3}},
		{"since the first", store.JudgedFilter{Since: at}, []int64{1, 2}},
		{"since a second later", store.JudgedFilter{Since: at.Add(time.Second)}, []int64{2}},
		{"caller b since the first", store.JudgedFilter{Since: at, Caller: "b"}, []int64{2}},
		{"unknown caller", store.JudgedFilter{Caller: "z"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ev.Judged(context.Background(), tc.f)
			if err != nil {
				t.Fatal(err)
			}
			var ids []int64
			for _, j := range got {
				ids = append(ids, j.ID)
			}
			if !slices.Equal(ids, tc.want) {
				t.Errorf("ids %v, want %v", ids, tc.want)
			}
		})
	}
}
