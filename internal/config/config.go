package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/BurntSushi/toml"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/glob"
)

// Config is the whole of a store's config file with every default filled in. See the package doc for
// which keys a command reads today.
type Config struct {
	Version int      `toml:"version"`
	Embed   Embed    `toml:"embed"`
	Sources []Source `toml:"sources"` // the store's corpus; none = nothing indexed
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

// Source is one [[sources]] table: where a store's entries come from. Paths and globs are
// slash-separated and relative to the location root (not the store directory).
//
//   - layout "entries": Path names a directory kb owns (kb add copies into it). Its *.md files and
//     its subdirectories with a README.md are entries, with strict front matter. Glob and Exclude
//     are not used.
//   - layout "files": every *.md file Glob matches, minus Exclude, is an entry of its own, front
//     matter optional. Path is not used. kb never writes or deletes these files.
type Source struct {
	Path    string   `toml:"path"`
	Glob    string   `toml:"glob"`
	Layout  string   `toml:"layout"`
	Exclude []string `toml:"exclude"`
}

// Search is [search]: retrieval settings. K, the limits, rrf_k, vec_overfetch, bm25, the fts, vec
// and exact list weights, exact_list, synonyms, spell and stores drive kb search; the other keys
// are reserved (see the package doc).
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

// Output is [output]. Budget and Expand are read by kb search; ContextBudget by kb context.
type Output struct {
	Budget        int    `toml:"budget"`         // tokens for kb search; 0 = unlimited
	ContextBudget int    `toml:"context_budget"` // default --budget for kb context (2000)
	Expand        string `toml:"expand"`         // none | section | entry
}

// Log is [log]. OpenWindow is read by kb show; CorpusRev is reserved.
type Log struct {
	CorpusRev  bool   `toml:"corpus_rev"`
	OpenWindow string `toml:"open_window"` // a Go duration; kb show counts an open for searches this recent
}

// Stale is [stale]. Days is the default --days of kb stale (PLAN 5.4): an entry dated that many
// whole days ago or more is listed.
type Stale struct {
	Days int `toml:"days"`
}

// Eval is [eval], read by kb eval. MinJudged: fewer chunk-level queries than this and the report
// says the sample is small (0 never does), and kb eval --sweep refuses to run without --force.
// Holdout is the share of judged queries a sweep holds out to report its best setting on.
type Eval struct {
	MinJudged int     `toml:"min_judged"`
	Holdout   float64 `toml:"holdout"`
}

// Prior is [prior], the feedback prior of kb search (PLAN 6.2). Enabled turns it on; it applies
// once MinJudged searches carry a verdict or an open. QueryMemory is read by PLAN 6.3.
type Prior struct {
	Enabled      bool    `toml:"enabled" json:"enabled"`
	MinJudged    int     `toml:"min_judged" json:"min_judged"`
	HalfLifeDays int     `toml:"half_life_days" json:"half_life_days"`
	MaxBoost     float64 `toml:"max_boost" json:"max_boost"`
	QueryMemory  bool    `toml:"query_memory" json:"query_memory"`
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

// MaxFileBytes is the largest config or location file kb reads (1 MiB). A bigger file is refused
// before it is read, so a stray or hostile multi-GB file costs one stat, not its full size in memory.
const MaxFileBytes = 1 << 20

// Read parses the file at path onto the defaults and validates it. found is false, with the
// defaults and a nil error, when the file does not exist. Errors are Usage and name the file (and
// the key or line). A file larger than MaxFileBytes, or one that is not a regular file (a
// directory, a FIFO, a device such as a symlink to /dev/zero), is refused unread.
func Read(path string) (c Config, found bool, err error) {
	data, regular, err := readCapped("config.Read", "config", path)
	if errors.Is(err, fs.ErrNotExist) {
		return Default(), false, nil
	}
	if err != nil {
		return Config{}, regular, err
	}
	c, err = Parse(data, path)
	if err != nil {
		return Config{}, true, err
	}
	return c, true, nil
}

// readCapped reads a small text file kb owns: a regular file of at most MaxFileBytes. regular
// reports that path is a regular file (also when it is refused for its size). A missing file is
// returned as the os error (wrapping fs.ErrNotExist) for the caller to classify; every other
// failure is a Usage error naming the file. what names the file kind in messages.
//
// The stat check refuses non-regular files before they are opened (opening a FIFO would block,
// and a device never ends); the limited read still refuses a file that grew past the cap after
// the stat.
func readCapped(op, what, path string) (data []byte, regular bool, err error) {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, false, err
	case err != nil:
		return nil, false, errs.Wrap(err, errs.Usage, op, "cannot read %s %s: %v", what, path, err)
	case !info.Mode().IsRegular():
		return nil, false, errs.New(errs.Usage, op, "%s: is %s, not a regular file; a %s file must be a plain file", path, fileKind(info.Mode()), what)
	case info.Size() > MaxFileBytes:
		return nil, true, errs.New(errs.Usage, op, "%s: file is %d bytes; a %s file may be at most %d bytes (1 MiB)", path, info.Size(), what, MaxFileBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, err // removed after the stat
		}
		return nil, true, errs.Wrap(err, errs.Usage, op, "cannot read %s %s: %v", what, path, err)
	}
	defer f.Close() // read-only: a close error loses nothing
	data, err = readLimited(op, what, path, f)
	return data, true, err
}

// ReadCapped reads a small text file kb owns (what names the kind in messages): a regular file of
// at most MaxFileBytes, the rule Read applies to config files. A missing file is the os error
// (errors.Is fs.ErrNotExist) for the caller to classify; any other failure is a Usage error.
func ReadCapped(op, what, path string) ([]byte, error) {
	data, _, err := readCapped(op, what, path)
	return data, err
}

// readLimited reads r through the cap. It is the check that holds when a file grows between
// readCapped's stat and its read (the stat alone cannot see that), split out so a test can feed it
// a reader longer than the cap without a racing writer.
func readLimited(op, what, path string, r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxFileBytes+1))
	if err != nil {
		return nil, errs.Wrap(err, errs.Usage, op, "cannot read %s %s: %v", what, path, err)
	}
	if len(data) > MaxFileBytes {
		return nil, errs.New(errs.Usage, op, "%s: file is over %d bytes; a %s file may be at most %d bytes (1 MiB)", path, MaxFileBytes, what, MaxFileBytes)
	}
	return data, nil
}

// fileKind names a non-regular file type for messages.
func fileKind(m fs.FileMode) string {
	switch {
	case m.IsDir():
		return "a directory"
	case m&fs.ModeNamedPipe != 0:
		return "a named pipe"
	case m&fs.ModeDevice != 0:
		return "a device"
	case m&fs.ModeSocket != 0:
		return "a socket"
	}
	return "a special file (" + m.Type().String() + ")"
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
		if math.IsNaN(v) || v < 0 || v > MaxWeight {
			return problem(key, "must be between 0 and %v (got %v)", MaxWeight, v)
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

	if err := validateSources(c.Sources, problem); err != nil {
		return err
	}

	// [search]
	s := c.Search
	if s.K < MinK || s.K > MaxK {
		return problem("search.k", "must be between %d and %d (got %d)", MinK, MaxK, s.K)
	}
	for _, f := range []struct {
		key string
		v   int
		max int // 0 = no upper bound
	}{
		{"search.list_limit", s.ListLimit, MaxListLimit},
		{"search.candidate_limit", s.CandidateLimit, MaxCandidateLimit},
		{"search.per_entry_cap", s.PerEntryCap, 0},
		{"search.rrf_k", s.RRFK, MaxRRFK},
		{"search.vec_overfetch", s.VecOverfetch, MaxVecOverfetch},
	} {
		if err := min1(f.key, f.v); err != nil {
			return err
		}
		if f.max > 0 && f.v > f.max {
			return problem(f.key, "must be between 1 and %d (got %d)", f.max, f.v)
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

// String names a source in messages: `"data" (entries)` or `"docs/**/*.md" (files)`.
func (s Source) String() string {
	if s.Layout == LayoutEntries {
		return fmt.Sprintf("%q (entries)", s.Path)
	}
	return fmt.Sprintf("%q (%s)", s.Glob, s.Layout)
}

// validateSources checks every [[sources]] table on its own, then the overlaps that need no disk
// access: two entries sources with the same or nested directories, and two identical globs. Two
// sources that match the same file through different globs are found by entry.Discover, which
// sees the files.
func validateSources(sources []Source, problem func(key, format string, args ...any) error) error {
	for i, s := range sources {
		key := fmt.Sprintf("sources[%d]", i+1)
		switch s.Layout {
		case LayoutEntries:
			if s.Path == "" {
				return problem(key+".path", `the entries layout needs a directory, such as path = "data"`)
			}
			if s.Glob != "" {
				return problem(key+".glob", `the entries layout reads a directory: set path, not glob (or use layout = "files")`)
			}
			if len(s.Exclude) > 0 {
				return problem(key+".exclude", `only the files layout takes exclude`)
			}
			if err := checkSourcePath(s.Path); err != nil {
				return problem(key+".path", "%v", err)
			}
		case LayoutFiles:
			if s.Glob == "" {
				return problem(key+".glob", `the files layout needs a glob, such as glob = "docs/**/*.md"`)
			}
			if s.Path != "" {
				return problem(key+".path", `the files layout matches a glob: set glob, not path (or use layout = "entries")`)
			}
			if err := checkSourceGlob(s.Glob); err != nil {
				return problem(key+".glob", "%v", err)
			}
			for _, ex := range s.Exclude {
				if err := checkSourceGlob(ex); err != nil {
					return problem(key+".exclude", "%v", err)
				}
			}
		default:
			return problem(key+".layout", "must be %q or %q (got %q)", LayoutEntries, LayoutFiles, s.Layout)
		}
	}
	for i, a := range sources {
		for j := i + 1; j < len(sources); j++ {
			b := sources[j]
			key := fmt.Sprintf("sources[%d]", j+1)
			switch {
			case a.Layout == LayoutEntries && b.Layout == LayoutEntries && (a.Path == b.Path ||
				strings.HasPrefix(b.Path, a.Path+"/") || strings.HasPrefix(a.Path, b.Path+"/")):
				return problem(key+".path", "overlaps sources[%d] (%s and %s); a file may belong to one source of a store", i+1, a, b)
			case a.Layout == LayoutFiles && b.Layout == LayoutFiles && a.Glob == b.Glob:
				return problem(key+".glob", "repeats sources[%d] (%s); a file may belong to one source of a store", i+1, a)
			}
		}
	}
	return nil
}

// The reserved-name tables below are the one place kb says which names are machinery. Config
// validation (checkSourcePath), entry discovery (internal/entry) and kb add (internal/cli) all ask
// the predicates, so a new reserved name is one edit. config is the package all three already
// import and it imports none of them.

// neverWalkedDirs are the directory names kb never indexes or descends into, at any depth: its own
// stores (.kb, also a nested location's), git's metadata and npm's dependency tree.
var neverWalkedDirs = []string{".kb", ".git", "node_modules"}

// reservedEntryDirs are the directories in an entries directory that are never entries even with
// a README.md (the never-walked ".kb" and ".git" among them, plus "skill").
var reservedEntryDirs = []string{"skill", ".kb", ".git"}

// reservedEntryFiles are the *.md files in an entries directory that are repository machinery,
// not entries.
var reservedEntryFiles = []string{"AGENTS.md", "CLAUDE.md", "index.md", "plan.md", "README.md"}

// NeverWalkedDir reports whether a directory of this name is never walked or indexed.
func NeverWalkedDir(name string) bool { return slices.Contains(neverWalkedDirs, name) }

// ReservedEntryDir reports whether a directory of this name is never a directory entry.
func ReservedEntryDir(name string) bool { return slices.Contains(reservedEntryDirs, name) }

// ReservedEntryFile reports whether a file of exactly this name is never an entry.
func ReservedEntryFile(name string) bool { return slices.Contains(reservedEntryFiles, name) }

// ReservedEntryFileFold is ReservedEntryFile ignoring case: kb add takes lowercase names only, and
// "agents.md" would be the same file as AGENTS.md on a case-insensitive filesystem.
func ReservedEntryFileFold(name string) bool {
	return slices.ContainsFunc(reservedEntryFiles, func(r string) bool { return strings.EqualFold(r, name) })
}

// checkSourcePath checks an entries directory: relative, clean, inside the root and not reserved.
func checkSourcePath(p string) error {
	if err := checkSourceText(p); err != nil {
		return err
	}
	if path.IsAbs(p) || filepath.IsAbs(p) {
		return fmt.Errorf("%q is absolute; source paths are relative to the location root", p)
	}
	if path.Clean(p) != p || p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return fmt.Errorf("%q must be a clean directory path inside the location root, such as \"data\"", p)
	}
	for _, r := range neverWalkedDirs {
		if p == r || strings.HasPrefix(p, r+"/") {
			return fmt.Errorf("%q is inside %s, which kb never indexes", p, r)
		}
	}
	return nil
}

// checkSourceGlob checks a files glob or an exclude pattern.
func checkSourceGlob(g string) error {
	if err := checkSourceText(g); err != nil {
		return err
	}
	return glob.Valid(g)
}

// checkSourceText refuses control characters, so every path and glob prints on one line.
func checkSourceText(s string) error {
	for _, r := range s {
		if unicode.IsControl(r) {
			return fmt.Errorf("%q contains a control character", s)
		}
	}
	return nil
}
