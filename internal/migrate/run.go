package migrate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/nickstrad/kb/internal/errs"
)

// DefaultLockWait is how long Run waits for another migrator when Options.LockWait is 0.
const DefaultLockWait = 10 * time.Second

// KeepBackups is how many backups per artefact Run keeps; older ones are deleted after a backup.
const KeepBackups = 3

// LockName is the advisory lock file Run creates beside each artefact. It is never deleted:
// removing a lock file while another process waits on it would let two runners in.
const LockName = "migrate.lock"

// Options configure Run.
type Options struct {
	DryRun   bool          // report the plan (pending steps, backup paths) and change nothing
	NoBackup bool          // skip the backup
	LockWait time.Duration // how long to wait for another migrator: 0 = DefaultLockWait, < 0 = do not wait
	Now      func() time.Time

	onWait func() // test seam: called once when the lock is busy, before waiting
}

// Outcome is what Run did to one artefact.
type Outcome struct {
	Status   Status   // as read under the lock (before Apply)
	Applied  []Info   // steps applied; in a dry run, nil (Status.Pending is the plan)
	Backup   string   // the backup written (or, in a dry run, that would be written); "" for none
	Warnings []string // non-fatal problems, e.g. an old backup that could not be pruned
}

// Report is what Run did, one Outcome per artefact it reached, in order.
type Report struct {
	DryRun    bool
	Artefacts []Outcome
}

// Needs is the combined effect the user must act on: of the applied steps, or in a dry run of
// the pending ones.
func (r Report) Needs() Effect {
	e := EffectNone
	for _, o := range r.Artefacts {
		infos := o.Applied
		if r.DryRun {
			infos = o.Status.Pending
		}
		e = max(e, Combined(infos))
	}
	return e
}

// Run brings each artefact to its target, in order: read status; for one with pending steps take
// the lock, re-read the status (another runner may have finished), back up, apply and verify.
// It stops at the first error and returns the report so far with it. A newer artefact is a
// Conflict and is never touched; a version-0 (fresh) artefact is created without a backup.
func Run(ctx context.Context, arts []Artefact, o Options) (Report, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	switch {
	case o.LockWait == 0:
		o.LockWait = DefaultLockWait
	case o.LockWait < 0:
		o.LockWait = 0
	}
	rep := Report{DryRun: o.DryRun}
	for _, a := range arts {
		out, err := runOne(ctx, a, o)
		if out.Status.Path != "" {
			rep.Artefacts = append(rep.Artefacts, out)
		}
		if err != nil {
			return rep, err
		}
	}
	return rep, nil
}

func runOne(ctx context.Context, a Artefact, o Options) (Outcome, error) {
	const op = "migrate.Run"
	st, err := status(ctx, a)
	if err != nil || len(st.Pending) == 0 {
		return Outcome{Status: st}, err
	}
	if o.DryRun {
		out := Outcome{Status: st}
		if !o.NoBackup && st.Current > 0 {
			out.Backup = backupPath(st.Path, st.Current, o.Now())
		}
		return out, nil
	}

	unlock, err := lock(ctx, filepath.Join(filepath.Dir(st.Path), LockName), o.LockWait, o.onWait)
	if err != nil {
		return Outcome{Status: st}, err
	}
	defer unlock()

	// Re-read under the lock: the runner we waited for may have done the work.
	if st, err = status(ctx, a); err != nil || len(st.Pending) == 0 {
		return Outcome{Status: st}, err
	}
	out := Outcome{Status: st}
	if !o.NoBackup && st.Current > 0 {
		dst := backupPath(st.Path, st.Current, o.Now())
		if err := a.Backup(ctx, dst); err != nil {
			return out, wrap(ctx, err, op, "could not back up %s to %s; nothing was migrated", st.Path, dst).With("path", st.Path)
		}
		out.Backup = dst
		out.Warnings = prune(st.Path, KeepBackups)
	}
	applied, err := a.Apply(ctx)
	out.Applied = applied
	if err != nil {
		return out, wrap(ctx, err, op, "could not migrate %s from v%d to v%d%s", st.Path, st.Current, st.Target, backupNote(out.Backup)).
			With("path", st.Path)
	}
	if v, ok := a.(Verifier); ok {
		if err := v.Verify(ctx); err != nil {
			return out, wrap(ctx, err, op, "migrated %s to v%d but it failed verification%s", st.Path, st.Target, backupNote(out.Backup)).
				With("path", st.Path)
		}
	}
	return out, nil
}

// status reads and validates an artefact's status: a newer artefact is a Conflict.
func status(ctx context.Context, a Artefact) (Status, error) {
	st, err := a.Status(ctx)
	if err != nil {
		return st, wrap(ctx, err, "migrate.Run", "could not read the migration status")
	}
	if st.Newer() {
		return st, errs.New(errs.Conflict, "migrate.Run",
			"%s was written by a newer kb (v%d > v%d); upgrade kb", st.Path, st.Current, st.Target).With("path", st.Path)
	}
	return st, nil
}

func backupNote(path string) string {
	if path == "" {
		return ""
	}
	return "; the backup " + path + " holds the previous version"
}

// wrap keeps an artefact's error kind (an *errs.Error passes its kind through), and turns any
// failure after a cancel into Interrupted.
func wrap(ctx context.Context, err error, op, format string, args ...any) *errs.Error {
	k := errs.KindOf(err)
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		k = errs.Interrupted
	}
	return errs.Wrap(err, k, op, format, args...)
}

// backupTimeLayout is the timestamp in a backup name: YYYYMMDD-HHMMSS, local time.
const backupTimeLayout = "20060102-150405"

// backupPath is "<file>.bak-v<N>-<YYYYMMDD-HHMMSS>". When that exists (two runs in one second),
// a "-2", "-3", … suffix keeps it unique, since a backup never overwrites.
func backupPath(file string, version int, now time.Time) string {
	base := fmt.Sprintf("%s.bak-v%d-%s", file, version, now.Format(backupTimeLayout))
	p := base
	for n := 2; exists(p); n++ {
		p = base + "-" + strconv.Itoa(n)
	}
	return p
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// prune deletes all but the newest keep backups of file, ordered by the timestamp and suffix in
// their names. Files that only look similar are left alone. It returns warnings, not errors: a
// stale backup is not a reason to fail a migration that succeeded.
func prune(file string, keep int) []string {
	dir := filepath.Dir(file)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return []string{fmt.Sprintf("could not list old backups in %s: %v", dir, err)}
	}
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(filepath.Base(file)) + `\.bak-v[0-9]+-([0-9]{8}-[0-9]{6})(?:-([0-9]+))?$`)
	type bak struct {
		name, ts string
		n        int
	}
	var baks []bak
	for _, e := range ents {
		m := re.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			continue
		}
		n := 1
		if m[2] != "" {
			n, _ = strconv.Atoi(m[2]) // the regexp guarantees digits
		}
		baks = append(baks, bak{e.Name(), m[1], n})
	}
	slices.SortFunc(baks, func(a, b bak) int { // newest first
		if a.ts != b.ts {
			if a.ts > b.ts {
				return -1
			}
			return 1
		}
		return b.n - a.n
	})
	var warns []string
	for _, b := range baks[min(keep, len(baks)):] {
		if err := os.Remove(filepath.Join(dir, b.name)); err != nil {
			warns = append(warns, fmt.Sprintf("could not delete the old backup %s: %v", b.name, err))
		}
	}
	return warns
}
