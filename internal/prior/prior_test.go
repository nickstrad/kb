package prior

import (
	"errors"
	"math"
	"math/rand"
	"testing"
	"time"
)

var asOf = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

var fresh = asOf.Add(-time.Nanosecond) // age ~0: weight 1 to 1e-15

func ago(days float64) time.Time {
	return asOf.Add(-time.Duration(days * 24 * float64(time.Hour)))
}

func params(minJudged int) Params {
	return Params{Enabled: true, MinJudged: minJudged, HalfLifeDays: 90, MaxBoost: 0.2}
}

func ev(id int64, k Kind, rank int, at time.Time) Event {
	return Event{SearchID: id, EntryPath: "a.md", Heading: "H", Kind: k, Rank: rank, At: at}
}

func mustBuild(t *testing.T, events []Event, p Params) Model {
	t.Helper()
	m, err := Build(events, p, asOf)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return m
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// Hand-computed: p = (u+2)/(u+n+4), mult = 1 + 0.2(2p-1).
func TestMultTable(t *testing.T) {
	cases := []struct {
		name   string
		events []Event
		want   float64
	}{
		{"no events", nil, 1},
		{"one useful rank1: u=1 p=3/5", []Event{ev(1, Useful, 1, fresh)}, 1 + 0.2*(0.2)},
		{"one not-useful: n=1 p=2/5", []Event{ev(1, NotUseful, 1, fresh)}, 1 - 0.2*0.2},
		{"not-useful ignores rank weight", []Event{ev(1, NotUseful, 50, fresh)}, 1 - 0.2*0.2},
		{"useful rank4 posWeight 2: p=2/3", []Event{ev(1, Useful, 4, fresh)}, 1 + 0.2/3},
		{"useful rank100 posWeight capped 3: p=5/7", []Event{ev(1, Useful, 100, fresh)}, 1 + 0.2*3/7},
		{"rank 0 counts as rank 1", []Event{ev(1, Useful, 0, fresh)}, 1 + 0.2*0.2},
		{"one half-life: u=.5 p=5/9", []Event{ev(1, Useful, 1, ago(90))}, 1 + 0.2/9},
		{"two half-lives: u=.25", []Event{ev(1, Useful, 1, ago(180))}, 1 + 0.2*(2*(2.25/4.25)-1)},
		{"useful+not-useful balance", []Event{ev(1, Useful, 1, fresh), ev(2, NotUseful, 1, fresh)}, 1},
		{"open counts half a useful: u=.5", []Event{ev(1, Open, 1, fresh)}, 1 + 0.2*(2*(2.5/4.5)-1)},
		{"two useful rank1: u=2 p=2/3", []Event{ev(1, Useful, 1, fresh), {SearchID: 2, EntryPath: "a.md", Heading: "H", Kind: Useful, Rank: 1, At: fresh}}, 1 + 0.2*(2*(4.0/6.0)-1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := mustBuild(t, tc.events, params(0))
			if got := m.Mult("a.md", "H"); !near(got, tc.want) {
				t.Fatalf("Mult = %.12f, want %.12f", got, tc.want)
			}
		})
	}
}

func TestOpenAppliesToEveryHeadingOfPath(t *testing.T) {
	m := mustBuild(t, []Event{ev(1, Open, 1, fresh)}, params(0))
	want := 1 + 0.2*(2*(2.5/4.5)-1)
	for _, h := range []string{"H", "Other", ""} {
		if got := m.Mult("a.md", h); !near(got, want) {
			t.Fatalf("heading %q: %v want %v", h, got, want)
		}
	}
	if got := m.Mult("b.md", "H"); got != 1 {
		t.Fatalf("other path = %v, want 1", got)
	}
	if got := m.Useful("a.md", "Other"); !near(got, 0.5) {
		t.Fatalf("Useful = %v, want 0.5", got)
	}
}

func TestCapBoundary(t *testing.T) {
	huge := func(k Kind) []Event {
		return []Event{{SearchID: 1, EntryPath: "a.md", Heading: "H", Kind: k, Rank: 1, At: fresh}}
	}
	// Below the cap: a single verdict.
	if got := mustBuild(t, huge(Useful), params(0)).Mult("a.md", "H"); !(got > 1 && got < 1.2) {
		t.Fatalf("below cap: %v", got)
	}
	// Near the cap: with many useful verdicts the result approaches, and never exceeds,
	// 1+max_boost (the clamp itself is table-tested below).
	var many []Event
	for i := 0; i < 200; i++ {
		many = append(many, Event{SearchID: int64(i), EntryPath: "a.md", Heading: "H", Kind: Useful, Rank: 100, At: fresh})
	}
	p := params(0)
	p.MaxBoost = 0.999
	got := mustBuild(t, many, p).Mult("a.md", "H")
	if got > 1+p.MaxBoost || got < 1.9 {
		t.Fatalf("near cap: %v", got)
	}
	// clamp itself: below, at and above the bounds.
	for _, tc := range []struct{ v, want float64 }{{0.7, 0.8}, {0.8, 0.8}, {1, 1}, {1.2, 1.2}, {1.3, 1.2}} {
		if g := clamp(tc.v, 0.8, 1.2); g != tc.want {
			t.Fatalf("clamp(%v) = %v, want %v", tc.v, g, tc.want)
		}
	}
	// MaxBoost 0 pins the multiplier at 1.
	p0 := params(0)
	p0.MaxBoost = 0
	if g := mustBuild(t, huge(Useful), p0).Mult("a.md", "H"); g != 1 {
		t.Fatalf("max_boost 0: %v", g)
	}
}

func TestGateMinJudged(t *testing.T) {
	// Distinct searches 1..k on other paths; only search 1 judges a.md.
	build := func(k int, enabled bool) Model {
		evs := []Event{ev(1, Useful, 1, ago(1))}
		for i := 2; i <= k; i++ {
			evs = append(evs, Event{SearchID: int64(i), EntryPath: "z.md", Kind: NotUseful, Rank: 1, At: ago(1)})
		}
		p := params(5)
		p.Enabled = enabled
		return mustBuild(t, evs, p)
	}
	for _, tc := range []struct {
		k       int
		enabled bool
		active  bool
	}{{4, true, false}, {5, true, true}, {6, true, true}, {5, false, false}, {0, true, false}} {
		m := build(tc.k, tc.enabled)
		if m.Active() != tc.active {
			t.Fatalf("k=%d enabled=%v: Active=%v want %v", tc.k, tc.enabled, m.Active(), tc.active)
		}
		if tc.k > 0 && m.Judged() != tc.k {
			t.Fatalf("Judged = %d, want %d", m.Judged(), tc.k)
		}
		got := m.Mult("a.md", "H")
		if tc.active && !(got > 1) {
			t.Fatalf("k=%d active but Mult=%v", tc.k, got)
		}
		if !tc.active && (got != 1 || m.Useful("a.md", "H") != 0) {
			t.Fatalf("k=%d gate off but Mult=%v", tc.k, got)
		}
	}
	// Many events of one search count once.
	evs := []Event{ev(7, Useful, 1, ago(1)), ev(7, Useful, 2, ago(1)), ev(7, NotUseful, 3, ago(1))}
	if m := mustBuild(t, evs, params(2)); m.Active() || m.Judged() != 1 {
		t.Fatalf("one search counted as %d, active=%v", m.Judged(), m.Active())
	}
}

func TestOtherStoreAndFutureIgnored(t *testing.T) {
	other := ev(1, Useful, 1, ago(1))
	other.Store = "other"
	future := ev(2, Useful, 1, asOf.Add(time.Hour))
	atAsOf := ev(3, Useful, 1, asOf)
	m := mustBuild(t, []Event{other, future, atAsOf}, params(0))
	if m.Judged() != 0 || m.Mult("a.md", "H") != 1 {
		t.Fatalf("ignored events leaked: judged=%d mult=%v", m.Judged(), m.Mult("a.md", "H"))
	}
	// An event just before asOf counts.
	m = mustBuild(t, []Event{ev(4, Useful, 1, asOf.Add(-time.Nanosecond))}, params(0))
	if m.Judged() != 1 || !(m.Mult("a.md", "H") > 1) {
		t.Fatal("event just before asOf was dropped")
	}
}

func TestKeysAreIndependent(t *testing.T) {
	m := mustBuild(t, []Event{ev(1, Useful, 1, ago(1))}, params(0))
	if m.Mult("a.md", "Other") != 1 || m.Mult("b.md", "H") != 1 {
		t.Fatal("evidence leaked to another key")
	}
}

func TestZeroModelAndValidate(t *testing.T) {
	var z Model
	if z.Active() || z.Judged() != 0 || z.Mult("a", "b") != 1 || z.Useful("a", "b") != 0 {
		t.Fatal("zero Model is not neutral")
	}
	if err := DefaultParams().Validate(); err != nil {
		t.Fatal(err)
	}
	if d := DefaultParams(); d.Enabled || d.MinJudged != 50 || d.HalfLifeDays != 90 || d.MaxBoost != 0.2 || d.QueryMemory {
		t.Fatalf("defaults %+v", d)
	}
	nan := math.NaN()
	for name, p := range map[string]Params{
		"min_judged<0":  {MinJudged: -1, HalfLifeDays: 1, MaxBoost: 0.1},
		"half-life 0":   {HalfLifeDays: 0, MaxBoost: 0.1},
		"half-life NaN": {HalfLifeDays: nan, MaxBoost: 0.1},
		"half-life Inf": {HalfLifeDays: math.Inf(1), MaxBoost: 0.1},
		"boost<0":       {HalfLifeDays: 1, MaxBoost: -0.1},
		"boost>1":       {HalfLifeDays: 1, MaxBoost: 1.0000001},
		"boost NaN":     {HalfLifeDays: 1, MaxBoost: nan},
	} {
		if _, err := Build(nil, p, asOf); !errors.Is(err, ErrBadParams) {
			t.Errorf("%s: err = %v, want ErrBadParams", name, err)
		}
	}
}

// --- properties ---

// decode turns fuzz bytes into events (7 bytes each) on two paths and two headings.
func decode(data []byte) []Event {
	var evs []Event
	for len(data) >= 7 {
		b := data[:7]
		data = data[7:]
		evs = append(evs, Event{
			SearchID:  int64(b[0] % 8),
			EntryPath: string(rune('a' + b[1]%2)),
			Heading:   string(rune('x' + b[2]%2)),
			Kind:      Kind(b[3] % 3),
			Rank:      int(b[4]) - 5, // includes rank <= 0
			At:        asOf.Add(-time.Duration(int(b[5])*37+1) * time.Hour * 3),
			Store:     [...]string{"", "", "", "s"}[b[6]%4],
		})
	}
	return evs
}

func checkProps(t *testing.T, evs []Event, minJudged int, boost float64) {
	t.Helper()
	p := Params{Enabled: true, MinJudged: minJudged, HalfLifeDays: 30, MaxBoost: boost}
	m, err := Build(evs, p, asOf)
	if err != nil {
		t.Fatal(err)
	}
	// Order independence: reversed and shuffled lists give bit-identical multipliers.
	rev := append([]Event(nil), evs...)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	rng := rand.New(rand.NewSource(int64(len(evs))))
	shuf := append([]Event(nil), evs...)
	rng.Shuffle(len(shuf), func(i, j int) { shuf[i], shuf[j] = shuf[j], shuf[i] })
	mr, _ := Build(rev, p, asOf)
	ms, _ := Build(shuf, p, asOf)
	for _, path := range []string{"a", "b"} {
		for _, h := range []string{"x", "y"} {
			got := m.Mult(path, h)
			if got < 1-boost || got > 1+boost || math.IsNaN(got) {
				t.Fatalf("Mult %v outside [1-%v,1+%v]", got, boost, boost)
			}
			if !m.Active() && got != 1 {
				t.Fatalf("gate off but Mult = %v", got)
			}
			if mr.Mult(path, h) != got || ms.Mult(path, h) != got {
				t.Fatalf("order dependent: %v %v %v", got, mr.Mult(path, h), ms.Mult(path, h))
			}
			// Monotone: one more useful verdict on the key never lowers it (judged count only
			// grows, so keep the gate open by reusing an existing search id).
			if m.Active() {
				more := append(append([]Event(nil), evs...), Event{SearchID: evs[0].SearchID, EntryPath: path, Heading: h, Kind: Useful, Rank: 1, At: ago(1)})
				mm, _ := Build(more, p, asOf)
				if mm.Mult(path, h) < got {
					t.Fatalf("extra useful lowered Mult: %v -> %v", got, mm.Mult(path, h))
				}
				// And one more not-useful never raises it.
				less := append(append([]Event(nil), evs...), Event{SearchID: evs[0].SearchID, EntryPath: path, Heading: h, Kind: NotUseful, Rank: 1, At: ago(1)})
				ml, _ := Build(less, p, asOf)
				if ml.Mult(path, h) > got {
					t.Fatalf("extra not-useful raised Mult: %v -> %v", got, ml.Mult(path, h))
				}
			}
		}
	}
}

func FuzzPrior(f *testing.F) {
	f.Add([]byte{}, uint8(0))
	f.Add([]byte{1, 0, 0, 0, 6, 3, 0, 2, 0, 0, 1, 9, 200, 0, 3, 1, 1, 0, 255, 1, 3}, uint8(1))
	f.Add([]byte{1, 0, 0, 0, 6, 3, 0, 2, 1, 1, 0, 6, 9, 0}, uint8(3))
	f.Fuzz(func(t *testing.T, data []byte, minJ uint8) {
		evs := decode(data)
		if len(evs) == 0 {
			evs = []Event{{SearchID: 1, EntryPath: "a", Heading: "x", Kind: Useful, Rank: 1, At: ago(1)}}
		}
		checkProps(t, evs, int(minJ%4), 0.2)
	})
}

func TestPropertiesRandomized(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 300; i++ {
		data := make([]byte, 7*(1+rng.Intn(20)))
		rng.Read(data)
		evs := decode(data)
		checkProps(t, evs, rng.Intn(4), 0.2)
	}
}

// Older evidence never outweighs identical newer evidence.
func TestOlderNeverOutweighsNewer(t *testing.T) {
	for _, kind := range []Kind{Useful, Open} {
		for _, rank := range []int{1, 2, 9, 40} {
			for _, d1 := range []float64{0.5, 10, 90, 400} {
				for _, d2 := range []float64{0.6, 11, 91, 900} {
					if d2 <= d1 {
						continue
					}
					newer := mustBuild(t, []Event{ev(1, kind, rank, ago(d1))}, params(0)).Mult("a.md", "H")
					older := mustBuild(t, []Event{ev(1, kind, rank, ago(d2))}, params(0)).Mult("a.md", "H")
					if older > newer {
						t.Fatalf("kind %v rank %d: older(%v d) %v > newer(%v d) %v", kind, rank, d2, older, d1, newer)
					}
				}
			}
		}
	}
	// A not-useful verdict is the mirror: older negative pulls down less.
	newer := mustBuild(t, []Event{ev(1, NotUseful, 1, ago(1))}, params(0)).Mult("a.md", "H")
	older := mustBuild(t, []Event{ev(1, NotUseful, 1, ago(200))}, params(0)).Mult("a.md", "H")
	if older < newer {
		t.Fatalf("older negative %v pulls harder than newer %v", older, newer)
	}
}

// A deeper-ranked useful hit is stronger evidence (up to the cap); unjudged is nothing.
func TestPositionWeightMonotoneUpToCap(t *testing.T) {
	prev := 0.0
	for _, r := range []int{1, 2, 4, 9, 10, 100} {
		got := mustBuild(t, []Event{ev(1, Useful, r, ago(1))}, params(0)).Mult("a.md", "H")
		if got < prev {
			t.Fatalf("rank %d: %v < %v", r, got, prev)
		}
		prev = got
	}
	a := mustBuild(t, []Event{ev(1, Useful, 9, ago(1))}, params(0)).Mult("a.md", "H")
	b := mustBuild(t, []Event{ev(1, Useful, 500, ago(1))}, params(0)).Mult("a.md", "H")
	if a != b {
		t.Fatalf("position weight not capped at rank 9: %v vs %v", a, b)
	}
}

// Validate accepts the edges: half-life below 1 day, min_judged 0, max_boost 0 and 1.
func TestValidateAcceptsEdges(t *testing.T) {
	for _, p := range []Params{
		{HalfLifeDays: 0.5, MaxBoost: 0},
		{HalfLifeDays: 1, MaxBoost: 0.999},
		{HalfLifeDays: 1, MaxBoost: 1}, // config accepts prior.max_boost = 1
		{HalfLifeDays: 1e-9, MinJudged: 0, MaxBoost: 0.5},
	} {
		if err := p.Validate(); err != nil {
			t.Errorf("%+v: %v", p, err)
		}
	}
}

// With thousands of events of varied weight, any shuffle gives bit-identical results: the
// canonical summation order, not luck, makes Build order-independent.
func TestOrderIndependenceBitExact(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	var evs []Event
	for i := 0; i < 3000; i++ {
		evs = append(evs, Event{
			SearchID: int64(i % 60), EntryPath: "a", Heading: "x", Kind: Kind(rng.Intn(3)),
			Rank: 1 + rng.Intn(30), At: ago(rng.Float64() * 400),
		})
	}
	p := Params{Enabled: true, HalfLifeDays: 37, MaxBoost: 0.9}
	base, _ := Build(evs, p, asOf)
	for round := 0; round < 20; round++ {
		rng.Shuffle(len(evs), func(i, j int) { evs[i], evs[j] = evs[j], evs[i] })
		m, _ := Build(evs, p, asOf)
		if m.Useful("a", "x") != base.Useful("a", "x") || m.Mult("a", "x") != base.Mult("a", "x") ||
			m.n[key{"a", "x"}] != base.n[key{"a", "x"}] {
			t.Fatalf("round %d: sums differ (%v vs %v)", round, m.Useful("a", "x"), base.Useful("a", "x"))
		}
	}
}

// With max_boost 1 the multiplier stays strictly inside (0, 2): the smoothing keeps p off 0 and 1,
// so heavy negative evidence demotes a hit but never zeroes its score.
func TestMaxBoostOneNeverZeroes(t *testing.T) {
	var evs []Event
	for i := 0; i < 200; i++ {
		evs = append(evs, ev(int64(i), NotUseful, 1, fresh))
	}
	p := Params{Enabled: true, HalfLifeDays: 90, MaxBoost: 1}
	m := mustBuild(t, evs, p)
	if got := m.Mult("a.md", "H"); !(got > 0 && got < 0.05) {
		t.Errorf("Mult under 200 negatives with max_boost 1 = %v, want in (0, 0.05)", got)
	}
}
