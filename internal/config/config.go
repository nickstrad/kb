package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/nickstrad/kb/internal/errs"
)

// Config is the whole of a store's config file with every default filled in. See the package doc for
// which keys a command reads today.
type Config struct {
	Version int      `toml:"version"`
	Embed   Embed    `toml:"embed"`
	Sources []Source `toml:"sources"` // reserved; non-empty is rejected for now
	Search  Search   `toml:"search"`
	Output  Output   `toml:"output"`
	Log     Log      `toml:"log"`
	Stale   Stale    `toml:"stale"`
	Eval    Eval     `toml:"eval"`
	Prior   Prior    `toml:"prior"`
	Agent   Agent    `toml:"agent"`
	Edit    Edit     `toml:"edit"`
}

// Embed is [embed]: which model builds the vectors. Model, Dim and Int8 are the identity of the
// index recorded in embed_meta.
type Embed struct {
	Model   string `toml:"model" json:"model"`     // a supported model id, or "none" (FTS only)
	Dim     int    `toml:"dim" json:"dim"`         // reserved: only 0 (native width) is accepted
	Int8    bool   `toml:"int8" json:"int8"`       // reserved: only false is accepted
	Workers int    `toml:"workers" json:"workers"` // reserved: 0 = rembed default
	Batch   int    `toml:"batch" json:"batch"`     // texts per Embed call during indexing
}

// Source is one [[sources]] table. Reserved: in-place sources arrive with PLAN 1.5.
type Source struct {
	Path    string   `toml:"path"`
	Glob    string   `toml:"glob"`
	Layout  string   `toml:"layout"`
	Exclude []string `toml:"exclude"`
}

// Search is [search]: retrieval settings. Only K is read by a command today; the rest are the
// constants of the search package until its trace work passes them in.
type Search struct {
	K                int         `toml:"k" json:"k"`
	ListLimit        int         `toml:"list_limit" json:"list_limit"`
	CandidateLimit   int         `toml:"candidate_limit" json:"candidate_limit"`
	PerEntryCap      int         `toml:"per_entry_cap" json:"per_entry_cap"`
	RRFK             int         `toml:"rrf_k" json:"rrf_k"`
	VecOverfetch     int         `toml:"vec_overfetch" json:"vec_overfetch"`
	BM25             BM25        `toml:"bm25" json:"bm25"`
	ListWeights      ListWeights `toml:"list_weights" json:"list_weights"`
	PhraseBoost      bool        `toml:"phrase_boost" json:"phrase_boost"`
	ExactList        bool        `toml:"exact_list" json:"exact_list"`
	Synonyms         string      `toml:"synonyms" json:"synonyms"`
	Spell            bool        `toml:"spell" json:"spell"`
	SupersedePenalty float64     `toml:"supersede_penalty" json:"supersede_penalty"`
	Stores           []string    `toml:"stores" json:"stores"`
}

// BM25 holds the per-column FTS5 bm25 weights.
type BM25 struct {
	Text    float64 `toml:"text" json:"text"`
	Heading float64 `toml:"heading" json:"heading"`
	Title   float64 `toml:"title" json:"title"`
}

// ListWeights are the reciprocal-rank-fusion multipliers of each ranked list.
type ListWeights struct {
	FTS    float64 `toml:"fts" json:"fts"`
	Vec    float64 `toml:"vec" json:"vec"`
	Exact  float64 `toml:"exact" json:"exact"`
	Memory float64 `toml:"memory" json:"memory"`
}

// Output is [output]. Reserved.
type Output struct {
	Budget        int    `toml:"budget"`         // tokens for kb search; 0 = unlimited
	ContextBudget int    `toml:"context_budget"` // default for kb context
	Expand        string `toml:"expand"`         // none | section | entry
}

// Log is [log]. OpenWindow is read by kb show; CorpusRev is reserved.
type Log struct {
	CorpusRev  bool   `toml:"corpus_rev"`
	OpenWindow string `toml:"open_window"` // a Go duration; kb show counts an open for searches this recent
}

// Stale is [stale]. Reserved.
type Stale struct {
	Days int `toml:"days"`
}

// Eval is [eval]. Reserved.
type Eval struct {
	MinJudged int     `toml:"min_judged"`
	Holdout   float64 `toml:"holdout"`
}

// Prior is [prior]. Reserved.
type Prior struct {
	Enabled      bool    `toml:"enabled"`
	MinJudged    int     `toml:"min_judged"`
	HalfLifeDays int     `toml:"half_life_days"`
	MaxBoost     float64 `toml:"max_boost"`
	QueryMemory  bool    `toml:"query_memory"`
}

// Agent is [agent]: who the person or tool running kb is.
type Agent struct {
	// Caller is the default --caller of kb search and kb feedback; "" means "unknown".
	Caller string `toml:"caller"`
}

// Edit is [edit].
type Edit struct {
	// Editor is the command kb edit runs; "" means $EDITOR, then vi.
	Editor string `toml:"editor"`
}

// OpenWindowDuration is Log.OpenWindow as a duration. A validated Config always parses; an
// unvalidated one yields 0.
func (l Log) OpenWindowDuration() time.Duration {
	d, err := time.ParseDuration(l.OpenWindow)
	if err != nil {
		return 0
	}
	return d
}

// Read parses the file at path onto the defaults and validates it. found is false, with the
// defaults and a nil error, when the file does not exist. Errors are Usage and name the file (and
// the key or line).
func Read(path string) (c Config, found bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Default(), false, nil
	}
	if err != nil {
		return Config{}, false, errs.Wrap(err, errs.Usage, "config.Read", "cannot read config %s: %v", path, err)
	}
	c, err = Parse(data, path)
	if err != nil {
		return Config{}, true, err
	}
	return c, true, nil
}

// Load is Read without the found flag: a missing file gives the defaults.
func Load(path string) (Config, error) {
	c, _, err := Read(path)
	return c, err
}

// Parse decodes TOML text onto the defaults and validates it. file names the origin in errors.
func Parse(data []byte, file string) (Config, error) {
	// The schema version is read first, so a file from another kb version gets a version message
	// and not an "unknown key" error about keys its schema has and ours lacks. A syntax error or a
	// non-integer version falls through to the strict decode, which reports it with its line.
	var head map[string]any
	if _, err := toml.Decode(string(data), &head); err == nil {
		if v, ok := head["version"].(int64); ok {
			if err := checkVersion(file, v, SchemaVersion); err != nil {
				return Config{}, err
			}
		}
	}
	c := Default()
	md, err := toml.Decode(string(data), &c)
	if err != nil {
		return Config{}, decodeErr(file, err)
	}
	if un := md.Undecoded(); len(un) > 0 {
		keys := make([]string, len(un))
		for i, k := range un {
			keys[i] = k.String()
		}
		sort.Strings(keys)
		noun := "key"
		if len(keys) > 1 {
			noun = "keys"
		}
		return Config{}, errs.New(errs.Usage, "config.Parse", "%s: unknown %s %s", file, noun, quoteJoin(keys))
	}
	normalize(&c)
	if err := c.Validate(file); err != nil {
		return Config{}, err
	}
	return c, nil
}

// checkVersion rejects a schema version other than current with a message that names the remedy.
// Versions below 1 are left to Validate.
func checkVersion(file string, v, current int64) error {
	switch {
	case v > current:
		return errs.New(errs.Usage, "config.Parse", "%s: version: config schema v%d was written by a newer kb (this kb reads v%d)", file, v, current)
	case v >= 1 && v < current:
		return errs.New(errs.Conflict, "config.Parse", "%s: version: config schema v%d is older than this kb's v%d; run kb migrate up", file, v, current)
	}
	return nil
}

// decodeErr turns a TOML syntax or type error into a Usage error naming the file and line.
func decodeErr(file string, err error) error {
	var pe toml.ParseError
	if errors.As(err, &pe) {
		where := file
		if pe.Position.Line > 0 {
			where = fmt.Sprintf("%s:%d", file, pe.Position.Line)
		}
		key := ""
		if pe.LastKey != "" {
			key = pe.LastKey + ": "
		}
		return errs.Wrap(err, errs.Usage, "config.Parse", "%s: %s%s", where, key, pe.Message)
	}
	// A type mismatch is a plain error of the form `toml: line 2 (last key "search.k"): msg`.
	if m := typeErrRE.FindStringSubmatch(err.Error()); m != nil {
		return errs.Wrap(err, errs.Usage, "config.Parse", "%s:%s: %s: %s", file, m[1], m[2], m[3])
	}
	return errs.Wrap(err, errs.Usage, "config.Parse", "%s: %v", file, err)
}

var typeErrRE = regexp.MustCompile(`^toml: line (\d+) \(last key "([^"]*)"\): (.*)$`)

func quoteJoin(keys []string) string {
	q := make([]string, len(keys))
	for i, k := range keys {
		q[i] = fmt.Sprintf("%q", k)
	}
	return strings.Join(q, ", ")
}

// normalize makes nil slices and maps empty, so a decoded Config equals one built by hand.
func normalize(c *Config) {
	if c.Sources == nil {
		c.Sources = []Source{}
	}
	for i := range c.Sources {
		if c.Sources[i].Exclude == nil {
			c.Sources[i].Exclude = []string{}
		}
	}
	if c.Search.Stores == nil {
		c.Search.Stores = []string{}
	}
}

// Save renders c as TOML: every key, in schema order. Parse of the result gives c back.
func Save(c Config) ([]byte, error) {
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(c); err != nil {
		return nil, errs.Wrap(err, errs.Internal, "config.Save", "cannot encode the configuration")
	}
	return buf.Bytes(), nil
}

// Validate checks every value. src names where c came from ("--set k=v", or a file path); the
// first problem is returned as a Usage error "<src>: <key>: <problem>".
func (c Config) Validate(src string) error {
	var problem func(key, format string, args ...any) error = func(key, format string, args ...any) error {
		return errs.New(errs.Usage, "config.Validate", "%s: %s: %s", src, key, fmt.Sprintf(format, args...))
	}
	min1 := func(key string, v int) error {
		if v < 1 {
			return problem(key, "must be at least 1 (got %d)", v)
		}
		return nil
	}
	min0 := func(key string, v int) error {
		if v < 0 {
			return problem(key, "must not be negative (got %d)", v)
		}
		return nil
	}
	weight := func(key string, v float64) error {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return problem(key, "must be a finite number, 0 or more (got %v)", v)
		}
		return nil
	}
	frac := func(key string, v float64, upperInclusive bool) error {
		if math.IsNaN(v) || v < 0 || v > 1 || (!upperInclusive && v == 1) {
			bound := ")"
			if upperInclusive {
				bound = "]"
			}
			return problem(key, "must be in [0, 1%s (got %v)", bound, v)
		}
		return nil
	}

	if c.Version != SchemaVersion {
		return problem("version", "unsupported config version %d; this kb reads version %d", c.Version, SchemaVersion)
	}

	// [embed]. The model id is checked against the supported table by the caller (CheckModel).
	if strings.TrimSpace(c.Embed.Model) == "" {
		return problem("embed.model", `must name a model, or "none" for full-text search only`)
	}
	if c.Embed.Dim != 0 {
		return problem("embed.dim", "not supported yet: only 0 (the model's native width) is accepted (got %d)", c.Embed.Dim)
	}
	if c.Embed.Int8 {
		return problem("embed.int8", "not supported yet: only false is accepted")
	}
	if err := min0("embed.workers", c.Embed.Workers); err != nil {
		return err
	}
	if err := min1("embed.batch", c.Embed.Batch); err != nil {
		return err
	}

	if len(c.Sources) > 0 {
		return problem("sources", "not supported yet: kb reads the entries under data/ only")
	}

	// [search]
	s := c.Search
	if s.K < MinK || s.K > MaxK {
		return problem("search.k", "must be between %d and %d (got %d)", MinK, MaxK, s.K)
	}
	for _, f := range []struct {
		key string
		v   int
	}{
		{"search.list_limit", s.ListLimit},
		{"search.candidate_limit", s.CandidateLimit},
		{"search.per_entry_cap", s.PerEntryCap},
		{"search.rrf_k", s.RRFK},
		{"search.vec_overfetch", s.VecOverfetch},
	} {
		if err := min1(f.key, f.v); err != nil {
			return err
		}
	}
	if s.CandidateLimit < s.K {
		return problem("search.candidate_limit", "must be at least search.k (%d), got %d", s.K, s.CandidateLimit)
	}
	for _, f := range []struct {
		key string
		v   float64
	}{
		{"search.bm25.text", s.BM25.Text},
		{"search.bm25.heading", s.BM25.Heading},
		{"search.bm25.title", s.BM25.Title},
		{"search.list_weights.fts", s.ListWeights.FTS},
		{"search.list_weights.vec", s.ListWeights.Vec},
		{"search.list_weights.exact", s.ListWeights.Exact},
		{"search.list_weights.memory", s.ListWeights.Memory},
	} {
		if err := weight(f.key, f.v); err != nil {
			return err
		}
	}
	if err := frac("search.supersede_penalty", s.SupersedePenalty, true); err != nil {
		return err
	}
	for _, name := range s.Stores {
		if strings.TrimSpace(name) == "" {
			return problem("search.stores", "must not contain an empty store name")
		}
	}

	// [output]
	if err := min0("output.budget", c.Output.Budget); err != nil {
		return err
	}
	if err := min1("output.context_budget", c.Output.ContextBudget); err != nil {
		return err
	}
	switch c.Output.Expand {
	case ExpandNone, ExpandSection, ExpandEntry:
	default:
		return problem("output.expand", "must be %s, %s or %s (got %q)", ExpandNone, ExpandSection, ExpandEntry, c.Output.Expand)
	}

	// [log]
	d, err := time.ParseDuration(c.Log.OpenWindow)
	if err != nil {
		return problem("log.open_window", "not a duration such as 30m or 2h (got %q)", c.Log.OpenWindow)
	}
	if d <= 0 {
		return problem("log.open_window", "must be longer than 0 (got %q)", c.Log.OpenWindow)
	}

	if err := min1("stale.days", c.Stale.Days); err != nil {
		return err
	}
	if err := min0("eval.min_judged", c.Eval.MinJudged); err != nil {
		return err
	}
	if err := frac("eval.holdout", c.Eval.Holdout, false); err != nil {
		return err
	}
	if err := min0("prior.min_judged", c.Prior.MinJudged); err != nil {
		return err
	}
	if err := min1("prior.half_life_days", c.Prior.HalfLifeDays); err != nil {
		return err
	}
	if err := frac("prior.max_boost", c.Prior.MaxBoost, true); err != nil {
		return err
	}

	// [agent], [edit]: one line, no control characters.
	if strings.ContainsAny(c.Agent.Caller, "\r\n") {
		return problem("agent.caller", "must be one line")
	}
	if strings.ContainsAny(c.Edit.Editor, "\r\n") {
		return problem("edit.editor", "must be one line")
	}

	return nil
}

// CheckModel validates embed.model against the caller's table of supported models. "none" always
// passes. known returns an error for an unsupported id; src and the key are added to it. The model
// table lives with the embedder (internal/embed/local), so config takes the lookup as a function.
func (c Config) CheckModel(src string, known func(id string) error) error {
	if c.Embed.Model == ModelNone {
		return nil
	}
	if err := known(c.Embed.Model); err != nil {
		return errs.Wrap(err, errs.Usage, "config.CheckModel", "%s: embed.model: %v", src, err)
	}
	return nil
}
