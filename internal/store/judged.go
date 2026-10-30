package store

import (
	"context"
	"time"
)

// JudgedLog reads the search log back for evaluation (kb eval). It is separate from SearchLog so a
// backend can add it without widening the interface every command takes.
type JudgedLog interface {
	// Judged returns every search matching f, oldest first by id, with its ranked verdicts, a
	// whole-search --none and the entries opened from it. Searches without any of these are
	// returned too, so the caller can count them.
	Judged(ctx context.Context, f JudgedFilter) ([]JudgedSearch, error)
}

// JudgedFilter narrows Judged. A zero Since means no lower bound (ts >= Since otherwise); an
// empty Caller means every caller.
type JudgedFilter struct {
	Since  time.Time
	Caller string
}

// JudgedSearch is one logged search and the signals recorded on it. Verdicts carry the store, path
// and heading the search returned at that rank, as logged: never the chunk id, which a reindex
// reuses. A cross-store search (PLAN 3.2) logs other stores' hits too, so a verdict or an open can
// be on another store's entry: Verdict.Store and OtherOpens say so, and gold from them must never
// be checked against this store's index.
type JudgedSearch struct {
	ID       int64
	TS       string // ISO-8601 UTC, as logged
	Query    string
	Mode     string
	K        int
	Caller   string
	Verdicts []Verdict // one per judged rank, by rank; the latest verdict on a rank wins
	None     bool      // a whole-search --none is on record
	Opens    []string  // this store's entry paths read with kb show after the search, sorted
	// OtherOpens are opens of other stores' entries (kb show REF:path), sorted by store, then
	// path. One path can be opened from this store and from another: it is then in both lists.
	OtherOpens []StorePath
}

// StorePath is an entry path of a store: Store is the canonical ref NAME@ABSROOT ("" = the store
// that logged the search), as in LoggedHit.
type StorePath struct {
	Store string
	Path  string
}

// Verdict is the current feedback on one returned hit.
type Verdict struct {
	Rank      int
	Store     string // the canonical ref of the store that returned the hit; "" = this store
	Useful    bool
	EntryPath string
	Heading   string
}
