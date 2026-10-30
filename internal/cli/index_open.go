package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
)

// encodeJSON writes v to the command's stdout encoder; a failed write (a closed pipe) is Internal,
// the one convention for stdout write errors (CODING_STYLE §1.5).
func encodeJSON(enc *json.Encoder, v any, op string) error {
	if err := enc.Encode(v); err != nil {
		return errs.Wrap(err, errs.Internal, op, "write JSON to stdout: %v", err)
	}
	return nil
}

// openIndexReadOnly stats the index file and opens it read-only: NotFound when there is no index,
// Usage when it cannot be read. kb eval and kb synonyms suggest share it. op names the caller in
// the error.
func (d *deps) openIndexReadOnly(ctx context.Context, dbPath, op string) (store.Store, error) {
	return d.openIndex(ctx, dbPath, op, true)
}

// openIndex is openIndexReadOnly with the mode chosen: kb search opens read-write when it logs.
func (d *deps) openIndex(ctx context.Context, dbPath, op string, readOnly bool) (store.Store, error) {
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errs.Wrap(err, errs.NotFound, op, "no index at %s; run kb reindex --all", dbPath)
		}
		// Driver-only: an unreadable database file whose directory (and config.toml) is still
		// readable; a test cannot take one without the other (Q86).
		return nil, errs.Wrap(err, errs.Usage, op, "cannot read the index %s: check the file and its permissions", dbPath).With("path", dbPath)
	}
	return d.openStore(ctx, dbPath, readOnly)
}
