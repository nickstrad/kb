package config

import (
	"encoding/json"
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
	if len(top) != 2 || top["embed"] == nil || top["search"] == nil {
		t.Errorf("top-level keys = %v, want exactly embed and search", keys(top))
	}
	// Canonical: keys sorted at every level, no whitespace.
	if !strings.HasPrefix(string(data), `{"embed":{"batch":16,"dim":0,"int8":false,"model":"BAAI/bge-small-en-v1.5","workers":0},"search":{"bm25":{"heading":2,"text":1,"title":3},"candidate_limit":40,`) {
		t.Errorf("not canonical: %s", data)
	}
	if len(hash) != 64 {
		t.Errorf("hash %q is not a hex sha256", hash)
	}

	// Same settings, same bytes; a different setting, a different hash; a setting outside
	// [embed]+[search] does not change it.
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
