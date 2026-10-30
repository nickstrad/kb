package store

import "context"

// FeedbackLog reads the raw verdict rows back, one per (search, rank) as stored. Judged collapses
// them into per-search verdicts; this keeps the note and the stamp, so a test can check exactly
// what a command recorded without reading the table (Q79).
type FeedbackLog interface {
	// FeedbackRows returns every verdict row, by search id then rank.
	FeedbackRows(ctx context.Context) ([]FeedbackRow, error)
}

// FeedbackRow is one stored verdict. Rank 0 is a whole-search --none.
type FeedbackRow struct {
	SearchID int64
	Rank     int
	Useful   bool
	Note     string // "" when none was given
	TS       string // ISO-8601 UTC, as stored
}
