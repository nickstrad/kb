package config

import (
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/nickstrad/kb/internal/errs"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultIsValidAndRoundTrips(t *testing.T) {
	d := Default()
	if err := d.Validate("default"); err != nil {
		t.Fatalf("Default() invalid: %v", err)
	}
	if d.Log.OpenWindow != "30m" {
		t.Errorf("default log.open_window = %q, want 30m (shipped behaviour)", d.Log.OpenWindow)
	}
	out, err := Save(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "rrf_k = 60\n") {
		t.Errorf("Save(Default) lacks `rrf_k = 60`:\n%s", out)
	}
	back, err := Parse(out, "saved")
	if err != nil {
		t.Fatalf("Parse(Save(Default)): %v\n%s", err, out)
	}
	if !reflect.DeepEqual(back, d) {
		t.Errorf("round trip changed the defaults:\n got %+v\nwant %+v", back, d)
	}
}

func TestReadMissingFileIsDefaults(t *testing.T) {
	c, found, err := Read(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil || found || !reflect.DeepEqual(c, Default()) {
		t.Errorf("Read(missing) = (%+v, %v, %v), want defaults, false, nil", c, found, err)
	}
}

// TestReadFoundFlag: found says whether a file existed, also when it was unusable.
func TestReadFoundFlag(t *testing.T) {
	_, found, err := Read(writeFile(t, "bogus = 1\n"))
	if err == nil || !found {
		t.Errorf("bad file: found=%v err=%v, want found and an error", found, err)
	}
	_, found, err = Read(t.TempDir())
	if err == nil || found {
		t.Errorf("unreadable: found=%v err=%v, want not found and an error", found, err)
	}
	// A regular file kb refuses still counts as found; a path it cannot even stat does not.
	big := filepath.Join(t.TempDir(), "big.toml")
	if err := os.WriteFile(big, make([]byte, MaxFileBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, found, err = Read(big); err == nil || !found {
		t.Errorf("over the cap: found=%v err=%v, want found and an error", found, err)
	}
	locked := writeFile(t, "version = 1\n")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o644) })
	if _, found, err = Read(locked); err == nil || !found {
		t.Errorf("mode 000: found=%v err=%v, want found and an error", found, err)
	}
	dir := t.TempDir()
	hidden := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(hidden, []byte("version = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	if _, found, err = Read(hidden); err == nil || found || errs.KindOf(err) != errs.Usage {
		t.Errorf("cannot stat: found=%v err=%v, want not found and a Usage error", found, err)
	}
}

// TestDefaultsGolden pins every default value to testdata/default.toml, the reviewed list.
func TestDefaultsGolden(t *testing.T) {
	want, err := os.ReadFile("testdata/default.toml")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Save(Default())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("Save(Default()) differs from testdata/default.toml (a changed default needs a reviewed golden):\n%s", got)
	}
}

// TestFractionBoundMessage: the range text shows whether the upper bound is allowed.
func TestFractionBoundMessage(t *testing.T) {
	c := Default()
	c.Eval.Holdout = 1
	if err := c.Validate("f"); err == nil || !strings.Contains(err.Error(), "[0, 1)") {
		t.Errorf("holdout 1: %v", err)
	}
	c = Default()
	c.Prior.MaxBoost = 2
	if err := c.Validate("f"); err == nil || !strings.Contains(err.Error(), "[0, 1]") {
		t.Errorf("max_boost 2: %v", err)
	}
}

func TestReadUnreadableIsUsage(t *testing.T) {
	// A directory where the file should be: exists, cannot be read.
	_, _, err := Read(t.TempDir())
	if errs.KindOf(err) != errs.Usage {
		t.Errorf("Read(dir) kind = %v (%v), want Usage", errs.KindOf(err), err)
	}
}

func TestPartialFileKeepsDefaults(t *testing.T) {
	p := writeFile(t, "[search]\nk = 5\n[search.bm25]\ntitle = 4\n")
	c, found, err := Read(p)
	if err != nil || !found {
		t.Fatalf("Read: %v found=%v", err, found)
	}
	want := Default()
	want.Search.K = 5
	want.Search.BM25.Title = 4
	if !reflect.DeepEqual(c, want) {
		t.Errorf("got %+v\nwant %+v", c, want)
	}
}

// TestParseErrors: every rejected file names the file, and where it can the key or line.
// TestReadSizeCapBoundary: a valid config padded with a TOML comment to each size around
// MaxFileBytes. At the cap and one under it the file is read; one over, it is refused unread.
func TestReadSizeCapBoundary(t *testing.T) {
	base, err := Save(Default())
	if err != nil {
		t.Fatal(err)
	}
	padded := func(t *testing.T, size int) string {
		t.Helper()
		pad := size - len(base)
		if pad < 2 {
			t.Fatalf("size %d is below the base config (%d bytes)", size, len(base))
		}
		// "#" + (pad-2) filler bytes + "\n" is pad bytes: a comment line that makes the total size.
		return string(base) + "#" + strings.Repeat("x", pad-2) + "\n"
	}
	cases := []struct {
		name    string
		content func(t *testing.T) string
		wantErr bool
	}{
		{"zero bytes is defaults", func(t *testing.T) string { return "" }, false},
		{"one MiB minus one is accepted", func(t *testing.T) string { return padded(t, MaxFileBytes-1) }, false},
		{"exactly one MiB is accepted", func(t *testing.T) string { return padded(t, MaxFileBytes) }, false},
		{"one MiB plus one is refused", func(t *testing.T) string { return padded(t, MaxFileBytes+1) }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeFile(t, tc.content(t))
			c, found, err := Read(p)
			if tc.wantErr {
				if errs.KindOf(err) != errs.Usage {
					t.Fatalf("Read kind = %v (%v), want Usage", errs.KindOf(err), err)
				}
				if !strings.Contains(err.Error(), p) || !strings.Contains(err.Error(), "1048576") {
					t.Errorf("error %q must name the file and the 1 MiB limit", err)
				}
				return
			}
			if err != nil || !found {
				t.Fatalf("Read: found=%v err=%v, want found and no error", found, err)
			}
			// Comments and empty input change nothing: every accepted file reads as the defaults.
			if !reflect.DeepEqual(c, Default()) {
				t.Errorf("got %+v, want defaults", c)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name, body string
		want       []string
	}{
		{"unknown top-level key", "bogus = 1\n", []string{`unknown key "bogus"`}},
		{"unknown nested key", "[search]\nrrf = 3\n", []string{`unknown key "search.rrf"`}},
		{"unknown key in bm25", "[search.bm25]\nbody = 3\n", []string{`"search.bm25.body"`}},
		{"several unknown keys", "a = 1\nb = 2\n", []string{"unknown keys", `"a", "b"`}},
		{"string for int", "[search]\nk = \"eight\"\n", []string{"config.toml:2: search.k: incompatible types"}},
		{"int for string", "[embed]\nmodel = 3\n", []string{"config.toml:2: embed.model:"}},
		{"float for int", "[search]\nk = 2.5\n", []string{"config.toml:2"}},
		{"int for bool", "[log]\ncorpus_rev = 1\n", []string{"config.toml:2"}},
		{"duplicate key names the key", "[search]\nk = 1\nk = 2\n", []string{"config.toml:3: search.k: "}},
		{"syntax error", "[search\nk = 1\n", []string{"config.toml:2"}},
		{"newer version", "version = 2\n", []string{"version", "config schema v2 was written by a newer kb (this kb reads v1)"}},
		{"newer version with unknown keys", "version = 2\n[future]\nx = 1\n", []string{"v2 was written by a newer kb"}},
		{"version zero", "version = 0\n", []string{"version", "unsupported config version 0"}},
		{"version negative", "version = -1\n", []string{"version", "unsupported config version -1"}},
		{"version string", "version = \"1\"\n", []string{"config.toml:1", "version"}},
		{"duration not parsing", "[log]\nopen_window = \"soon\"\n", []string{"log.open_window", "not a duration"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeFile(t, tt.body))
			if err == nil {
				t.Fatal("Load accepted it")
			}
			if errs.KindOf(err) != errs.Usage {
				t.Errorf("kind = %v, want Usage", errs.KindOf(err))
			}
			msg := err.Error()
			if !strings.Contains(msg, "config.toml") {
				t.Errorf("message %q does not name the file", msg)
			}
			for _, w := range tt.want {
				if !strings.Contains(msg, w) {
					t.Errorf("message %q lacks %q", msg, w)
				}
			}
			if strings.Contains(msg, "\n") {
				t.Errorf("message is not one line: %q", msg)
			}
		})
	}
}

// TestVersionCheck: the version is judged before the unknown-key check; an older file is a Conflict.
func TestVersionCheck(t *testing.T) {
	if err := checkVersion("f", 3, 3); err != nil {
		t.Errorf("current: %v", err)
	}
	err := checkVersion("f.toml", 4, 3)
	if errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), "f.toml: version: config schema v4 was written by a newer kb (this kb reads v3)") {
		t.Errorf("newer = %v (%v)", err, errs.KindOf(err))
	}
	for _, v := range []int64{1, 2} {
		err = checkVersion("f.toml", v, 3)
		if errs.KindOf(err) != errs.Conflict || !strings.Contains(err.Error(), "older than this kb's v3; run kb migrate up") {
			t.Errorf("older v%d = %v (%v)", v, err, errs.KindOf(err))
		}
	}
	for _, v := range []int64{0, -1} {
		if err := checkVersion("f", v, 3); err != nil {
			t.Errorf("v%d left to Validate, got %v", v, err)
		}
	}
	// Missing version means current.
	if c, err := Parse([]byte("[search]\nk = 4\n"), "f"); err != nil || c.Version != SchemaVersion {
		t.Errorf("missing version: %+v %v", c, err)
	}
	if _, err := Parse([]byte("version = 1\n"), "f"); err != nil {
		t.Errorf("current version: %v", err)
	}
}

// TestValidateBoundaries is the branch-outcome table for Validate: each rule just inside and just
// outside its limit.
func TestValidateBoundaries(t *testing.T) {
	set := func(f func(*Config)) Config {
		c := Default()
		f(&c)
		return c
	}
	tests := []struct {
		name    string
		c       Config
		wantKey string // "" = valid
	}{
		{"k = MinK-1", set(func(c *Config) { c.Search.K = MinK - 1 }), "search.k"},
		{"k = MinK", set(func(c *Config) { c.Search.K = MinK }), ""},
		{"k = MaxK", set(func(c *Config) { c.Search.K = MaxK }), ""},
		{"k = MaxK+1", set(func(c *Config) { c.Search.K = MaxK + 1 }), "search.k"},
		{"list_limit 0", set(func(c *Config) { c.Search.ListLimit = 0 }), "search.list_limit"},
		{"list_limit 1", set(func(c *Config) { c.Search.ListLimit = 1 }), ""},
		{"candidate_limit 0", set(func(c *Config) { c.Search.CandidateLimit = 0 }), "search.candidate_limit"},
		{"candidate_limit = k-1", set(func(c *Config) { c.Search.K = 10; c.Search.CandidateLimit = 9 }), "search.candidate_limit"},
		{"candidate_limit = k", set(func(c *Config) { c.Search.K = 10; c.Search.CandidateLimit = 10 }), ""},
		{"per_entry_cap 0", set(func(c *Config) { c.Search.PerEntryCap = 0 }), "search.per_entry_cap"},
		{"per_entry_cap 1", set(func(c *Config) { c.Search.PerEntryCap = 1 }), ""},
		{"rrf_k 0", set(func(c *Config) { c.Search.RRFK = 0 }), "search.rrf_k"},
		{"rrf_k -1", set(func(c *Config) { c.Search.RRFK = -1 }), "search.rrf_k"},
		{"rrf_k 1", set(func(c *Config) { c.Search.RRFK = 1 }), ""},
		{"list_limit = MaxListLimit", set(func(c *Config) { c.Search.ListLimit = MaxListLimit }), ""},
		{"list_limit = MaxListLimit+1", set(func(c *Config) { c.Search.ListLimit = MaxListLimit + 1 }), "search.list_limit"},
		{"candidate_limit = MaxCandidateLimit", set(func(c *Config) { c.Search.CandidateLimit = MaxCandidateLimit }), ""},
		{"candidate_limit = MaxCandidateLimit+1", set(func(c *Config) { c.Search.CandidateLimit = MaxCandidateLimit + 1 }), "search.candidate_limit"},
		{"per_entry_cap large (no upper bound)", set(func(c *Config) { c.Search.PerEntryCap = 1 << 30 }), ""},
		{"rrf_k = MaxRRFK", set(func(c *Config) { c.Search.RRFK = MaxRRFK }), ""},
		{"rrf_k = MaxRRFK+1", set(func(c *Config) { c.Search.RRFK = MaxRRFK + 1 }), "search.rrf_k"},
		{"vec_overfetch 0", set(func(c *Config) { c.Search.VecOverfetch = 0 }), "search.vec_overfetch"},
		{"vec_overfetch 1", set(func(c *Config) { c.Search.VecOverfetch = 1 }), ""},
		{"vec_overfetch = MaxVecOverfetch", set(func(c *Config) { c.Search.VecOverfetch = MaxVecOverfetch }), ""},
		{"vec_overfetch = MaxVecOverfetch+1", set(func(c *Config) { c.Search.VecOverfetch = MaxVecOverfetch + 1 }), "search.vec_overfetch"},
		{"bm25 heading = MaxWeight", set(func(c *Config) { c.Search.BM25.Heading = MaxWeight }), ""},
		{"bm25 heading above MaxWeight", set(func(c *Config) { c.Search.BM25.Heading = math.Nextafter(MaxWeight, 2*MaxWeight) }), "search.bm25.heading"},
		{"bm25 text +Inf", set(func(c *Config) { c.Search.BM25.Text = math.Inf(1) }), "search.bm25.text"},
		{"list weight fts 0", set(func(c *Config) { c.Search.ListWeights.FTS = 0 }), ""},
		{"list weight fts = MaxWeight", set(func(c *Config) { c.Search.ListWeights.FTS = MaxWeight }), ""},
		{"list weight fts MaxWeight+1", set(func(c *Config) { c.Search.ListWeights.FTS = MaxWeight + 1 }), "search.list_weights.fts"},
		{"list weight memory MaxWeight+1 (reserved, still bounded)", set(func(c *Config) { c.Search.ListWeights.Memory = MaxWeight + 1 }), "search.list_weights.memory"},
		{"bm25 title -0.1", set(func(c *Config) { c.Search.BM25.Title = -0.1 }), "search.bm25.title"},
		{"bm25 title 0", set(func(c *Config) { c.Search.BM25.Title = 0 }), ""},
		{"bm25 text NaN", set(func(c *Config) { c.Search.BM25.Text = nan() }), "search.bm25.text"},
		{"list weight vec -1", set(func(c *Config) { c.Search.ListWeights.Vec = -1 }), "search.list_weights.vec"},
		{"supersede 1", set(func(c *Config) { c.Search.SupersedePenalty = 1 }), ""},
		{"supersede 1.1", set(func(c *Config) { c.Search.SupersedePenalty = 1.1 }), "search.supersede_penalty"},
		{"supersede -0.1", set(func(c *Config) { c.Search.SupersedePenalty = -0.1 }), "search.supersede_penalty"},
		{"empty store name", set(func(c *Config) { c.Search.Stores = []string{"a", " "} }), "search.stores"},
		{"model empty", set(func(c *Config) { c.Embed.Model = " " }), "embed.model"},
		{"model none", set(func(c *Config) { c.Embed.Model = ModelNone }), ""},
		{"dim 0", set(func(c *Config) { c.Embed.Dim = 0 }), ""},
		{"dim 128 (reserved)", set(func(c *Config) { c.Embed.Dim = 128 }), "embed.dim"},
		{"int8 (reserved)", set(func(c *Config) { c.Embed.Int8 = true }), "embed.int8"},
		{"workers -1", set(func(c *Config) { c.Embed.Workers = -1 }), "embed.workers"},
		{"workers 0", set(func(c *Config) { c.Embed.Workers = 0 }), ""},
		{"batch 0", set(func(c *Config) { c.Embed.Batch = 0 }), "embed.batch"},
		{"batch 1", set(func(c *Config) { c.Embed.Batch = 1 }), ""},
		{"output.budget -1", set(func(c *Config) { c.Output.Budget = -1 }), "output.budget"},
		{"output.context_budget 0", set(func(c *Config) { c.Output.ContextBudget = 0 }), "output.context_budget"},
		{"expand none", set(func(c *Config) { c.Output.Expand = "none" }), ""},
		{"expand section", set(func(c *Config) { c.Output.Expand = "section" }), ""},
		{"expand entry", set(func(c *Config) { c.Output.Expand = "entry" }), ""},
		{"expand page", set(func(c *Config) { c.Output.Expand = "page" }), "output.expand"},
		{"expand empty", set(func(c *Config) { c.Output.Expand = "" }), "output.expand"},
		{"open_window 1ns", set(func(c *Config) { c.Log.OpenWindow = "1ns" }), ""},
		{"open_window 0s", set(func(c *Config) { c.Log.OpenWindow = "0s" }), "log.open_window"},
		{"open_window -5m", set(func(c *Config) { c.Log.OpenWindow = "-5m" }), "log.open_window"},
		{"open_window 2h", set(func(c *Config) { c.Log.OpenWindow = "2h" }), ""},
		{"open_window empty", set(func(c *Config) { c.Log.OpenWindow = "" }), "log.open_window"},
		{"stale.days 0", set(func(c *Config) { c.Stale.Days = 0 }), "stale.days"},
		{"eval.min_judged -1", set(func(c *Config) { c.Eval.MinJudged = -1 }), "eval.min_judged"},
		{"holdout 0", set(func(c *Config) { c.Eval.Holdout = 0 }), ""},
		{"holdout 1", set(func(c *Config) { c.Eval.Holdout = 1 }), "eval.holdout"},
		{"holdout -0.1", set(func(c *Config) { c.Eval.Holdout = -0.1 }), "eval.holdout"},
		{"prior.min_judged -1", set(func(c *Config) { c.Prior.MinJudged = -1 }), "prior.min_judged"},
		{"prior.half_life 0", set(func(c *Config) { c.Prior.HalfLifeDays = 0 }), "prior.half_life_days"},
		{"max_boost 1", set(func(c *Config) { c.Prior.MaxBoost = 1 }), ""},
		{"max_boost 1.01", set(func(c *Config) { c.Prior.MaxBoost = 1.01 }), "prior.max_boost"},
		{"caller two lines", set(func(c *Config) { c.Agent.Caller = "a\nb" }), "agent.caller"},
		{"editor two lines", set(func(c *Config) { c.Edit.Editor = "a\rb" }), "edit.editor"},
		{"store ref empty in search.stores", set(func(c *Config) { c.Search.Stores = []string{"global", "name@/p", "alias"} }), ""},
		{"version 0", set(func(c *Config) { c.Version = 0 }), "version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.c.Validate("cfg.toml")
			if tt.wantKey == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate accepted it, want an error about %s", tt.wantKey)
			}
			if errs.KindOf(err) != errs.Usage {
				t.Errorf("kind = %v, want Usage", errs.KindOf(err))
			}
			if want := "cfg.toml: " + tt.wantKey + ":"; !strings.HasPrefix(err.Error(), want) {
				t.Errorf("message %q does not start with %q", err, want)
			}
		})
	}
}

func nan() float64 { z := 0.0; return z / z }

func TestCheckModel(t *testing.T) {
	known := func(id string) error {
		if id == "good/model" {
			return nil
		}
		return fmt.Errorf("unknown embedding model %q (supported: good/model)", id)
	}
	for _, tt := range []struct {
		model string
		ok    bool
	}{{"good/model", true}, {ModelNone, true}, {"bad/model", false}} {
		c := Default()
		c.Embed.Model = tt.model
		err := c.CheckModel("/s/.kb/config.toml", known)
		if (err == nil) != tt.ok {
			t.Errorf("CheckModel(%q) = %v, want ok=%v", tt.model, err, tt.ok)
		}
		if err != nil {
			if errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), "/s/.kb/config.toml: embed.model:") {
				t.Errorf("CheckModel(%q) = %v, want a Usage error naming file and key", tt.model, err)
			}
		}
	}
}

// randomConfig builds a valid Config from r, spanning every field's range.
func randomConfig(r *rand.Rand) Config {
	c := Default()
	c.Embed.Model = []string{DefaultModel, ModelNone, "a/b c", `quo"te`}[r.Intn(4)]
	c.Embed.Workers = r.Intn(5)
	c.Embed.Batch = 1 + r.Intn(64)
	c.Search.K = MinK + r.Intn(MaxK-MinK+1)
	c.Search.ListLimit = 1 + r.Intn(100)
	c.Search.CandidateLimit = c.Search.K + r.Intn(100)
	c.Search.PerEntryCap = 1 + r.Intn(9)
	c.Search.RRFK = 1 + r.Intn(200)
	c.Search.VecOverfetch = 1 + r.Intn(200)
	f := func() float64 { return float64(r.Intn(1000)) / 8 }
	c.Search.BM25 = BM25{f(), f(), f()}
	c.Search.ListWeights = ListWeights{f(), f(), f(), f()}
	c.Search.PhraseBoost, c.Search.ExactList, c.Search.Spell = r.Intn(2) == 0, r.Intn(2) == 0, r.Intn(2) == 0
	c.Search.Synonyms = []string{"", "syn.txt", "é ü\t\"x\""}[r.Intn(3)]
	c.Search.SupersedePenalty = float64(r.Intn(9)) / 8
	for i := r.Intn(3); i > 0; i-- {
		c.Search.Stores = append(c.Search.Stores, fmt.Sprintf("s%d", r.Intn(9)))
	}
	c.Output = Output{Budget: r.Intn(5000), ContextBudget: 1 + r.Intn(5000), Expand: []string{"none", "section", "entry"}[r.Intn(3)]}
	c.Log = Log{CorpusRev: r.Intn(2) == 0, OpenWindow: []string{"30m", "2h", "1h30m", "90s"}[r.Intn(4)]}
	c.Stale.Days = 1 + r.Intn(400)
	c.Eval = Eval{MinJudged: r.Intn(100), Holdout: float64(r.Intn(8)) / 8}
	c.Prior = Prior{Enabled: r.Intn(2) == 0, MinJudged: r.Intn(100), HalfLifeDays: 1 + r.Intn(200),
		MaxBoost: float64(r.Intn(9)) / 8, QueryMemory: r.Intn(2) == 0}
	c.Sources = [][]Source{
		{},
		{{Path: "data", Layout: LayoutEntries, Exclude: []string{}}},
		{{Path: "data-adr", Layout: LayoutEntries, Exclude: []string{}},
			{Glob: "docs/**/*.md", Layout: LayoutFiles, Exclude: []string{"docs/archive/**", "é ü.md"}}},
	}[r.Intn(3)]
	c.Agent.Caller = []string{"", "claude", "a b"}[r.Intn(3)]
	c.Edit.Editor = []string{"", "code --wait", `vi "x"`}[r.Intn(3)]
	return c
}

// TestSaveParseRoundTrip: Parse(Save(c)) == c for valid configs, and Save is a fixed point.
func TestSaveParseRoundTrip(t *testing.T) {
	for seed := int64(0); seed < 300; seed++ {
		c := randomConfig(rand.New(rand.NewSource(seed)))
		if err := c.Validate("gen"); err != nil {
			t.Fatalf("seed %d: generator made an invalid config: %v", seed, err)
		}
		out, err := Save(c)
		if err != nil {
			t.Fatal(err)
		}
		back, err := Parse(out, "saved")
		if err != nil {
			t.Fatalf("seed %d: Parse(Save(c)): %v\n%s", seed, err, out)
		}
		if !reflect.DeepEqual(back, c) {
			t.Fatalf("seed %d: round trip changed the config\n got %+v\nwant %+v\n%s", seed, back, c, out)
		}
		again, _ := Save(back)
		if string(again) != string(out) {
			t.Fatalf("seed %d: Save is not idempotent", seed)
		}
	}
}

// FuzzParseSave: whatever Parse accepts survives Save and Parse unchanged; nothing panics.
func FuzzParseSave(f *testing.F) {
	d, _ := Save(Default())
	for _, s := range []string{
		"", string(d), "[search]\nk = 3\n", "bogus = 1", "[search]\nk = \"x\"", "[[sources]]\n",
		"[log]\nopen_window = \"1h\"\n", "[search.bm25]\ntitle = 1e400\n",
		"[embed]\nmodel = \"none\"\n", "version = 1\n[agent]\ncaller = \"\\u00e9\"\n",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		c, err := Parse([]byte(in), "fuzz.toml")
		if err != nil {
			if errs.KindOf(err) != errs.Usage {
				t.Fatalf("Parse error kind %v, want Usage: %v", errs.KindOf(err), err)
			}
			return
		}
		out, err := Save(c)
		if err != nil {
			t.Fatalf("Save of a parsed config: %v", err)
		}
		back, err := Parse(out, "saved")
		if err != nil {
			t.Fatalf("Parse(Save(c)) failed: %v\n%s", err, out)
		}
		if !reflect.DeepEqual(back, c) {
			t.Fatalf("round trip changed the config\n got %+v\nwant %+v", back, c)
		}
		if _, _, err := c.EffectiveJSON(); err != nil {
			t.Fatalf("EffectiveJSON of a valid config: %v", err)
		}
	})
}

func TestOpenWindowDuration(t *testing.T) {
	if got := (Log{OpenWindow: "30m"}).OpenWindowDuration().Minutes(); got != 30 {
		t.Errorf("30m = %v minutes", got)
	}
	if got := (Log{OpenWindow: "nonsense"}).OpenWindowDuration(); got != 0 {
		t.Errorf("unparsable = %v, want 0", got)
	}
}

var _ = errors.Is

// TestReadLimited is the boundary table for the in-read cap (Q62): a file that grew past the cap
// after readCapped's stat is refused as Usage; exactly the cap passes; a failing reader is Usage.
func TestReadLimited(t *testing.T) {
	for _, tc := range []struct {
		name    string
		r       io.Reader
		wantErr bool
	}{
		{"empty", strings.NewReader(""), false},
		{"cap-1", strings.NewReader(strings.Repeat("a", MaxFileBytes-1)), false},
		{"cap", strings.NewReader(strings.Repeat("a", MaxFileBytes)), false},
		{"cap+1 (grew after the stat)", strings.NewReader(strings.Repeat("a", MaxFileBytes+1)), true},
		{"endless", zeroReader{}, true},
		{"read error", iotest.ErrReader(errors.New("boom")), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := readLimited("config.Read", "config", "p", tc.r)
			if tc.wantErr {
				if errs.KindOf(err) != errs.Usage || data != nil {
					t.Fatalf("err = %v (kind %v), data %d bytes; want Usage and no data", err, errs.KindOf(err), len(data))
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
		})
	}
}

// zeroReader never ends, like /dev/zero.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// TestReservedNamePredicates is the decision table for the shared reserved-name predicates (Q76):
// exact, case-sensitive for discovery; the Fold form also matches the lowercase spellings kb add
// accepts. Config validation uses the never-walked table, so a source under any of them is refused.
func TestReservedNamePredicates(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		neverWalked, entryDir bool
		file, fileFold        bool
	}{
		{name: ".kb", neverWalked: true, entryDir: true},
		{name: ".git", neverWalked: true, entryDir: true},
		{name: "node_modules", neverWalked: true},
		{name: "skill", entryDir: true},
		{name: "AGENTS.md", file: true, fileFold: true},
		{name: "agents.md", fileFold: true},
		{name: "index.md", file: true, fileFold: true},
		{name: "INDEX.md", fileFold: true},
		{name: "plan.md", file: true, fileFold: true},
		{name: "README.md", file: true, fileFold: true},
		{name: "readme.md", fileFold: true},
		{name: "notes.md"},
		{name: "data"},
		{name: ""},
	} {
		if got := NeverWalkedDir(tc.name); got != tc.neverWalked {
			t.Errorf("NeverWalkedDir(%q) = %v, want %v", tc.name, got, tc.neverWalked)
		}
		if got := ReservedEntryDir(tc.name); got != tc.entryDir {
			t.Errorf("ReservedEntryDir(%q) = %v, want %v", tc.name, got, tc.entryDir)
		}
		if got := ReservedEntryFile(tc.name); got != tc.file {
			t.Errorf("ReservedEntryFile(%q) = %v, want %v", tc.name, got, tc.file)
		}
		if got := ReservedEntryFileFold(tc.name); got != tc.fileFold {
			t.Errorf("ReservedEntryFileFold(%q) = %v, want %v", tc.name, got, tc.fileFold)
		}
	}
	for _, p := range []string{".kb", ".git/x", "node_modules/a/b"} {
		if err := checkSourcePath(p); err == nil {
			t.Errorf("checkSourcePath(%q) = nil, want a refusal", p)
		}
	}
}
