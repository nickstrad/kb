package config

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/errs"
)

func TestApply(t *testing.T) {
	tests := []struct {
		name   string
		assign string
		check  func(c Config) bool
		errHas []string // nil = must succeed
	}{
		{"int", "search.rrf_k=30", func(c Config) bool { return c.Search.RRFK == 30 }, nil},
		{"nested float key from an int literal", "search.bm25.title=4", func(c Config) bool { return c.Search.BM25.Title == 4 }, nil},
		{"nested float literal", "search.list_weights.vec=0.5", func(c Config) bool { return c.Search.ListWeights.Vec == 0.5 }, nil},
		{"bool", "search.spell=false", func(c Config) bool { return !c.Search.Spell }, nil},
		{"bare string", "embed.model=none", func(c Config) bool { return c.Embed.Model == "none" }, nil},
		{"quoted string", `agent.caller="my tool"`, func(c Config) bool { return c.Agent.Caller == "my tool" }, nil},
		{"string with equals sign", "edit.editor=vi -c a=b", func(c Config) bool { return c.Edit.Editor == "vi -c a=b" }, nil},
		{"duration string", "log.open_window=2h", func(c Config) bool { return c.Log.OpenWindow == "2h" }, nil},
		{"top-level int", "version=1", func(c Config) bool { return c.Version == 1 }, nil},
		{"bare list", "search.stores=global,notes", func(c Config) bool { return reflect.DeepEqual(c.Search.Stores, []string{"global", "notes"}) }, nil},
		{"toml list", `search.stores=["a"]`, func(c Config) bool { return reflect.DeepEqual(c.Search.Stores, []string{"a"}) }, nil},
		{"empty list", "search.stores=", func(c Config) bool { return len(c.Search.Stores) == 0 }, nil},
		{"spaces around", " search.k = 3 ", func(c Config) bool { return c.Search.K == 3 }, nil},

		{"no equals", "search.k", nil, []string{"want key=value"}},
		{"empty path", "=3", nil, []string{"want key=value"}},
		{"unknown top key", "nope=1", nil, []string{`unknown key "nope"`}},
		{"unknown nested key", "search.nope=1", nil, []string{`unknown key "search.nope"`}},
		{"unknown below a leaf", "search.k.deep=1", nil, []string{`"search.k" is a value, not a table`}},
		{"a table", "search.bm25=3", nil, []string{`"search.bm25" is a table`, "search.bm25.text"}},
		{"array of tables", "sources=1", nil, []string{"edit the config file"}},
		{"bad type string for int", "search.k=eight", nil, []string{"--set search.k=eight:"}},
		{"bad type float for int", "search.k=2.5", nil, []string{"--set search.k=2.5:", "incompatible types"}},
		{"bad type string for bool", "search.spell=maybe", nil, []string{"--set search.spell=maybe:"}},
		{"missing value for int", "search.k=", nil, []string{"missing value"}},
		{"bad value below range", "search.k=0", nil, []string{"search.k: must be between 1 and 20 (got 0)"}},
		{"bad value above range", "search.k=21", nil, []string{"search.k: must be between 1 and 20"}},
		{"bad value rrf_k", "search.rrf_k=0", nil, []string{"search.rrf_k"}},
		{"bad enum", "output.expand=page", nil, []string{"output.expand"}},
		{"bad duration", "log.open_window=soon", nil, []string{"log.open_window"}},
		{"reserved value", "embed.dim=128", nil, []string{"embed.dim", "not supported yet"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := Default()
			got, err := Apply(base, tt.assign)
			if tt.errHas == nil {
				if err != nil {
					t.Fatalf("Apply(%q) = %v", tt.assign, err)
				}
				if !tt.check(got) {
					t.Errorf("Apply(%q) did not set the key: %+v", tt.assign, got)
				}
				if !reflect.DeepEqual(base, Default()) {
					t.Errorf("Apply modified its argument")
				}
				return
			}
			if err == nil {
				t.Fatalf("Apply(%q) accepted it", tt.assign)
			}
			if errs.KindOf(err) != errs.Usage {
				t.Errorf("kind = %v, want Usage", errs.KindOf(err))
			}
			if !strings.Contains(err.Error(), tt.assign[:strings.IndexAny(tt.assign+"=", "=")]) {
				t.Errorf("message %q does not name the key", err)
			}
			if strings.Contains(err.Error(), "toml:") {
				t.Errorf("message %q leaks the decoder prefix", err)
			}
			for _, w := range tt.errHas {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("message %q lacks %q", err, w)
				}
			}
			if !reflect.DeepEqual(base, Default()) {
				t.Errorf("a failed Apply modified its argument")
			}
		})
	}
}

func TestEffectiveJSON(t *testing.T) {
	c := Default()
	data, hash, err := c.EffectiveJSON()
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatal(err)
	}
	if len(top) != 3 || top["embed"] == nil || top["search"] == nil || top["prior"] == nil {
		t.Errorf("top-level keys = %v, want exactly embed, prior and search", keys(top))
	}
	// Canonical: keys sorted at every level, no whitespace.
	if !strings.HasPrefix(string(data), `{"embed":{"batch":16,"dim":0,"int8":false,"model":"BAAI/bge-small-en-v1.5","workers":0},"prior":{"enabled":false,"half_life_days":90,"max_boost":0.2,"min_judged":50,"query_memory":false},"search":{"bm25":{"heading":2,"text":1,"title":3},"candidate_limit":40,`) {
		t.Errorf("not canonical: %s", data)
	}
	if len(hash) != 64 {
		t.Errorf("hash %q is not a hex sha256", hash)
	}

	// Same settings, same bytes; a different setting, a different hash; a setting outside
	// [embed]+[search]+[prior] does not change it.
	again, hash2, _ := Default().EffectiveJSON()
	if string(again) != string(data) || hash2 != hash {
		t.Errorf("EffectiveJSON is not deterministic")
	}
	// Every key kb search reads changes the hash.
	for _, tc := range []struct {
		key  string
		edit func(*Search)
	}{
		{"search.k", func(s *Search) { s.K = 9 }},
		{"search.list_limit", func(s *Search) { s.ListLimit = 21 }},
		{"search.candidate_limit", func(s *Search) { s.CandidateLimit = 41 }},
		{"search.per_entry_cap", func(s *Search) { s.PerEntryCap = 4 }},
		{"search.rrf_k", func(s *Search) { s.RRFK = 61 }},
		{"search.vec_overfetch", func(s *Search) { s.VecOverfetch = 61 }},
		{"search.bm25.text", func(s *Search) { s.BM25.Text = 1.5 }},
		{"search.bm25.heading", func(s *Search) { s.BM25.Heading = 2.5 }},
		{"search.bm25.title", func(s *Search) { s.BM25.Title = 3.5 }},
		{"search.list_weights.fts", func(s *Search) { s.ListWeights.FTS = 0.5 }},
		{"search.list_weights.vec", func(s *Search) { s.ListWeights.Vec = 0.5 }},
	} {
		other := Default()
		tc.edit(&other.Search)
		if _, h, _ := other.EffectiveJSON(); h == hash {
			t.Errorf("changing %s did not change the hash", tc.key)
		}
	}
	// Every [prior] key reaches the hash too: a baseline run with the prior on is not comparable.
	for _, tc := range []struct {
		key  string
		edit func(*Prior)
	}{
		{"prior.enabled", func(p *Prior) { p.Enabled = true }},
		{"prior.min_judged", func(p *Prior) { p.MinJudged = 51 }},
		{"prior.half_life_days", func(p *Prior) { p.HalfLifeDays = 91 }},
		{"prior.max_boost", func(p *Prior) { p.MaxBoost = 0.3 }},
		{"prior.query_memory", func(p *Prior) { p.QueryMemory = true }},
	} {
		other := Default()
		tc.edit(&other.Prior)
		if _, h, _ := other.EffectiveJSON(); h == hash {
			t.Errorf("changing %s did not change the hash", tc.key)
		}
	}
	other := Default()
	other.Log.OpenWindow = "2h"
	other.Agent.Caller = "x"
	if _, h, _ := other.EffectiveJSON(); h != hash {
		t.Errorf("settings outside embed and search changed the hash")
	}
}

func keys(m map[string]json.RawMessage) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestApplyAs: errors name the flag the assignment came from, both the parse and the validation.
func TestApplyAs(t *testing.T) {
	for _, assign := range []string{"search.nope=1", "search.rrf_k=0"} {
		_, err := ApplyAs(Default(), "--sweep", assign)
		if errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), "--sweep "+assign) || strings.Contains(err.Error(), "--set") {
			t.Errorf("%s: %v, want a Usage error naming --sweep only", assign, err)
		}
	}
	// A parse failure reports the ApplyAs op, not Apply's.
	var e *errs.Error
	if _, err := ApplyAs(Default(), "--sweep", "search.nope=1"); !errors.As(err, &e) || e.Op != "config.ApplyAs" {
		t.Errorf("op of %v, want config.ApplyAs", err)
	}
	if c, err := ApplyAs(Default(), "--sweep", "search.rrf_k=7"); err != nil || c.Search.RRFK != 7 {
		t.Errorf("ApplyAs: %v %d", err, c.Search.RRFK)
	}
}

// TestApplyProviderTables: --set reaches the optional provider sub-tables one key at a time, in
// any order, and never shares a sub-table with the Config it was applied to.
func TestApplyProviderTables(t *testing.T) {
	base := Default()
	c := base
	var err error
	for _, s := range []string{"embed.provider=http", "embed.http.url=https://api.example/v1", "embed.dim=768", "embed.model=text-embedding-3-small"} {
		if c, err = Apply(c, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if c.Embed.ProviderName() != ProviderHTTP || c.Embed.HTTP.URL != "https://api.example/v1" || c.Embed.Dim != 768 || c.Embed.Needs() != "" {
		t.Fatalf("got %+v %+v", c.Embed, c.Embed.HTTP)
	}
	if base.Embed.HTTP != nil {
		t.Fatal("Apply wrote into its input's [embed.http]")
	}
	d, err := Apply(c, "embed.http.path=/embed")
	if err != nil || d.Embed.HTTP.Path != "/embed" || c.Embed.HTTP.Path != "" {
		t.Fatalf("Apply shared the sub-table with its input: %v %+v %+v", err, d.Embed.HTTP, c.Embed.HTTP)
	}
	args, err := Apply(Default(), "embed.command.args=--fast,--quiet")
	if err != nil || len(args.Embed.Command.Args) != 2 {
		t.Fatalf("embed.command.args: %v %+v", err, args.Embed.Command)
	}
	// The local provider keeps its rule: dim stays 0.
	if _, err := Apply(Default(), "embed.dim=768"); err == nil || !strings.Contains(err.Error(), "only 0") {
		t.Errorf("local dim: %v", err)
	}
}

// TestEmbedProviderRules is the decision table for [embed] provider keys: Validate's checks and
// what Needs reports at build time.
func TestEmbedProviderRules(t *testing.T) {
	http := func(url string) *EmbedHTTP { return &EmbedHTTP{URL: url} }
	cases := []struct {
		name     string
		e        Embed
		validErr string // substring; "" = valid
		needs    string // substring; "" = nothing missing
	}{
		{"default", Default().Embed, "", ""},
		{"explicit local", Embed{Provider: "Local", Model: "m", Batch: 1}, "", ""},
		{"local dim", Embed{Model: "m", Dim: 8, Batch: 1}, "only 0", ""},
		{"provider with space", Embed{Provider: "my http", Model: "m", Batch: 1}, "one word", ""},
		{"http complete", Embed{Provider: "http", Model: "m", Dim: 8, Batch: 1, HTTP: http("u")}, "", ""},
		{"http no dim", Embed{Provider: "http", Model: "m", Batch: 1, HTTP: http("u")}, "", "embed.dim"},
		{"http no table", Embed{Provider: "http", Model: "m", Dim: 8, Batch: 1}, "", "embed.http.url"},
		{"http none model", Embed{Provider: "http", Model: ModelNone, Batch: 1}, "", ""},
		{"negative dim", Embed{Provider: "http", Model: "m", Dim: -1, Batch: 1}, "positive", ""},
		{"http bad timeout", Embed{Provider: "http", Model: "m", Dim: 8, Batch: 1, HTTP: &EmbedHTTP{URL: "u", TimeoutSeconds: -1}}, "timeout_seconds", ""},
		{"command complete", Embed{Provider: "command", Model: "m", Dim: 8, Batch: 1, Command: &EmbedCommand{Program: "p", Args: []string{"-x"}}}, "", ""},
		{"command no program", Embed{Provider: "command", Model: "m", Dim: 8, Batch: 1, Command: &EmbedCommand{}}, "", "embed.command.program"},
		{"command spaced arg", Embed{Provider: "command", Model: "m", Dim: 8, Batch: 1, Command: &EmbedCommand{Program: "p", Args: []string{"a b"}}}, "args[1]", ""},
		{"command empty arg", Embed{Provider: "command", Model: "m", Dim: 8, Batch: 1, Command: &EmbedCommand{Program: "p", Args: []string{""}}}, "args[1]", ""},
		{"command bad timeout", Embed{Provider: "command", Model: "m", Dim: 8, Batch: 1, Command: &EmbedCommand{Program: "p", TimeoutSeconds: -2}}, "timeout_seconds", ""},
		{"unknown provider", Embed{Provider: "corp", Model: "m", Dim: 8, Batch: 1}, "", ""},
	}
	for _, tc := range cases {
		c := Default()
		c.Embed = tc.e
		err := c.Validate("f")
		if (err != nil) != (tc.validErr != "") || (err != nil && !strings.Contains(err.Error(), tc.validErr)) {
			t.Errorf("%s: Validate = %v, want %q", tc.name, err, tc.validErr)
		}
		if err != nil {
			continue // Needs is asked only of a valid table
		}
		if got := tc.e.Needs(); (got != "") != (tc.needs != "") || !strings.Contains(got, tc.needs) {
			t.Errorf("%s: Needs = %q, want %q", tc.name, got, tc.needs)
		}
	}
}

// TestNoProviderKeepsEncodingAndHash: a config that names no provider writes no new keys and
// hashes as it did before the provider keys existed (the settings_hash in old search logs).
func TestNoProviderKeepsEncodingAndHash(t *testing.T) {
	data, _, err := Default().EffectiveJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"provider", "http", "command"} {
		if strings.Contains(string(data), `"`+k+`"`) {
			t.Errorf("EffectiveJSON of the default names %q: %s", k, data)
		}
	}
	c := Default()
	c.Embed.Provider = "http"
	c.Embed.HTTP = &EmbedHTTP{URL: "u"}
	_, h1, _ := Default().EffectiveJSON()
	_, h2, _ := c.EffectiveJSON()
	if h1 == h2 {
		t.Error("selecting a provider does not change the settings hash")
	}
}

// TestCheckModelOnlyChecksLocal: the supported-model table applies to the local provider only.
func TestCheckModelOnlyChecksLocal(t *testing.T) {
	reject := func(string) error { return errors.New("unsupported") }
	c := Default()
	c.Embed.Model = "text-embedding-3-small"
	if err := c.CheckModel("f", reject); err == nil {
		t.Error("local: an unknown model passed")
	}
	c.Embed.Provider = "http"
	if err := c.CheckModel("f", reject); err != nil {
		t.Errorf("http: %v", err)
	}
}
