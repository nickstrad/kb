package search

import (
	"crypto/sha256"
	"encoding/hex"
	"math"

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/prior"
	"github.com/nickstrad/kb/internal/store"
)

// Params are the retrieval knobs one search runs with, taken from [search] in the config. Every
// field is set; DefaultParams holds the shipped values (the package constants).
type Params struct {
	ListLimit      int               `json:"list_limit"`      // rows taken from each ranked list
	CandidateLimit int               `json:"candidate_limit"` // length cap of the fused list
	PerEntryCap    int               `json:"per_entry_cap"`   // returned chunks per entry
	RRFK           int               `json:"rrf_k"`           // the k of 1/(k+rank)
	VecOverFetch   int               `json:"vec_overfetch"`   // KNN rows fetched before a tag filter
	BM25           store.BM25Weights `json:"bm25"`            // FTS column weights
	FTSWeight      float64           `json:"fts_weight"`      // multiplier of the FTS list's RRF term
	VecWeight      float64           `json:"vec_weight"`      // multiplier of the vector list's RRF term
	ExactWeight    float64           `json:"exact_weight"`    // multiplier of the exact-identifier list's RRF term
	MemoryWeight   float64           `json:"memory_weight"`   // multiplier of the query-memory list's RRF term (PLAN 6.3)
	ExactList      bool              `json:"exact_list"`      // run the exact-identifier list for a query with an identifier
	Spell          bool              `json:"spell"`           // correct unknown words when the FTS list is short (PLAN 5.6)
	PhraseBoost    bool              `json:"phrase_boost"`    // phrase and NEAR tiers ahead of AND for a query of two or more groups (PLAN 4.6)
	// SupersedePenalty multiplies the RRF score of a chunk whose entry another entry supersedes
	// (PLAN 5.3); 1 leaves the order alone.
	SupersedePenalty float64 `json:"supersede_penalty"`
	// Prior is [prior] (PLAN 6.2): the feedback prior that multiplies the fused score once enough
	// searches are judged. It is not marshalled here: the settings JSON and hash come from
	// config.EffectiveJSON, which carries [prior] itself.
	Prior prior.Params `json:"-"`
}

// ParamsFrom maps [search] onto Params. The config must already be validated.
func ParamsFrom(s config.Search) Params {
	return Params{
		ListLimit:      s.ListLimit,
		CandidateLimit: s.CandidateLimit,
		PerEntryCap:    s.PerEntryCap,
		RRFK:           s.RRFK,
		VecOverFetch:   s.VecOverfetch,
		BM25:           store.BM25Weights{Text: s.BM25.Text, Heading: s.BM25.Heading, Title: s.BM25.Title},
		FTSWeight:      s.ListWeights.FTS,
		VecWeight:      s.ListWeights.Vec,
		ExactWeight:    s.ListWeights.Exact,
		MemoryWeight:   s.ListWeights.Memory,
		ExactList:      s.ExactList,
		Spell:          s.Spell,
		PhraseBoost:    s.PhraseBoost,

		SupersedePenalty: s.SupersedePenalty,
	}
}

// priorFrom maps [prior] onto prior.Params. The config must already be validated.
func priorFrom(c config.Prior) prior.Params {
	return prior.Params{Enabled: c.Enabled, MinJudged: c.MinJudged, HalfLifeDays: float64(c.HalfLifeDays),
		MaxBoost: c.MaxBoost, QueryMemory: c.QueryMemory}
}

// DefaultParams are the parameters of the default config.
func DefaultParams() Params {
	c := config.Default()
	p := ParamsFrom(c.Search)
	p.Prior = priorFrom(c.Prior)
	return p
}

// valid is what config.Validate guarantees: positive limits and finite, non-negative weights.
// Search asserts it, because an invalid Params can only come from a caller that skipped
// validation.
func (p Params) valid() bool {
	w := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }
	return p.ListLimit >= 1 && p.CandidateLimit >= 1 && p.PerEntryCap >= 1 && p.RRFK >= 1 && p.VecOverFetch >= 1 &&
		w(p.BM25.Text) && w(p.BM25.Heading) && w(p.BM25.Title) && w(p.FTSWeight) && w(p.VecWeight) && w(p.ExactWeight) && w(p.MemoryWeight) &&
		w(p.SupersedePenalty) && p.SupersedePenalty <= 1 && (!p.Prior.Enabled || p.Prior.Validate() == nil)
}

// Settings is what a search ran with, for its trace: the Params it used, and the canonical
// effective-settings JSON of the config they came from with its hash (config.EffectiveJSON). The
// hash identifies a configuration across searches; equal configs give equal hashes.
type Settings struct {
	Params Params
	JSON   []byte
	Hash   string
}

// NewSettings derives the Settings of a validated config. Its only error is an Internal one from
// encoding the settings.
func NewSettings(c config.Config) (Settings, error) {
	data, hash, err := c.EffectiveJSON()
	if err != nil {
		return Settings{}, errs.Wrap(err, errs.KindOf(err), "search.NewSettings", "%v", err)
	}
	p := ParamsFrom(c.Search)
	p.Prior = priorFrom(c.Prior)
	return Settings{Params: p, JSON: data, Hash: hash}, nil
}

// WithSynonyms is s for a search that expands its query with synonyms whose content hash is h: the
// settings hash then also identifies the synonyms (an eval baseline run with other synonyms is not
// comparable). No synonyms (h "") leave s, hash included, unchanged. s must come from NewSettings.
func (s Settings) WithSynonyms(h string) Settings {
	if h == "" {
		return s
	}
	assert.Pre(s.Hash != "", "WithSynonyms: settings have no hash")
	sum := sha256.Sum256([]byte(s.Hash + "\x00synonyms:" + h))
	s.Hash = hex.EncodeToString(sum[:])
	return s
}

// DefaultSettings are the Settings of the default config.
func DefaultSettings() (Settings, error) { return NewSettings(config.Default()) }

// settings is the Settings a search uses: s.Settings, or the defaults when it was left zero.
func (s *Searcher) settings() (Settings, error) {
	if s.Settings.Hash == "" {
		return DefaultSettings()
	}
	assert.Pre(s.Settings.Params.valid(), "Search: invalid params %+v", s.Settings.Params)
	return s.Settings, nil
}
