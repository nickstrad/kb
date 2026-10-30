package migrate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nickstrad/kb/internal/errs"
)

// lockPoll is how often a waiting runner retries the lock.
const lockPoll = 20 * time.Millisecond

// lock takes an exclusive advisory flock on path, creating the file (and its directory) if
// needed. A busy lock is retried every lockPoll until wait has passed (then Conflict) or ctx is
// cancelled (Interrupted); onWait, if set, runs once at the first busy attempt. flock locks
// belong to the open file description, so two opens in one process exclude each other too.
// The returned func releases the lock and closes the file.
func lock(ctx context.Context, path string, wait time.Duration, onWait func()) (func(), error) {
	const op = "migrate.Run"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, errs.Wrap(err, errs.Usage, op, "could not create the directory for %s", path).With("path", path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, errs.Wrap(err, errs.Usage, op, "could not open the migration lock %s: check its permissions", path).With("path", path)
	}
	deadline := time.Now().Add(wait)
	for waited := false; ; {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return func() {
				// Closing the file releases the flock too; the explicit unlock makes the order plain.
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) // nothing useful to do if it fails
				_ = f.Close()
			}, nil
		case errors.Is(err, syscall.EINTR):
			continue
		case !errors.Is(err, syscall.EWOULDBLOCK):
			f.Close()
			return nil, errs.Wrap(err, errs.Internal, op, "could not lock %s", path).With("path", path)
		case !time.Now().Before(deadline):
			f.Close()
			return nil, errs.New(errs.Conflict, op, "another kb is migrating %s; try again when it finishes", filepath.Dir(path)).
				With("lock", path)
		}
		if !waited && onWait != nil {
			onWait()
		}
		waited = true
		select {
		case <-ctx.Done():
			f.Close()
			return nil, errs.Wrap(ctx.Err(), errs.Interrupted, op, "interrupted while waiting for %s", path).With("lock", path)
		case <-time.After(min(lockPoll, max(time.Until(deadline), time.Millisecond))):
		}
	}
}
