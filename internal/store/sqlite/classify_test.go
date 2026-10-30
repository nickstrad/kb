package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	sqlite3 "github.com/mattn/go-sqlite3"

	"github.com/nickstrad/kb/internal/errs"
)

// TestWrapClassifies is the decision table for wrap: each known edge case gets its kind and
// friendly message, a well-formed error passes through, and the rest is Internal.
func TestWrapClassifies(t *testing.T) {
	wellFormed := errs.New(errs.NotFound, "x", "no such search")
	for _, tc := range []struct {
		name    string
		err     error
		kind    errs.Kind
		wantMsg string
	}{
		{"ctx cancel", fmt.Errorf("q: %w", context.Canceled), errs.Interrupted, "interrupted"},
		{"statement interrupted", sqlite3.Error{Code: sqlite3.ErrInterrupt}, errs.Interrupted, "interrupted"},
		{"corrupt", sqlite3.Error{Code: sqlite3.ErrCorrupt}, errs.Conflict, "damaged"},
		{"not a database", sqlite3.Error{Code: sqlite3.ErrNotADB}, errs.Conflict, "damaged"},
		{"busy", sqlite3.Error{Code: sqlite3.ErrBusy}, errs.Conflict, "stayed locked"},
		{"locked", sqlite3.Error{Code: sqlite3.ErrLocked}, errs.Conflict, "stayed locked"},
		{"cannot open", sqlite3.Error{Code: sqlite3.ErrCantOpen}, errs.Usage, "permissions"},
		{"permission", sqlite3.Error{Code: sqlite3.ErrPerm}, errs.Usage, "permissions"},
		{"read-only file", sqlite3.Error{Code: sqlite3.ErrReadonly}, errs.Usage, "permissions"},
		{"disk full", sqlite3.Error{Code: sqlite3.ErrFull}, errs.Usage, "disk full or unwritable: /x/kb.sqlite; free space and retry"},
		{"disk I/O error", sqlite3.Error{Code: sqlite3.ErrIoErr}, errs.Usage, "disk full or unwritable: /x/kb.sqlite; free space and retry"},
		{"disk I/O error (extended write)", sqlite3.Error{Code: sqlite3.ErrIoErr, ExtendedCode: sqlite3.ErrIoErrWrite}, errs.Usage, "free space and retry"},
		{"out of memory stays internal", sqlite3.Error{Code: sqlite3.ErrNomem}, errs.Internal, "could not do it"},
		{"plain", errors.New("boom"), errs.Internal, "could not do it"},
		{"well-formed passes through", wellFormed, errs.NotFound, "no such search"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := wrap(context.Background(), tc.err, "/x/kb.sqlite", "sqlite.Test", "could not do it")
			var e *errs.Error
			if !errors.As(err, &e) || errs.KindOf(err) != tc.kind || !errors.Is(err, tc.err) {
				t.Fatalf("wrap = %v (kind %v), want kind %v keeping the cause", err, errs.KindOf(err), tc.kind)
			}
			if got := err.Error(); !strings.Contains(got, tc.wantMsg) || e.Ctx["db"] != "/x/kb.sqlite" {
				t.Errorf("message %q / ctx %v, want %q and the db path", got, e.Ctx, tc.wantMsg)
			}
		})
	}
}

// TestWrapAfterCancelIsInterrupted: once ctx is cancelled, whatever the driver returned (here
// sql.ErrTxDone from a transaction database/sql already rolled back) is Interrupted.
func TestWrapAfterCancelIsInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := wrap(ctx, sql.ErrTxDone, "/x", "op", "commit failed"); errs.KindOf(err) != errs.Interrupted || !errors.Is(err, sql.ErrTxDone) {
		t.Errorf("wrap after cancel = %v (kind %v), want Interrupted", err, errs.KindOf(err))
	}
	if err := wrap(context.TODO(), sql.ErrTxDone, "/x", "op", "close failed"); err == nil || errs.KindOf(err) != errs.Internal {
		t.Errorf("wrap with nil ctx = kind %v, want Internal", errs.KindOf(err))
	}
}
