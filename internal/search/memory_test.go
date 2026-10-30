package search

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/store/sqlite"
)

// TestMemoryKey: the key is the lowercase, sorted, unique words the FTS list would search.
func TestMemoryKey(t *testing.T) {
	for _, tc := range []struct{ query, want string }{
		{"sprocket calibration", "calibration sprocket"},
		{"Calibration SPROCKET", "calibration sprocket"},
		{"sprocket sprocket calibration", "calibration sprocket"},
		{"  sprocket\tcalibration\n", "calibration sprocket"},
		{"draw-visual", "draw visual"},
		{"visual draw", "draw visual"},
		{"sprocket*", "sprocket"},
		{`"sprocket"`, "sprocket"},
		{"sprocket AND calibration", "calibration sprocket"}, // the operator is a field of its own...
		{"or", ""}, // ...and a lone operator vanishes
		{"--- !!!", ""},
		{"", ""},
		{"résumé", "résumé"},
		{"a*b", "ab"}, // the prefix star is deleted, as the MATCH expression does
		{"a\x00b", "a b"},
		{`a"b`, "a b"},
		{"and", ""},
	} {
		if got := MemoryKey(tc.query); got != tc.want {
			t.Errorf("MemoryKey(%q) = %q, want %q", tc.query, got, tc.want)
		}
	}
}

// FuzzMemoryKey: the key depends on the set of words only. It does not change when the fields
// are reordered or repeated, or (for ASCII) when the case changes, and it is the sorted, unique,
// lowercase words joined by one space. It is not idempotent on purpose: "x-and" keys to "and x",
// and a lone "and" is an operator that the FTS list (and so the key) drops.
func FuzzMemoryKey(f *testing.F) {
	for _, s := range []string{"sprocket calibration", "Calibration, SPROCKET!", "draw-visual visual draw", "x-and", "or",
		"a*b", "a\x00b", `a"b`, "résumé RÉSUMÉ", "İstanbul", "straße STRASSE", "--- !!!", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, q string) {
		key := MemoryKey(q)
		fields := strings.Fields(q)
		rev := slices.Clone(fields)
		slices.Reverse(rev)
		if got := MemoryKey(strings.Join(rev, " ")); got != key {
			t.Fatalf("reordered %q: %q, want %q", q, got, key)
		}
		if got := MemoryKey(q + " " + q); got != key {
			t.Fatalf("repeated %q: %q, want %q", q, got, key)
		}
		if isASCII(q) {
			if got := MemoryKey(strings.ToUpper(q)); got != key {
				t.Fatalf("upper %q: %q, want %q", q, got, key)
			}
		}
		if key == "" {
			return
		}
		words := strings.Split(key, " ")
		for i, w := range words {
			if w == "" || strings.ToLower(w) != w || strings.ContainsFunc(w, unicode.IsSpace) {
				t.Fatalf("key %q of %q: word %q is empty, not lowercase or holds a space", key, q, w)
			}
			if i > 0 && words[i-1] >= w {
				t.Fatalf("key %q of %q: words not sorted and unique", key, q)
			}
		}
	})
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// memStore is supersedeStore plus hidden.md, which the query "sprocket calibration" never
// retrieves: a summary chunk and a "Tuning" chunk.
func memStore(t *testing.T) *sqlite.Store {
	t.Helper()
	st := supersedeStore(t, nil)
	in := store.EntryInput{Path: "hidden.md", Kind: "file", Title: "hidden.md"}
	if _, err := st.ReplaceEntry(context.Background(), in, []store.ChunkInput{
		{Ord: 0, SourceFile: "hidden.md", Text: "gizmo overview", TextHash: "h0"},
		{Ord: 1, SourceFile: "hidden.md", Heading: "Tuning", Text: "gizmo tuning notes", TextHash: "h1"},
	}); err != nil {
		t.Fatal(err)
	}
	return st
}

// logUseful logs n searches for query that returned (path, heading) at rank 1 and marks it useful.
func logUseful(t *testing.T, st *sqlite.Store, n int, query, path, heading string) {
	t.Helper()
	ctx := context.Background()
	for range n {
		id, err := st.LogSearch(ctx, store.SearchRecord{TS: priorEvidenceAt, Query: query, Mode: "fts", K: 8, Caller: "c",
			Results: []store.LoggedHit{{ChunkID: 1, EntryPath: path, Heading: heading, RRFScore: 1}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Feedback(ctx, id, 1, true, "", priorEvidenceAt.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
}

// memSearcher is priorSearcher with [prior] query_memory and the memory list weight as given.
func memSearcher(t *testing.T, st *sqlite.Store, memory bool, weight float64, minJudged int) *Searcher {
	t.Helper()
	cfg := config.Default()
	cfg.Prior = config.Prior{Enabled: true, MinJudged: minJudged, HalfLifeDays: 90, MaxBoost: 0, QueryMemory: memory}
	cfg.Search.ListWeights.Memory = weight
	settings, err := NewSettings(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &Searcher{Store: st, Log: st, Prior: st, Settings: settings, Now: func() time.Time { return priorNow }}
}

const memQuery = "sprocket calibration"

func chunkHeading(out *Outcome, i int) (string, string) {
	return out.Candidates[i].EntryPath, out.Candidates[i].Heading
}

// TestMemoryList: a repeated query (any order, case or punctuation of the same words) surfaces its
// previously useful hit, even one that no other list retrieved, at rank 1; another key does not.
func TestMemoryList(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	logUseful(t, st, 2, "Calibration, sprocket!", "hidden.md", "Tuning")
	s := memSearcher(t, st, true, 2, 1) // weight 2: a memory-only hit outscores an FTS rank 1 (weight 1)
	base, err := memSearcher(t, st, false, 1, 1).Search(ctx, Request{Query: memQuery, Mode: ModeFTS})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(paths(base), "hidden.md") {
		t.Fatalf("hidden.md is retrieved without memory: %v", paths(base))
	}

	out, err := s.Search(ctx, Request{Query: "calibration SPROCKET", Mode: ModeFTS})
	if err != nil {
		t.Fatal(err)
	}
	if p, h := chunkHeading(out, 0); p != "hidden.md" || h != "Tuning" {
		t.Fatalf("top candidate = %s › %s, want hidden.md › Tuning from memory:\n%v", p, h, paths(out))
	}
	c := out.Candidates[0]
	if c.MemoryRank == nil || *c.MemoryRank != 1 || c.MemoryScore == nil || *c.MemoryScore <= 0 || c.FTSRank != nil {
		t.Errorf("candidate memory = %v/%v fts %v, want memory rank 1 with u > 0 and no FTS rank", c.MemoryRank, c.MemoryScore, c.FTSRank)
	}
	if m := out.Memory; !m.Active || m.Key != "calibration sprocket" || m.Searches != 2 || m.Items != 1 || m.Skipped != 0 {
		t.Errorf("Memory = %+v", m)
	}
	if out.Result.Hits[0].MemoryRank == nil || *out.Result.Hits[0].MemoryRank != 1 {
		t.Errorf("hit 0 memory_rank = %v, want 1", out.Result.Hits[0].MemoryRank)
	}

	// Another key remembers nothing and ranks as without memory.
	other, err := s.Search(ctx, Request{Query: "sprocket", Mode: ModeFTS})
	if err != nil {
		t.Fatal(err)
	}
	if m := other.Memory; !m.Active || m.Items != 0 || m.Searches != 0 {
		t.Errorf("different key: Memory = %+v, want active and empty", m)
	}
	want, err := memSearcher(t, st, false, 1, 1).Search(ctx, Request{Query: "sprocket", Mode: ModeFTS})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(paths(other), paths(want)) {
		t.Errorf("different key order = %v, want %v", paths(other), paths(want))
	}
}

// TestMemoryAsOf: feedback recorded at or after asOf is not remembered.
func TestMemoryAsOf(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	logUseful(t, st, 1, memQuery, "hidden.md", "Tuning")
	for _, tc := range []struct {
		name  string
		asOf  time.Time
		items int
		on    bool
	}{
		{"after the feedback", priorEvidenceAt.Add(time.Minute + time.Second), 1, true},
		{"at the feedback instant", priorEvidenceAt.Add(time.Minute), 0, false}, // min_judged 1 is no longer met
		{"before the feedback", priorEvidenceAt, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := memSearcher(t, st, true, 1, 1).Search(ctx, Request{Query: memQuery, Mode: ModeFTS, AsOf: tc.asOf})
			if err != nil {
				t.Fatal(err)
			}
			if out.Memory.Active != tc.on || out.Memory.Items != tc.items {
				t.Errorf("Memory = %+v, want active=%v items=%d", out.Memory, tc.on, tc.items)
			}
		})
	}
	// With min_judged 0 the gate stays open, and still the later feedback is not remembered.
	out, err := memSearcher(t, st, true, 1, 0).Search(ctx, Request{Query: memQuery, Mode: ModeFTS, AsOf: priorEvidenceAt})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Memory.Active || out.Memory.Items != 0 || out.Memory.Searches != 0 {
		t.Errorf("min_judged 0, asOf before the feedback: Memory = %+v, want active and empty", out.Memory)
	}
}

// hideLocator is a Retriever that does not implement store.ChunkLocator.
type hideLocator struct{ store.Retriever }

// TestMemoryGate: the memory list runs only when the prior's gate is open and query_memory is on,
// the list weight is above 0 and the store can locate chunks; every other row ranks as without it.
func TestMemoryGate(t *testing.T) {
	const minJudged = 2
	for _, tc := range []struct {
		name       string
		memory     bool
		weight     float64
		judged     int
		noPrior    bool
		noLocator  bool
		active     bool
		inactive   string
		wantHidden bool
	}{
		{"on", true, 1, minJudged, false, false, true, "", true},
		{"query_memory off", false, 1, minJudged, false, false, false, "", false},
		{"below min_judged", true, 1, minJudged - 1, false, false, false, "memory inactive: 1/2 judged searches", false},
		{"--no-prior", true, 1, minJudged, true, false, false, "memory inactive: --no-prior", false},
		{"weight 0", true, 0, minJudged, false, false, false, "memory inactive: search.list_weights.memory is 0", false},
		{"no locator", true, 1, minJudged, false, true, false, "memory inactive: the store cannot locate chunks", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := memStore(t)
			logUseful(t, st, tc.judged, memQuery, "hidden.md", "Tuning")
			s := memSearcher(t, st, tc.memory, tc.weight, minJudged)
			if tc.noLocator {
				s.Store = hideLocator{st}
			}
			base, err := memSearcher(t, st, false, 1, minJudged).Search(ctx, Request{Query: memQuery, Mode: ModeFTS, NoPrior: true})
			if err != nil {
				t.Fatal(err)
			}
			out, err := s.Search(ctx, Request{Query: memQuery, Mode: ModeFTS, NoPrior: tc.noPrior})
			if err != nil {
				t.Fatal(err)
			}
			if out.Memory.Enabled != tc.memory || out.Memory.Active != tc.active || out.Memory.Inactive() != tc.inactive {
				t.Errorf("Memory = %+v (inactive %q), want active=%v inactive %q", out.Memory, out.Memory.Inactive(), tc.active, tc.inactive)
			}
			if got := slices.Contains(paths(out), "hidden.md"); got != tc.wantHidden {
				t.Errorf("hidden.md in candidates = %v, want %v", got, tc.wantHidden)
			}
			if !tc.wantHidden && !slices.Equal(paths(out), paths(base)) {
				t.Errorf("order = %v, want unchanged %v", paths(out), paths(base))
			}
		})
	}
}

// TestMemorySkipsGone: a remembered hit whose entry or heading is no longer indexed is skipped
// and counted; the others still rank.
func TestMemorySkipsGone(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	logUseful(t, st, 1, memQuery, "hidden.md", "Tuning")
	logUseful(t, st, 1, memQuery, "hidden.md", "Renamed away") // the heading no longer exists
	logUseful(t, st, 1, memQuery, "gone.md", "")               // the entry was deleted
	out, err := memSearcher(t, st, true, 2, 1).Search(ctx, Request{Query: memQuery, Mode: ModeFTS})
	if err != nil {
		t.Fatal(err)
	}
	if m := out.Memory; m.Items != 1 || m.Skipped != 2 || m.Searches != 3 {
		t.Errorf("Memory = %+v, want 1 item, 2 skipped, 3 searches", m)
	}
	if p, h := chunkHeading(out, 0); p != "hidden.md" || h != "Tuning" {
		t.Errorf("top = %s › %s", p, h)
	}
	if err := st.DeleteEntry(ctx, "hidden.md"); err != nil {
		t.Fatal(err)
	}
	out, err = memSearcher(t, st, true, 1, 1).Search(ctx, Request{Query: memQuery, Mode: ModeFTS})
	if err != nil {
		t.Fatal(err)
	}
	if m := out.Memory; m.Items != 0 || m.Skipped != 3 || slices.Contains(paths(out), "hidden.md") {
		t.Errorf("after delete: Memory = %+v, paths %v", m, paths(out))
	}
}

// TestMemoryRankingAndOpens: remembered hits rank by u (more, newer and lower-ranked-position
// evidence first); an entry only opened is remembered by its first chunk; a tag filter excludes
// what the tag does not cover.
func TestMemoryRankingAndOpens(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	logUseful(t, st, 3, memQuery, "hidden.md", "Tuning")
	logUseful(t, st, 1, memQuery, "old.md", "")
	id, err := st.LogSearch(ctx, store.SearchRecord{TS: priorEvidenceAt, Query: memQuery, Mode: "fts", K: 8, Caller: "c",
		Results: []store.LoggedHit{{ChunkID: 1, EntryPath: "other.md", RRFScore: 1}}})
	if err != nil || id == 0 {
		t.Fatal(id, err)
	}
	if ok, err := st.RecordOpen(ctx, store.OpenRecord{EntryPath: "other.md", At: priorEvidenceAt.Add(time.Minute), Window: time.Hour}); err != nil || !ok {
		t.Fatalf("RecordOpen: %v %v", ok, err)
	}
	out, err := memSearcher(t, st, true, 1, 1).Search(ctx, Request{Query: memQuery, Mode: ModeFTS})
	if err != nil {
		t.Fatal(err)
	}
	var mem []string
	byMem := map[int]Candidate{}
	for _, c := range out.Candidates {
		if c.MemoryRank != nil {
			byMem[*c.MemoryRank] = c
		}
	}
	for r := 1; r <= len(byMem); r++ {
		mem = append(mem, byMem[r].EntryPath)
	}
	if want := []string{"hidden.md", "old.md", "other.md"}; !slices.Equal(mem, want) {
		t.Errorf("memory list = %v, want %v (3 useful, 1 useful, 1 open at half weight)", mem, want)
	}
	if m := out.Memory; m.Searches != 5 || m.Items != 3 {
		t.Errorf("Memory = %+v", m)
	}
}

// countingLog counts Judged reads.
type countingLog struct {
	store.JudgedLog
	n atomic.Int32
}

func (c *countingLog) Judged(ctx context.Context, f store.JudgedFilter) ([]store.JudgedSearch, error) {
	c.n.Add(1)
	return c.JudgedLog.Judged(ctx, f)
}

// TestMemorySingleRead (Q130): the prior and the memory list share one read of the judged log per
// search; a closed gate reads none.
func TestMemorySingleRead(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	logUseful(t, st, 2, memQuery, "hidden.md", "Tuning")
	for _, tc := range []struct {
		name    string
		memory  bool
		noPrior bool
		reads   int32
	}{
		{"prior and memory", true, false, 1},
		{"prior only", false, false, 1},
		{"--no-prior", true, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := memSearcher(t, st, tc.memory, 1, 1)
			log := &countingLog{JudgedLog: st}
			s.Prior = log
			if _, err := s.Search(ctx, Request{Query: memQuery, Mode: ModeFTS, NoPrior: tc.noPrior}); err != nil {
				t.Fatal(err)
			}
			if got := log.n.Load(); got != tc.reads {
				t.Errorf("Judged called %d times, want %d", got, tc.reads)
			}
		})
	}
}

// TestMemoryExplain: explain shows the key, the list's size and each candidate's rank, u and RRF term.
func TestMemoryExplain(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	logUseful(t, st, 1, memQuery, "hidden.md", "Tuning")
	ex, err := memSearcher(t, st, true, 2, 1).Explain(ctx, Request{Query: memQuery, Mode: ModeFTS})
	if err != nil {
		t.Fatal(err)
	}
	if m := ex.Memory; m == nil || !m.Active || m.Key != "calibration sprocket" || m.Weight != 2 || m.Items != 1 || m.Searches != 1 {
		t.Fatalf("Memory = %+v", m)
	}
	h := ex.Candidates[0]
	if h.Path != "hidden.md" || h.Memory == nil || h.Memory.Rank != 1 || h.Memory.U <= 0 || len(h.RRF) != 1 ||
		h.RRF[0].List != "memory" || h.RRF[0].Weight != 2 || h.RRF[0].Term != 2.0/61 {
		t.Errorf("candidate 0 = %+v (memory %+v)", h, h.Memory)
	}
	off, err := memSearcher(t, st, false, 2, 1).Explain(ctx, Request{Query: memQuery, Mode: ModeFTS})
	if err != nil || off.Memory != nil {
		t.Errorf("query_memory off: Memory = %+v, %v; want nil", off.Memory, err)
	}
}

// TestFuseMemory: a memory row scores weight/(k+rank); weight 0 adds the chunk with no score; the
// memory rank breaks ties after the other lists'.
func TestFuseMemory(t *testing.T) {
	p := DefaultParams()
	p.MemoryWeight = 2
	got := fuse([]FTSRow{{ChunkID: 1}}, nil, nil, []MemoryRow{{ChunkID: 2, Score: 1}, {ChunkID: 1, Score: 0.5}}, p)
	if len(got) != 2 || got[0].ChunkID != 1 || got[1].ChunkID != 2 {
		t.Fatalf("order = %+v", got)
	}
	if want := 1.0/61 + 2.0/62; got[0].RRFScore != want || *got[0].MemoryRank != 2 || *got[1].MemoryRank != 1 {
		t.Errorf("chunk 1 = %+v, want score %v, memory rank 2", got[0], want)
	}
	p.MemoryWeight = 0
	if z := fuse(nil, nil, nil, []MemoryRow{{ChunkID: 3}}, p); len(z) != 1 || z[0].RRFScore != 0 || z[0].MemoryRank == nil {
		t.Errorf("weight 0: %+v", z)
	}
	// Equal scores (weight 0): the better memory rank first, whatever the chunk ids.
	if z := fuse(nil, nil, nil, []MemoryRow{{ChunkID: 9}, {ChunkID: 4}}, p); z[0].ChunkID != 9 || z[1].ChunkID != 4 {
		t.Errorf("memory rank tie-break: %+v", z)
	}
}

// failLocator is a ChunkLocator that fails.
type failLocator struct{ store.Retriever }

func (failLocator) LocateChunks(context.Context, []store.ChunkRef, string) (map[store.ChunkRef]int64, error) {
	return nil, errs.New(errs.Unavailable, "test", "locate failed")
}

// TestMemoryLookupFailure: a failing chunk lookup fails the search with the lookup's kind.
func TestMemoryLookupFailure(t *testing.T) {
	st := memStore(t)
	logUseful(t, st, 1, memQuery, "hidden.md", "Tuning")
	s := memSearcher(t, st, true, 2, 1)
	s.Store = failLocator{st}
	if _, err := s.Search(context.Background(), Request{Query: memQuery, Mode: ModeFTS}); errs.KindOf(err) != errs.Unavailable {
		t.Errorf("err = %v (kind %v), want Unavailable", err, errs.KindOf(err))
	}
}

// TestRememberedGold is the query-memory decision over values: which judged evidence is
// remembered, and in what order. Every search is at priorEvidenceAt; verdicts and opens a minute
// later unless the row says otherwise; asOf is priorNow.
func TestRememberedGold(t *testing.T) {
	at := priorEvidenceAt.Add(time.Minute)
	useful := func(id int64, q, path, heading string, rank int) store.JudgedSearch {
		return store.JudgedSearch{ID: id, Query: q, Verdicts: []store.Verdict{{Rank: rank, Useful: true, At: at, EntryPath: path, Heading: heading}}}
	}
	opened := func(id int64, q string, paths ...string) store.JudgedSearch {
		j := store.JudgedSearch{ID: id, Query: q, Opens: paths, OpenedAt: map[string]time.Time{}}
		for _, p := range paths {
			j.OpenedAt[p] = at
		}
		return j
	}
	withVerdict := func(j store.JudgedSearch, f func(*store.Verdict)) store.JudgedSearch {
		f(&j.Verdicts[0])
		return j
	}
	many := func(n int) []store.JudgedSearch {
		var out []store.JudgedSearch
		for i := range n {
			out = append(out, useful(int64(i+1), memQuery, fmt.Sprintf("g%03d.md", i), "", 1))
		}
		return out
	}
	key := MemoryKey(memQuery)
	for _, tc := range []struct {
		name     string
		judged   []store.JudgedSearch
		want     []string // "path›heading", "path›*" for any heading; nil = none
		searches int
	}{
		{"no evidence", nil, nil, 0},
		{"another key is not remembered", []store.JudgedSearch{useful(1, "sprocket", "a.md", "H", 1)}, nil, 0},
		{"same words in any order, case or punctuation", []store.JudgedSearch{useful(1, "Calibration, SPROCKET!", "a.md", "H", 1)}, []string{"a.md›H"}, 1},
		{"another store's verdict is not evidence", []store.JudgedSearch{withVerdict(useful(1, memQuery, "a.md", "H", 1), func(v *store.Verdict) { v.Store = "x@/y" })}, nil, 0},
		{"a verdict at asOf is not yet evidence", []store.JudgedSearch{withVerdict(useful(1, memQuery, "a.md", "H", 1), func(v *store.Verdict) { v.At = priorNow })}, nil, 0},
		{"a verdict just before asOf is", []store.JudgedSearch{withVerdict(useful(1, memQuery, "a.md", "H", 1), func(v *store.Verdict) { v.At = priorNow.Add(-time.Nanosecond) })}, []string{"a.md›H"}, 1},
		{"a not-useful verdict counts the search but is no memory", []store.JudgedSearch{withVerdict(useful(1, memQuery, "a.md", "H", 1), func(v *store.Verdict) { v.Useful = false })}, nil, 1},
		{"an entry only opened is remembered by any heading", []store.JudgedSearch{opened(1, memQuery, "b.md")}, []string{"b.md›*"}, 1},
		{"an opened entry judged useful is remembered once, by its heading", []store.JudgedSearch{useful(1, memQuery, "a.md", "H", 1), opened(2, memQuery, "a.md")}, []string{"a.md›H"}, 2},
		{"more evidence first", []store.JudgedSearch{useful(1, memQuery, "a.md", "", 1), useful(2, memQuery, "b.md", "", 1), useful(3, memQuery, "b.md", "", 1)}, []string{"b.md›", "a.md›"}, 3},
		{"a useful verdict outranks an open (half weight)", []store.JudgedSearch{opened(1, memQuery, "a.md"), useful(2, memQuery, "b.md", "", 1)}, []string{"b.md›", "a.md›*"}, 2},
		{"equal u: path, then heading", []store.JudgedSearch{useful(1, memQuery, "b.md", "", 1), useful(2, memQuery, "a.md", "Z", 1), useful(3, memQuery, "a.md", "", 1)}, []string{"a.md›", "a.md›Z", "b.md›"}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			golds, searches, err := rememberedGold(tc.judged, key, DefaultParams().Prior, priorNow)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, g := range golds {
				h := g.ref.Heading
				if g.ref.AnyHeading {
					h = "*"
				}
				got = append(got, g.ref.Path+"›"+h)
				if !(g.u > 0) {
					t.Errorf("%s: u = %v, want > 0", g.ref.Path, g.u)
				}
			}
			if !slices.Equal(got, tc.want) || searches != tc.searches {
				t.Errorf("golds = %v, searches %d; want %v, %d", got, searches, tc.want, tc.searches)
			}
		})
	}
	// The lookup bound: memoryMaxGold golds are kept as they are, one more is cut.
	for _, n := range []int{memoryMaxGold, memoryMaxGold + 1} {
		golds, _, err := rememberedGold(many(n), key, DefaultParams().Prior, priorNow)
		if err != nil || len(golds) != memoryMaxGold || golds[len(golds)-1].ref.Path != fmt.Sprintf("g%03d.md", memoryMaxGold-1) {
			t.Errorf("%d golds: kept %d (last %v), err %v; want the first %d by path", n, len(golds), golds[len(golds)-1].ref, err, memoryMaxGold)
		}
	}
}

// TestMemoryRowsOf: golds map to rows in gold order; one with no chunk is skipped and counted;
// the list stops at limit, and a gold past the limit is not counted as skipped.
func TestMemoryRowsOf(t *testing.T) {
	a, b, c, gone := store.ChunkRef{Path: "a"}, store.ChunkRef{Path: "b"}, store.ChunkRef{Path: "c", AnyHeading: true}, store.ChunkRef{Path: "gone"}
	golds := []memoryGold{{a, 3}, {gone, 2.5}, {b, 2}, {c, 1}}
	ids := map[store.ChunkRef]int64{a: 10, b: 20, c: 30}
	for _, tc := range []struct {
		name    string
		limit   int
		want    []MemoryRow
		skipped int
	}{
		{"all fit", 10, []MemoryRow{{10, 3}, {20, 2}, {30, 1}}, 1},
		{"limit equals the resolvable count", 3, []MemoryRow{{10, 3}, {20, 2}, {30, 1}}, 1},
		{"limit below it", 2, []MemoryRow{{10, 3}, {20, 2}}, 1},
		{"limit 1: the skip before the cut still counts", 1, []MemoryRow{{10, 3}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, skipped := memoryRowsOf(golds, ids, tc.limit)
			if !slices.Equal(rows, tc.want) || skipped != tc.skipped {
				t.Errorf("rows %v, skipped %d; want %v, %d", rows, skipped, tc.want, tc.skipped)
			}
		})
	}
	if rows, skipped := memoryRowsOf(nil, ids, 5); rows != nil || skipped != 0 {
		t.Errorf("no golds: %v, %d", rows, skipped)
	}
}
