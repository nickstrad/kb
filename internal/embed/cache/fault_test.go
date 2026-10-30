package cache_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/nickstrad/kb/internal/embed/cache"
	"github.com/nickstrad/kb/internal/sqlfault"
)

// faults is the fault driver the cache opens through (cache.Options.Driver).
var faults = sqlfault.Register("sqlite3_cache_fault")

// TestOpenStatementFaults fails each statement kind Open runs, in turn: the open must return the
// injected error (with the cache path in the message) and leave no half-open handle behind.
func TestOpenStatementFaults(t *testing.T) {
	for _, match := range []string{"journal_mode", "BEGIN", "user_version", "CREATE TABLE", "COMMIT"} {
		t.Run(match, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "c.sqlite")
			faults.FailAlways(t, match) // enableWAL retries, so fail every attempt
			c, err := cache.OpenWith(context.Background(), path, cache.Options{Driver: faults.Name()})
			if err == nil {
				c.Close()
				t.Fatalf("Open succeeded with %q failing", match)
			}
			if !errors.Is(err, sqlfault.ErrFault) {
				t.Fatalf("Open error = %v, want the injected fault in the chain", err)
			}
			// The fault gone, the same path opens: a failed Open left nothing that blocks it.
			faults.Disarm()
			c, err = cache.OpenWith(context.Background(), path, cache.Options{Driver: faults.Name()})
			if err != nil {
				t.Fatalf("reopen after %q fault: %v", match, err)
			}
			c.Close()
		})
	}
}

// TestPutGetStatementFaults: a failing write or read is an error from Put/Get, never a panic or
// a partial row, and the cache answers again once the fault is gone.
func TestPutGetStatementFaults(t *testing.T) {
	ctx := context.Background()
	c, err := cache.OpenWith(ctx, filepath.Join(t.TempDir(), "c.sqlite"), cache.Options{Driver: faults.Name()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	faults.Fail(t, "INSERT", 1)
	if err := c.Put(ctx, "m", 4, []string{"x"}, [][]float32{{1, 0, 0, 0}}); !errors.Is(err, sqlfault.ErrFault) {
		t.Fatalf("Put error = %v, want the injected fault", err)
	}
	faults.Disarm()
	got, err := c.Get(ctx, "m", 4, []string{"x"})
	if err != nil || len(got) != 1 || got[0] != nil {
		t.Fatalf("Get after a failed Put = %v, %v; want one miss (nothing half-written)", got, err)
	}
	faults.Fail(t, "SELECT", 1)
	if _, err := c.Get(ctx, "m", 4, []string{"x"}); !errors.Is(err, sqlfault.ErrFault) {
		t.Fatalf("Get error = %v, want the injected fault", err)
	}
}
