package config

// Bounds and defaults. MinK and MaxK mirror search.MinK and search.MaxK; a test in the cli
// package pins them to each other, so config does not import a service package.
const (
	MinK = 1
	MaxK = 20

	// SchemaVersion is the config schema version this kb reads.
	SchemaVersion = 1

	// ModelNone is the embed.model value for an FTS-only store.
	ModelNone = "none"

	// DefaultModel is the embedding model of a new store; it equals local.DefaultModel (a cli
	// test pins that).
	DefaultModel = "BAAI/bge-small-en-v1.5"

	// DefaultOpenWindow is log.open_window. It is 30m, as shipped, not the 2h of the first
	// design.
	DefaultOpenWindow = "30m"
)

// Layouts a [[sources]] table may name.
const (
	LayoutEntries = "entries"
	LayoutFiles   = "files"
)

// Expand modes for output.expand.
const (
	ExpandNone    = "none"
	ExpandSection = "section"
	ExpandEntry   = "entry"
)

// Default returns the configuration of a store with no config file. The slices and the map are
// non-nil and empty, so a Default round-trips through TOML unchanged.
func Default() Config {
	return Config{
		Version: SchemaVersion,
		Embed: Embed{
			Model: DefaultModel,
			Batch: 16,
		},
		Sources: []Source{},
		Search: Search{
			K:                8,
			ListLimit:        20,
			CandidateLimit:   40,
			PerEntryCap:      3,
			RRFK:             60,
			VecOverfetch:     60,
			BM25:             BM25{Text: 1.0, Heading: 2.0, Title: 3.0},
			ListWeights:      ListWeights{FTS: 1.0, Vec: 1.0, Exact: 1.0, Memory: 1.0},
			PhraseBoost:      true,
			ExactList:        true,
			Spell:            true,
			SupersedePenalty: 0.5,
			Stores:           []string{},
		},
		Output: Output{Budget: 0, ContextBudget: 2000, Expand: ExpandNone},
		Log:    Log{OpenWindow: DefaultOpenWindow},
		Stale:  Stale{Days: 180},
		Eval:   Eval{MinJudged: 30, Holdout: 0.3},
		Prior:  Prior{MinJudged: 50, HalfLifeDays: 90, MaxBoost: 0.2},
	}
}
