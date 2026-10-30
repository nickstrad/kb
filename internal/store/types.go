package store

import (
	"errors"
	"fmt"
	"time"
)

// FTSOnlyModel is the embed_meta model of an index built with no embedder. Its dim is 0 and it
// has no vectors. It matches embed.NoneModel.
const FTSOnlyModel = "none"

// EmbedMeta names the embedder an index was built with.
type EmbedMeta struct {
	Model string
	Dim   int
}

// ErrEmbedMetaMismatch is matched with errors.Is; the concrete value is *EmbedMetaMismatchError.
var ErrEmbedMetaMismatch = errors.New("embedding model mismatch")

// EmbedMetaMismatchError reports the stored embed_meta against the configured embedder.
type EmbedMetaMismatchError struct {
	StoredModel string
	StoredDim   int
	WantModel   string
	WantDim     int
}

func (e *EmbedMetaMismatchError) Error() string {
	return fmt.Sprintf("%s: this database was indexed with %s (%d dims) but this run uses %s (%d dims); rebuild it with `kb reindex --all`",
		ErrEmbedMetaMismatch, e.StoredModel, e.StoredDim, e.WantModel, e.WantDim)
}

// Unwrap makes errors.Is(err, ErrEmbedMetaMismatch) true.
func (e *EmbedMetaMismatchError) Unwrap() error { return ErrEmbedMetaMismatch }

// EntryRow is a stored entry. Updated and Verified are "" when unset; Tags and Aliases are
// decoded, as written (Tags keeps its case; the tag filter uses the normalised form, NormTag).
type EntryRow struct {
	ID        int64
	Path      string
	Kind      string
	Title     string
	Summary   string
	Tags      []string
	Aliases   []string
	Updated   string
	Verified  string
	BodyHash  string
	IndexedAt string
}

// ChunkRow is a stored chunk.
type ChunkRow struct {
	ID         int64
	EntryID    int64
	Ord        int
	SourceFile string
	Heading    string
	Text       string
	TextHash   string
}

// EntryInput is an entry about to be written. Path ("data/foo.md") is the unique key; Kind is
// entry.KindFile or entry.KindDir. Aliases are other names the entry answers to: they join the
// title in the full-text index and resolve wikilinks (ResolveEdges). Edges are the entry's
// outgoing links; repeats of one (DstPath, Rel, Kind) are written once.
type EntryInput struct {
	Path     string
	Kind     string
	Title    string
	Summary  string
	Tags     []string
	Aliases  []string
	Updated  string
	Verified string
	BodyHash string
	Edges    []EdgeInput
}

// Edge kinds: how an edge's target was written.
const (
	// EdgePath: DstPath is a root-relative, cleaned path (a Markdown link or a front-matter
	// path, already resolved against the linking file's directory).
	EdgePath = "path"
	// EdgeWikilink: DstPath is a [[name]] as written; ResolveEdges matches it by path, basename
	// or alias (EdgeResolver).
	EdgeWikilink = "wikilink"
)

// EdgeInput is one outgoing link of an entry about to be written: its target, its relation
// (links for a body link; related, supersedes or depends_on for a front-matter list) and its
// kind (EdgePath or EdgeWikilink). None may be empty.
type EdgeInput struct {
	DstPath string
	Rel     string
	Kind    string
}

// EdgeRow is a stored edge with both ends named. DstEntryID is 0 and DstEntryPath "" while the
// target matches no entry (dangling: a broken link, or one not resolved yet).
type EdgeRow struct {
	SrcEntryID   int64
	SrcPath      string
	DstPath      string
	Rel          string
	Kind         string
	DstEntryID   int64
	DstEntryPath string
}

// ChunkInput is one chunk plus its vector. Embedding is exactly embed_meta.dim long, or nil on
// an FTS-only index.
type ChunkInput struct {
	Ord        int
	SourceFile string
	Heading    string
	Text       string
	TextHash   string
	Embedding  []float32
}

// FTSOp joins the terms of an FTS query.
type FTSOp int

const (
	OpAnd FTSOp = iota // every term must match
	OpOr               // any term may match
)

// FTSQuery is one full-text query. Terms are raw user fields (search splits on whitespace); the
// backend quotes and sanitises each one. Tag, when set, keeps only entries carrying it, compared
// in its normalised form (NormTag: trimmed, lower case), so `Docker` and `docker` filter alike
// (schema v7; before it the match was case-sensitive). Weights
// are the per-column relevance weights; nil means DefaultBM25(). ChunkIDs, when non-nil, restricts
// the query to those chunks (an empty non-nil slice matches nothing), so a caller can re-score a
// known candidate set. Exact runs the query against the identifier index instead of the main one:
// it does not stem or fold diacritics and keeps `_ - . : /` inside a token, so `foo_bar`,
// `pkg.Func`, `--flag` and `path/to` each match only themselves. It has no title column, so
// Weights.Title is ignored there (chunk text starts with the title anyway).
type FTSQuery struct {
	Terms    []string
	Op       FTSOp
	Tag      string
	Limit    int
	Weights  *BM25Weights
	ChunkIDs []int64
	Exact    bool
	// Alts maps a raw term (exactly as it appears in Terms) to extra alternatives; the term then
	// matches any of itself and them: ("k8s" OR "kubernetes"). An alternative may be several
	// words and is matched as one phrase. nil means no expansion. Synonyms (PLAN 5.5).
	Alts map[string][]string
}

// BM25Weights are the column weights of the full-text score: chunk text, heading and entry
// title. Each is finite and 0 or more; a 0 column adds nothing to the score.
type BM25Weights struct {
	Text    float64
	Heading float64
	Title   float64
}

// DefaultBM25 is the weighting an FTSQuery without Weights uses: a title hit counts three times
// a body hit, a heading hit twice.
func DefaultBM25() BM25Weights { return BM25Weights{Text: 1.0, Heading: 2.0, Title: 3.0} }

// FTSResult is the ranked rows plus the backend-native expression built from the terms ("" when
// every term sanitised away and nothing ran). An empty non-nil ChunkIDs runs nothing but still
// reports the expression.
type FTSResult struct {
	Rows  []FTSRow
	Match string
}

// FTSRow is one row of the full-text list, in rank order. Score is the backend's relevance
// score; for SQLite it is bm25(), negative and lower-is-better.
type FTSRow struct {
	ChunkID int64
	Score   float64
}

// KNNQuery asks for the K nearest chunks to Vec. With a Tag (matched as FTSQuery.Tag is) the
// backend may fetch OverFetch candidates before filtering, since it cannot filter inside the
// vector index.
type KNNQuery struct {
	Vec       []float32
	K         int
	Tag       string
	OverFetch int
}

// VecRow is one row of the vector list, in ascending distance order.
type VecRow struct {
	ChunkID  int64
	Distance float64
}

// ChunkMeta is the display data of one chunk and where it sits: its entry (id, path, updated
// date, "" when unset) and its source file and position in that entry.
type ChunkMeta struct {
	EntryPath    string
	Heading      string
	Text         string
	EntryID      int64
	EntryUpdated string
	Ord          int
	SourceFile   string
}

// WholeSearchRank is the feedback rank of a whole-search verdict (kb feedback N --none). No
// result has rank 0, so the row makes its search judged and never joins a result.
const WholeSearchRank = 0

// SearchRecord is one search as the log stores it. A nil *Ms means that stage did not run,
// which the log keeps distinct from 0 ms.
type SearchRecord struct {
	// TS is when the search ran (searches.ts, stored in UTC at one-second resolution). The caller
	// sets it (search.Searcher.Now), so tests can place a search at an exact time; the zero value
	// means "now" by the backend's clock.
	TS         time.Time
	Query      string
	Mode       string
	K          int
	Tag        string // "" = no tag filter
	EmbedModel string // "" = nothing was embedded
	NFTS       int
	NVec       int
	FTSMs      *int64
	VecMs      *int64
	EmbedMs    *int64
	TotalMs    int64
	Caller     string
	KBVersion  string
	CorpusRev  string
	// Settings is the canonical JSON of the [embed] and [search] settings the search ran with,
	// SettingsHash its hash and FTSMatch the FTS MATCH expression that produced the FTS list.
	// "" is stored as NULL: not recorded.
	Settings     string
	SettingsHash string
	FTSMatch     string
	Results      []LoggedHit // what the caller was shown; rank = index + 1
	Candidates   []LoggedHit // the fused list; fused_rank = index + 1
}

// LoggedHit is one result or candidate row. A nil rank or score means the chunk was not in
// that list (ExactRank/ExactScore: the identifier list, FTSQuery.Exact). Store is "" for a hit of the store that logs the search and otherwise the canonical
// ref NAME@ABSROOT of the store that returned it (a cross-store search); ChunkID is that store's,
// so a hit is identified by (Store, ChunkID).
type LoggedHit struct {
	Store       string
	ChunkID     int64
	EntryPath   string
	Heading     string
	FTSRank     *int
	FTSScore    *float64
	VecRank     *int
	VecDistance *float64
	ExactRank   *int
	ExactScore  *float64
	RRFScore    float64
	// Prior is the feedback-prior multiplier (PLAN 6.2) already folded into RRFScore; nil when the
	// prior was inactive for the search (off, --no-prior, below prior.min_judged or no source).
	Prior *float64
}

// OpenRecord is one read of an entry (kb show) to attach to a recent search: the entry at
// EntryPath of Store ("" = the logging store itself, else a canonical ref as in LoggedHit), read
// at At. It attaches to the newest search at most Window old that returned that entry.
type OpenRecord struct {
	Store     string
	EntryPath string
	At        time.Time
	Window    time.Duration
}

// LastSearch is the newest logged search for one caller (kb feedback --last).
type LastSearch struct {
	ID        int64
	Query     string
	TS        string // ISO-8601 UTC
	NReturned int
}

// Table is one analytic query's output. Values are int64, float64, string or nil.
type Table struct {
	Columns []string
	Rows    [][]any
}
