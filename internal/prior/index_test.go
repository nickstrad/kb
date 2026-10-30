package prior

import (
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"time"
)

// sameModel fails unless a and b answer every query the same, bit for bit, on the keys of evs
// plus one key no event has.
func sameModel(t *testing.T, label string, a, b Model, evs []Event) {
	t.Helper()
	if a.Active() != b.Active() || a.Judged() != b.Judged() {
		t.Fatalf("%s: active/judged %v/%d, Build gives %v/%d", label, a.Active(), a.Judged(), b.Active(), b.Judged())
	}
	keys := []key{{"none", "none"}}
	for _, e := range evs {
		keys = append(keys, key{e.EntryPath, e.Heading})
	}
	for _, k := range keys {
		if a.Mult(k.path, k.heading) != b.Mult(k.path, k.heading) ||
			a.Useful(k.path, k.heading) != b.Useful(k.path, k.heading) ||
			a.Opened(k.path) != b.Opened(k.path) {
			t.Fatalf("%s: key %v: mult %v useful %v opened %v; Build gives %v %v %v", label, k,
				a.Mult(k.path, k.heading), a.Useful(k.path, k.heading), a.Opened(k.path),
				b.Mult(k.path, k.heading), b.Useful(k.path, k.heading), b.Opened(k.path))
		}
	}
}

// without is evs minus the events of the searches in ids: what Index.Model's exclude leaves out.
func without(evs []Event, ids []int64) []Event {
	var out []Event
	for _, e := range evs {
		if !slices.Contains(ids, e.SearchID) {
			out = append(out, e)
		}
	}
	return out
}

// The slicing property (PLAN 6.4, Q133): for random logs, instants and excluded searches, the
// model an Index gives is the model Build gives over the events kept fresh, bit for bit. asOf
// lands on event times too (an event at asOf is out), and exclude holds duplicates and ids no
// event has.
func TestIndexModelEqualsBuild(t *testing.T) {
	rng := rand.New(rand.NewSource(64))
	for round := 0; round < 400; round++ {
		data := make([]byte, 7*rng.Intn(40))
		rng.Read(data)
		evs := decode(data)
		ix := NewIndex(evs)
		p := Params{Enabled: rng.Intn(5) > 0, MinJudged: rng.Intn(5), HalfLifeDays: 1 + rng.Float64()*60, MaxBoost: rng.Float64()}
		at := ago(rng.Float64() * 1000)
		if len(evs) > 0 && rng.Intn(3) == 0 {
			at = evs[rng.Intn(len(evs))].At
		}
		var exclude []int64
		for range rng.Intn(4) {
			exclude = append(exclude, int64(rng.Intn(10))) // ids 8, 9 have no event
		}
		got, err := ix.Model(p, at, exclude)
		if err != nil {
			t.Fatal(err)
		}
		want, err := Build(without(evs, exclude), p, at)
		if err != nil {
			t.Fatal(err)
		}
		sameModel(t, fmt.Sprintf("round %d (asOf %v, exclude %v)", round, at, exclude), got, want, evs)
	}
}

// The boundary rows of the slice: an event exactly at asOf is out, a nanosecond earlier is in;
// excluding a search removes it from the count and its evidence, once even when listed twice;
// excluding a search with only later events changes nothing.
func TestIndexModelBoundaries(t *testing.T) {
	at := asOf
	evs := []Event{
		ev(1, Useful, 1, at.Add(-time.Nanosecond)),
		ev(2, Useful, 1, at),
		ev(3, NotUseful, 1, ago(1)),
		ev(4, Open, 0, at.Add(time.Hour)),
	}
	ix := NewIndex(evs)
	p := params(0)
	for _, tc := range []struct {
		name    string
		exclude []int64
		judged  int
	}{
		{"none excluded: 1 and 3 count, 2 at asOf and 4 after do not", nil, 2},
		{"exclude 1", []int64{1}, 1},
		{"exclude 1 twice", []int64{1, 1}, 1},
		{"exclude 2 (at asOf) and 4 (later)", []int64{2, 4}, 2},
		{"exclude unknown 99", []int64{99}, 2},
		{"exclude all", []int64{1, 2, 3, 4}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ix.Model(p, at, tc.exclude)
			if err != nil {
				t.Fatal(err)
			}
			if got.Judged() != tc.judged {
				t.Errorf("Judged = %d, want %d", got.Judged(), tc.judged)
			}
			want, _ := Build(without(evs, tc.exclude), p, at)
			sameModel(t, tc.name, got, want, evs)
		})
	}
}

// Index.Model validates like Build: same error, nothing computed. The zero-evidence model of an
// empty index is neutral.
func TestIndexModelParams(t *testing.T) {
	ix := NewIndex(nil)
	if _, err := ix.Model(Params{HalfLifeDays: 0, MaxBoost: 0.2}, asOf, nil); !errors.Is(err, ErrBadParams) {
		t.Errorf("bad params: err = %v, want ErrBadParams", err)
	}
	m, err := ix.Model(params(0), asOf, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Active() || m.Judged() != 0 || m.Mult("a.md", "H") != 1 {
		t.Errorf("empty index: active %v judged %d mult %v, want true 0 1", m.Active(), m.Judged(), m.Mult("a.md", "H"))
	}
	// Other stores' events never count, as in Build.
	ix = NewIndex([]Event{{SearchID: 1, EntryPath: "a.md", Heading: "H", Kind: Useful, At: ago(1), Store: "s"}})
	if m, _ := ix.Model(params(0), asOf, nil); m.Judged() != 0 || m.Mult("a.md", "H") != 1 {
		t.Errorf("other store: judged %d mult %v, want 0 1", m.Judged(), m.Mult("a.md", "H"))
	}
}

// benchEvents is a judged log of n searches: three verdicts each over 5000 entries, an open every
// tenth search, one minute apart.
func benchEvents(n int, end time.Time) []Event {
	var events []Event
	for i := range n {
		at := end.Add(-time.Duration(n-i) * time.Minute)
		for k := 1; k <= 3; k++ {
			kind := Useful
			if k%2 == 0 {
				kind = NotUseful
			}
			events = append(events, Event{SearchID: int64(i), EntryPath: fmt.Sprintf("e%d.md", (i*7+k)%5000), Kind: kind, Rank: k, At: at})
		}
		if i%10 == 0 {
			events = append(events, Event{SearchID: int64(i), EntryPath: fmt.Sprintf("e%d.md", i%5000), Kind: Open, At: at})
		}
	}
	return events
}

// BenchmarkReplayModels is the prior's share of an eval replay of a 2k-search judged log: a model
// as of every search's time, asked for 40 candidates' multipliers. "build" builds each model
// from the whole log (the pre-6.4 cost per replayed query); "index" prepares the log once
// and slices it per query. Leverage entry search-latency-and-scale cites the numbers.
func BenchmarkReplayModels(b *testing.B) {
	const n = 2000
	end := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	events := benchEvents(n, end)
	p := Params{Enabled: true, MinJudged: 50, HalfLifeDays: 90, MaxBoost: 0.2}
	asks := func(m Model, i int) {
		for c := range 40 {
			m.Mult(fmt.Sprintf("e%d.md", (i+c*13)%5000), "")
		}
	}
	at := func(i int) time.Time { return end.Add(-time.Duration(n-i) * time.Minute) }
	b.Run("build", func(b *testing.B) {
		for b.Loop() {
			for i := range n {
				m, err := Build(events, p, at(i))
				if err != nil {
					b.Fatal(err)
				}
				asks(m, i)
			}
		}
	})
	b.Run("index", func(b *testing.B) {
		for b.Loop() {
			ix := NewIndex(events)
			for i := range n {
				m, err := ix.Model(p, at(i), []int64{int64(i)})
				if err != nil {
					b.Fatal(err)
				}
				asks(m, i)
			}
		}
	})
}

// BenchmarkLivePrior is the prior's share of one kb search with [prior] enabled at 10k judged
// searches: "build" the pre-6.4 Build over the whole log, "index" the NewIndex + Model + 40
// multipliers Search now does per search.
func BenchmarkLivePrior(b *testing.B) {
	end := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	events := benchEvents(10000, end)
	p := Params{Enabled: true, MinJudged: 50, HalfLifeDays: 90, MaxBoost: 0.2}
	asks := func(m Model) {
		for c := range 40 {
			m.Mult(fmt.Sprintf("e%d.md", c*13), "")
		}
	}
	b.Run("build", func(b *testing.B) {
		for b.Loop() {
			m, _ := Build(events, p, end)
			asks(m)
		}
	})
	b.Run("index", func(b *testing.B) {
		for b.Loop() {
			m, _ := NewIndex(events).Model(p, end, nil)
			asks(m)
		}
	})
}
