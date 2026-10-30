package search

import (
	"bytes"
	"context"
	"math"
	"testing"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/store"
)

// settingsWith is the Settings of the default config with [search] changed by edit.
func settingsWith(t testing.TB, edit func(*config.Search)) Settings {
	t.Helper()
	c := config.Default()
	edit(&c.Search)
	if err := c.Validate("test"); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}
	st, err := NewSettings(c)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// TestDefaultParamsMatchConstants: the default config is the package's documented constants, so
// a zero Searcher and one built from config.Default rank identically.
func TestDefaultParamsMatchConstants(t *testing.T) {
	want := Params{
		ListLimit: ListLimit, CandidateLimit: CandidateLimit, PerEntryCap: PerEntryCap, RRFK: RRFConstant,
		VecOverFetch: vecOverFetch, BM25: store.DefaultBM25(), FTSWeight: 1, VecWeight: 1,
	}
	if got := DefaultParams(); got != want {
		t.Errorf("DefaultParams = %+v, want %+v", got, want)
	}
	if config.Default().Search.K != DefaultK {
		t.Errorf("config search.k = %d, want DefaultK %d", config.Default().Search.K, DefaultK)
	}
}

// TestParamsFromMapsEveryKey: each wired [search] key lands in its own Params field.
func TestParamsFromMapsEveryKey(t *testing.T) {
	s := config.Search{
		ListLimit: 11, CandidateLimit: 12, PerEntryCap: 13, RRFK: 14, VecOverfetch: 15,
		BM25:        config.BM25{Text: 1.5, Heading: 2.5, Title: 3.5},
		ListWeights: config.ListWeights{FTS: 0.25, Vec: 0.75, Exact: 9, Memory: 9},
	}
	want := Params{ListLimit: 11, CandidateLimit: 12, PerEntryCap: 13, RRFK: 14, VecOverFetch: 15,
		BM25: store.BM25Weights{Text: 1.5, Heading: 2.5, Title: 3.5}, FTSWeight: 0.25, VecWeight: 0.75}
	if got := ParamsFrom(s); got != want {
		t.Errorf("ParamsFrom = %+v, want %+v", got, want)
	}
}

// TestNewSettingsHash: the trace carries config.EffectiveJSON and its hash; a changed key changes
// both the params and the hash, and the default searcher reports the default hash.
func TestNewSettingsHash(t *testing.T) {
	def, err := DefaultSettings()
	if err != nil {
		t.Fatal(err)
	}
	data, hash, err := config.Default().EffectiveJSON()
	if err != nil {
		t.Fatal(err)
	}
	if def.Hash != hash || !bytes.Equal(def.JSON, data) || def.Params != DefaultParams() {
		t.Errorf("DefaultSettings = %+v, want hash %s and DefaultParams", def, hash)
	}
	changed := settingsWith(t, func(s *config.Search) { s.RRFK = 20 })
	if changed.Hash == def.Hash || changed.Params.RRFK != 20 {
		t.Errorf("rrf_k=20: hash %s (default %s), params %+v", changed.Hash, def.Hash, changed.Params)
	}
}

// TestSearchTrace: Outcome carries the settings it ran with, the FTS expression, the OR flag and
// which candidates the diversity cap dropped.
func TestSearchTrace(t *testing.T) {
	ctx := context.Background()
	s, _ := testSearcher(t)
	def, _ := DefaultSettings()

	t.Run("zero Settings reports the defaults", func(t *testing.T) {
		out, err := s.Search(ctx, Request{Query: "zebracorn", Mode: ModeFTS, K: MaxK})
		if err != nil {
			t.Fatal(err)
		}
		if out.Settings.Hash != def.Hash || out.Settings.Params != DefaultParams() {
			t.Errorf("Settings = %+v, want the defaults", out.Settings)
		}
		if out.FTSMatch != `"zebracorn"` || out.FTSFallbackToOr {
			t.Errorf("FTSMatch = %q, or = %v; want the AND expression", out.FTSMatch, out.FTSFallbackToOr)
		}
		// alpha.md has five zebracorn chunks: the cap keeps three and drops two.
		var dropped, returned int
		for _, c := range out.Candidates {
			if c.CapDropped {
				dropped++
				if c.EntryPath != "alpha.md" {
					t.Errorf("dropped %s, want only alpha.md chunks", c.EntryPath)
				}
			}
		}
		returned = len(out.Returned)
		if dropped != 2 || returned != PerEntryCap {
			t.Errorf("dropped %d, returned %d; want 2 and %d", dropped, returned, PerEntryCap)
		}
	})

	t.Run("explicit Settings are used and echoed", func(t *testing.T) {
		st := settingsWith(t, func(c *config.Search) { c.PerEntryCap = 1 })
		s2 := *s
		s2.Settings = st
		out, err := s2.Search(ctx, Request{Query: "zebracorn", Mode: ModeFTS, K: MaxK})
		if err != nil {
			t.Fatal(err)
		}
		if out.Settings.Hash != st.Hash || len(out.Returned) != 1 {
			t.Errorf("hash %s returned %d; want %s and 1", out.Settings.Hash, len(out.Returned), st.Hash)
		}
	})

	t.Run("vec mode runs no FTS expression", func(t *testing.T) {
		out, err := s.Search(ctx, Request{Query: "zebracorn", Mode: ModeVec, K: MaxK})
		if err != nil {
			t.Fatal(err)
		}
		if out.FTSMatch != "" || out.FTSFallbackToOr {
			t.Errorf("FTSMatch = %q, or = %v; want none", out.FTSMatch, out.FTSFallbackToOr)
		}
	})
}

// TestParamsValid: what Search asserts, field by field, at the edge of each range: an int knob
// at 0 and 1, a weight at 0, below 0, NaN and +Inf.
func TestParamsValid(t *testing.T) {
	ints := map[string]func(*Params) *int{
		"ListLimit":      func(p *Params) *int { return &p.ListLimit },
		"CandidateLimit": func(p *Params) *int { return &p.CandidateLimit },
		"PerEntryCap":    func(p *Params) *int { return &p.PerEntryCap },
		"RRFK":           func(p *Params) *int { return &p.RRFK },
		"VecOverFetch":   func(p *Params) *int { return &p.VecOverFetch },
	}
	floats := map[string]func(*Params) *float64{
		"BM25.Text":    func(p *Params) *float64 { return &p.BM25.Text },
		"BM25.Heading": func(p *Params) *float64 { return &p.BM25.Heading },
		"BM25.Title":   func(p *Params) *float64 { return &p.BM25.Title },
		"FTSWeight":    func(p *Params) *float64 { return &p.FTSWeight },
		"VecWeight":    func(p *Params) *float64 { return &p.VecWeight },
	}
	if !DefaultParams().valid() {
		t.Fatal("DefaultParams is not valid")
	}
	for name, field := range ints {
		for v, want := range map[int]bool{0: false, 1: true} {
			p := DefaultParams()
			*field(&p) = v
			if p.valid() != want {
				t.Errorf("%s = %d: valid = %v, want %v", name, v, !want, want)
			}
		}
	}
	for name, field := range floats {
		for _, tc := range []struct {
			v    float64
			want bool
		}{{0, true}, {math.Nextafter(0, -1), false}, {math.NaN(), false}, {math.Inf(1), false}} {
			p := DefaultParams()
			*field(&p) = tc.v
			if p.valid() != tc.want {
				t.Errorf("%s = %v: valid = %v, want %v", name, tc.v, !tc.want, tc.want)
			}
		}
	}
}
