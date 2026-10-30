package migrate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickstrad/kb/internal/errs"
)

// fakeTarget is the version the fake artefact's steps reach.
const fakeTarget = 3

// fakeInfos are the fake's steps: v1→v2 needs a reindex, v2→v3 a re-embed.
var fakeInfos = []Info{
	{From: 0, ID: "fake/0001-create", Desc: "create", Effect: EffectNone},
	{From: 1, ID: "fake/0002-edges", Desc: "edges", Effect: EffectReindex},
	{From: 2, ID: "fake/0003-vectors", Desc: "vectors", Effect: EffectReembed},
}

// fake is a versioned file whose content is its version number; a missing file is version 0.
// Apply writes fakeTarget unless it fails, so "version unchanged" is visible on disk.
type fake struct {
	path      string
	statusErr error
	backupErr error
	// backupPartial makes Backup write a few bytes to dst and then fail, as an interrupted copy does.
	backupPartial bool
	applyErr      error
	applyHook     func(ctx context.Context) error // runs at the start of Apply; an error aborts it
	applies       atomic.Int32
}

func newFake(t *testing.T, version int) *fake {
	t.Helper()
	f := &fake{path: filepath.Join(t.TempDir(), "art.db")}
	if version > 0 {
		writeVersion(t, f.path, version)
	}
	return f
}

func writeVersion(t *testing.T, path string, v int) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strconv.Itoa(v)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *fake) version() (int, error) {
	b, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(b))
}

func (f *fake) Status(context.Context) (Status, error) {
	st := Status{Artefact: "fake", Path: f.path, Target: fakeTarget, Pending: fakeInfos}
	if f.statusErr != nil {
		return st, f.statusErr // with Pending set: Run must still stop
	}
	st.Pending = nil
	v, err := f.version()
	if err != nil {
		return st, err
	}
	st.Current = v
	if v < fakeTarget {
		st.Pending = fakeInfos[v:]
	}
	return st, nil
}

func (f *fake) Backup(_ context.Context, dst string) error {
	if f.backupPartial {
		if err := os.WriteFile(dst, []byte("half"), 0o644); err != nil {
			return err
		}
		return errors.New("copy interrupted")
	}
	if f.backupErr != nil {
		return f.backupErr
	}
	if exists(dst) {
		return fmt.Errorf("backup %s exists", dst)
	}
	b, err := os.ReadFile(f.path)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

func (f *fake) Apply(ctx context.Context) ([]Info, error) {
	if f.applyHook != nil {
		if err := f.applyHook(ctx); err != nil {
			return nil, err
		}
	}
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	v, err := f.version()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(f.path, []byte(strconv.Itoa(fakeTarget)), 0o644); err != nil {
		return nil, err
	}
	f.applies.Add(1)
	return fakeInfos[v:], nil
}

// verifyingFake adds a Verifier to fake.
type verifyingFake struct {
	*fake
	verifyErr error
	verified  int
}

func (v *verifyingFake) Verify(context.Context) error {
	v.verified++
	return v.verifyErr
}

// backups lists the backup files beside path.
func backups(t *testing.T, path string) []string {
	t.Helper()
	m, err := filepath.Glob(path + ".bak-v*")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

var fixedNow = func() time.Time { return time.Date(2026, 10, 30, 16, 12, 5, 0, time.Local) }

// TestRunDecisions is the Run decision table: artefact state × options × lock × failures. Each
// row asserts the outcome kind, the version on disk, the backups on disk and what was applied.
func TestRunDecisions(t *testing.T) {
	errBackup := errs.New(errs.Usage, "fake.Backup", "disk full")
	errApply := errors.New("apply exploded")
	errVerify := errs.New(errs.Conflict, "fake.Verify", "schema drift")
	type lockState int
	const (
		lockFree lockState = iota
		lockHeldThenReleased
		lockHeldPastWait
		lockHeldThenCancel
	)
	for _, tc := range []struct {
		name        string
		start       int // 0 = no file
		opts        Options
		lock        lockState
		statusErr   error
		backupErr   error
		applyErr    error
		cancelIn    bool // the apply hook cancels ctx and returns its error (Ctrl-C mid-apply)
		cancelPlain bool // the apply hook cancels ctx and returns an unrelated error (SQLITE_INTERRUPT)
		verify      bool
		verifyErr   error
		wantKind    errs.Kind // -1 = no error
		wantVer     int
		wantBackup  bool   // a backup file exists afterwards
		wantPlan    bool   // Outcome.Backup names a path although no file was written (dry run)
		wantApply   int    // applied steps in the outcome
		wantNeeds   Effect // Report.Needs
	}{
		{name: "current: nothing pending", start: 3, wantKind: -1, wantVer: 3},
		{name: "current with verifier: not verified", start: 3, verify: true, wantKind: -1, wantVer: 3},
		{name: "fresh: created, no backup", start: 0, wantKind: -1, wantVer: 3, wantApply: 3, wantNeeds: EffectReembed},
		{name: "pending: backup then apply", start: 1, wantKind: -1, wantVer: 3, wantBackup: true, wantApply: 2, wantNeeds: EffectReembed},
		{name: "pending one step (Target-1)", start: 2, wantKind: -1, wantVer: 3, wantBackup: true, wantApply: 1, wantNeeds: EffectReembed},
		{name: "pending + verify ok", start: 1, verify: true, wantKind: -1, wantVer: 3, wantBackup: true, wantApply: 2, wantNeeds: EffectReembed},
		{name: "pending NoBackup", start: 1, opts: Options{NoBackup: true}, wantKind: -1, wantVer: 3, wantApply: 2, wantNeeds: EffectReembed},
		{name: "pending DryRun", start: 1, opts: Options{DryRun: true}, wantKind: -1, wantVer: 1, wantPlan: true, wantNeeds: EffectReembed},
		{name: "pending DryRun NoBackup", start: 1, opts: Options{DryRun: true, NoBackup: true}, wantKind: -1, wantVer: 1, wantNeeds: EffectReembed},
		{name: "fresh DryRun: no backup planned", start: 0, opts: Options{DryRun: true}, wantKind: -1, wantVer: 0, wantNeeds: EffectReembed},
		{name: "DryRun ignores a held lock", start: 1, opts: Options{DryRun: true}, lock: lockHeldPastWait, wantKind: -1, wantVer: 1, wantPlan: true, wantNeeds: EffectReembed},
		{name: "newer: refused, untouched", start: 4, wantKind: errs.Conflict, wantVer: 4},
		{name: "newer DryRun: refused", start: 4, opts: Options{DryRun: true}, wantKind: errs.Conflict, wantVer: 4},
		{name: "newer NoBackup: refused", start: 4, opts: Options{NoBackup: true}, wantKind: errs.Conflict, wantVer: 4},
		{name: "status fails: kind kept", start: 1, statusErr: errs.New(errs.Usage, "fake", "unreadable"), wantKind: errs.Usage, wantVer: 1},
		{name: "backup fails: nothing applied", start: 1, backupErr: errBackup, wantKind: errs.Usage, wantVer: 1},
		{name: "backup fails NoBackup: applied", start: 1, opts: Options{NoBackup: true}, backupErr: errBackup, wantKind: -1, wantVer: 3, wantApply: 2, wantNeeds: EffectReembed},
		{name: "apply fails: version unchanged, backup kept", start: 1, applyErr: errApply, wantKind: errs.Internal, wantVer: 1, wantBackup: true},
		{name: "apply fails NoBackup", start: 1, opts: Options{NoBackup: true}, applyErr: errApply, wantKind: errs.Internal, wantVer: 1},
		{name: "Ctrl-C mid-apply: Interrupted, rolled back, backup kept", start: 1, cancelIn: true, wantKind: errs.Interrupted, wantVer: 1, wantBackup: true},
		{name: "Ctrl-C mid-apply surfacing as a driver error: Interrupted", start: 1, cancelPlain: true, wantKind: errs.Interrupted, wantVer: 1, wantBackup: true},
		{name: "verify fails: applied, backup kept", start: 1, verify: true, verifyErr: errVerify, wantKind: errs.Conflict, wantVer: 3, wantBackup: true, wantApply: 2, wantNeeds: EffectReembed},
		{name: "lock held then released: applies", start: 1, lock: lockHeldThenReleased, wantKind: -1, wantVer: 3, wantBackup: true, wantApply: 2, wantNeeds: EffectReembed},
		{name: "lock held past LockWait: Conflict", start: 1, lock: lockHeldPastWait, wantKind: errs.Conflict, wantVer: 1},
		{name: "lock held, Ctrl-C while waiting", start: 1, lock: lockHeldThenCancel, wantKind: errs.Interrupted, wantVer: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := newFake(t, tc.start)
			f.statusErr, f.backupErr, f.applyErr = tc.statusErr, tc.backupErr, tc.applyErr
			if tc.cancelIn {
				f.applyHook = func(ctx context.Context) error {
					cancel()
					<-ctx.Done()
					return fmt.Errorf("apply: %w", ctx.Err())
				}
			}
			if tc.cancelPlain {
				f.applyHook = func(context.Context) error {
					cancel()
					return errors.New("interrupted")
				}
			}
			var art Artefact = f
			vf := &verifyingFake{fake: f, verifyErr: tc.verifyErr}
			if tc.verify {
				art = vf
			}
			o := tc.opts
			o.Now = fixedNow
			if tc.lock != lockFree {
				unlock, err := lock(ctx, filepath.Join(filepath.Dir(f.path), LockName), 0, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
				switch tc.lock {
				case lockHeldThenReleased:
					o.LockWait = time.Minute
					o.onWait = unlock
				case lockHeldPastWait:
					o.LockWait = -1
				case lockHeldThenCancel:
					o.LockWait = time.Minute
					o.onWait = cancel
				}
			}

			rep, err := Run(ctx, []Artefact{art}, o)

			if tc.wantKind < 0 && err != nil {
				t.Fatalf("Run: %v", err)
			}
			if tc.wantKind >= 0 && (err == nil || errs.KindOf(err) != tc.wantKind) {
				t.Fatalf("Run err = %v (kind %v), want kind %v", err, errs.KindOf(err), tc.wantKind)
			}
			if v, _ := f.version(); v != tc.wantVer {
				t.Errorf("version on disk = %d, want %d", v, tc.wantVer)
			}
			baks := backups(t, f.path)
			if (len(baks) == 1) != tc.wantBackup || len(baks) > 1 {
				t.Errorf("backups on disk = %v, want present=%v", baks, tc.wantBackup)
			}
			if tc.wantBackup {
				if b, _ := os.ReadFile(baks[0]); string(b) != strconv.Itoa(tc.start) {
					t.Errorf("backup holds %q, want the starting version %d", b, tc.start)
				}
			}
			if rep.DryRun != tc.opts.DryRun {
				t.Errorf("Report.DryRun = %v", rep.DryRun)
			}
			if got := rep.Needs(); got != tc.wantNeeds {
				t.Errorf("Needs = %v, want %v", got, tc.wantNeeds)
			}
			if tc.statusErr != nil {
				return
			}
			if len(rep.Artefacts) != 1 {
				t.Fatalf("report has %d outcomes, want 1", len(rep.Artefacts))
			}
			out := rep.Artefacts[0]
			if len(out.Applied) != tc.wantApply {
				t.Errorf("Applied = %v, want %d steps", out.Applied, tc.wantApply)
			}
			switch {
			case tc.wantBackup && out.Backup != baks[0]:
				t.Errorf("Outcome.Backup = %q, want %q", out.Backup, baks[0])
			case tc.wantPlan && out.Backup != f.path+".bak-v1-20261030-161205":
				t.Errorf("planned backup = %q", out.Backup)
			case !tc.wantBackup && !tc.wantPlan && out.Backup != "":
				t.Errorf("Outcome.Backup = %q, want none", out.Backup)
			}
			if tc.verify && tc.wantApply > 0 && vf.verified != 1 {
				t.Errorf("verified %d times, want 1", vf.verified)
			}
			if tc.verify && tc.wantApply == 0 && vf.verified != 0 {
				t.Errorf("verified %d times with nothing applied", vf.verified)
			}
			if err != nil && (tc.applyErr != nil || tc.verifyErr != nil) {
				if note := "the backup " + out.Backup + " holds the previous version"; tc.wantBackup != strings.Contains(err.Error(), note) {
					t.Errorf("message %q: backup note present=%v, want %v", err, !tc.wantBackup, tc.wantBackup)
				}
			}
			if tc.wantKind == errs.Conflict && tc.start > fakeTarget && !strings.Contains(err.Error(), "newer kb (v4 > v3)") {
				t.Errorf("newer message = %q", err)
			}
		})
	}
}

// TestRunStopsAtFirstError: with several artefacts, Run reports what it reached and stops.
func TestRunStopsAtFirstError(t *testing.T) {
	ok, bad, never := newFake(t, 1), newFake(t, 4), newFake(t, 1)
	rep, err := Run(context.Background(), []Artefact{ok, bad, never}, Options{Now: fixedNow, NoBackup: true})
	if errs.KindOf(err) != errs.Conflict {
		t.Fatalf("err = %v, want Conflict for the newer artefact", err)
	}
	if len(rep.Artefacts) != 2 || len(rep.Artefacts[0].Applied) != 2 || len(rep.Artefacts[1].Applied) != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if v, _ := never.version(); v != 1 || never.applies.Load() != 0 {
		t.Errorf("artefact after the failure was touched: v%d, %d applies", v, never.applies.Load())
	}
}

// TestRunStatusErrorWithoutPath: an artefact that cannot say where it is gets no outcome row.
func TestRunStatusErrorWithoutPath(t *testing.T) {
	rep, err := Run(context.Background(), []Artefact{pathless{}}, Options{})
	if errs.KindOf(err) != errs.Internal || len(rep.Artefacts) != 0 {
		t.Fatalf("rep %+v err %v", rep, err)
	}
}

type pathless struct{ Artefact }

func (pathless) Status(context.Context) (Status, error) { return Status{}, errors.New("no path") }

// TestRunConcurrent: two runners on one artefact. The first to lock applies; the other waits,
// re-reads the status under the lock and does nothing. The apply blocks until the loser is
// waiting, so the interleaving is forced without sleeps. Run with -race.
func TestRunConcurrent(t *testing.T) {
	f := newFake(t, 1)
	waiting := make(chan struct{})
	var once sync.Once
	f.applyHook = func(context.Context) error { <-waiting; return nil }
	o := Options{Now: fixedNow, LockWait: time.Minute, onWait: func() { once.Do(func() { close(waiting) }) }}

	var wg sync.WaitGroup
	reps := make([]Report, 2)
	errsOut := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o := o
			o.Now = func() time.Time { return fixedNow().Add(time.Duration(i) * time.Second) }
			reps[i], errsOut[i] = Run(context.Background(), []Artefact{f}, o)
		}()
	}
	wg.Wait()
	for i, err := range errsOut {
		if err != nil {
			t.Fatalf("runner %d: %v", i, err)
		}
	}
	if n := f.applies.Load(); n != 1 {
		t.Fatalf("applied %d times, want 1", n)
	}
	applied := len(reps[0].Artefacts[0].Applied) + len(reps[1].Artefacts[0].Applied)
	if applied != 2 {
		t.Errorf("applied steps across both reports = %d, want 2 (one runner, two steps)", applied)
	}
	if baks := backups(t, f.path); len(baks) != 1 {
		t.Errorf("backups = %v, want exactly one", baks)
	}
}

// TestBackupPath: the name format, and a suffix when a backup of that second exists.
func TestBackupPath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "kb.sqlite")
	want := file + ".bak-v3-20261030-161205"
	if got := backupPath(file, 3, fixedNow()); got != want {
		t.Fatalf("backupPath = %q, want %q", got, want)
	}
	for _, suffix := range []string{"", "-2", "-3"} {
		if got := backupPath(file, 3, fixedNow()); got != want+suffix {
			t.Fatalf("backupPath with %q taken = %q, want %q", suffix, got, want+suffix)
		}
		if err := os.WriteFile(want+suffix, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestPrune: the newest KeepBackups survive, ordered by timestamp then suffix; boundary counts
// (keep-1, keep, keep+1) and look-alike files are covered.
func TestPrune(t *testing.T) {
	names := []string{
		"kb.sqlite.bak-v1-20261001-000000",
		"kb.sqlite.bak-v2-20261002-000000",
		"kb.sqlite.bak-v2-20261002-000000-2",
		"kb.sqlite.bak-v3-20261003-000000",
		"kb.sqlite.bak-v1-20261004-000000", // newest by time even with a lower version
	}
	for _, tc := range []struct {
		n    int // how many of names exist
		want []string
	}{
		{2, names[:2]},
		{3, names[:3]},
		{4, names[1:4]},
		{5, []string{names[2], names[3], names[4]}},
	} {
		t.Run(strconv.Itoa(tc.n), func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "kb.sqlite")
			others := []string{"kb.sqlite", "kb.sqlite.bak-vX-20261001-000000", "other.sqlite.bak-v1-20260101-000000", "kb.sqlite.bak-v1-20261001-000000.tmp"}
			for _, n := range append(slices.Clone(names[:tc.n]), others...) {
				if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(filepath.Join(dir, "kb.sqlite.bak-v9-20991231-000000"), 0o755); err != nil { // a directory: never touched
				t.Fatal(err)
			}
			if w := prune(file, KeepBackups); len(w) != 0 {
				t.Fatalf("warnings: %v", w)
			}
			for _, n := range names[:tc.n] {
				kept := slices.Contains(tc.want, n)
				if exists(filepath.Join(dir, n)) != kept {
					t.Errorf("%s: exists=%v, want %v", n, !kept, kept)
				}
			}
			for _, n := range append(others, "kb.sqlite.bak-v9-20991231-000000") {
				if !exists(filepath.Join(dir, n)) {
					t.Errorf("look-alike %s was deleted", n)
				}
			}
		})
	}
}

// TestPruneWarnings: failures to list or delete become warnings, never errors.
func TestPruneWarnings(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	if w := prune(filepath.Join(t.TempDir(), "missing", "kb.sqlite"), 3); len(w) != 1 || !strings.Contains(w[0], "could not list") {
		t.Errorf("missing dir: %v", w)
	}
	dir := t.TempDir()
	for i := range 4 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("kb.sqlite.bak-v1-2026100%d-000000", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)
	if w := prune(filepath.Join(dir, "kb.sqlite"), 3); len(w) != 1 || !strings.Contains(w[0], "could not delete") {
		t.Errorf("read-only dir: %v", w)
	}
}

// TestRunPrunesAfterBackup: a run that backs up leaves at most KeepBackups backups, and reports
// prune failures as warnings.
func TestRunPrunesAfterBackup(t *testing.T) {
	f := newFake(t, 1)
	for i := range 4 {
		if err := os.WriteFile(fmt.Sprintf("%s.bak-v1-2026090%d-000000", f.path, i), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := Run(context.Background(), []Artefact{f}, Options{Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	baks := backups(t, f.path)
	if len(baks) != KeepBackups || !slices.Contains(baks, rep.Artefacts[0].Backup) {
		t.Errorf("backups = %v, want %d including %s", baks, KeepBackups, rep.Artefacts[0].Backup)
	}
}

// TestLockFailures: a lock path that cannot be created or opened is Usage.
func TestLockFailures(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// The directory that cannot be created is named, so the user knows what to fix (Q56).
	if _, err := lock(ctx, filepath.Join(file, "sub", LockName), 0, nil); errs.KindOf(err) != errs.Usage ||
		!strings.Contains(err.Error(), "directory") || !strings.Contains(err.Error(), filepath.Join(file, "sub")) {
		t.Errorf("lock under a file: %v, want a Usage error naming %s", err, filepath.Join(file, "sub", LockName))
	}
	if err := os.Mkdir(filepath.Join(dir, LockName), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := lock(ctx, filepath.Join(dir, LockName), 0, nil); errs.KindOf(err) != errs.Usage {
		t.Errorf("lock path is a directory: %v", err)
	}
}

// TestRunDefaults: LockWait 0 means DefaultLockWait (the run waits rather than failing at once)
// and a nil Now is time.Now.
func TestRunDefaults(t *testing.T) {
	f := newFake(t, 1)
	unlock, err := lock(context.Background(), filepath.Join(filepath.Dir(f.path), LockName), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	waited := false
	_, err = Run(context.Background(), []Artefact{f}, Options{onWait: func() { waited = true; unlock() }})
	if err != nil || !waited {
		t.Fatalf("Run with LockWait 0: waited=%v err=%v; want it to wait for the lock", waited, err)
	}
	if baks := backups(t, f.path); len(baks) != 1 || !strings.Contains(baks[0], time.Now().Format("20060102-")) {
		t.Errorf("backup with the default clock: %v", baks)
	}
}

// TestLockOnWaitOnce: onWait runs once however many times the busy lock is retried, and a lock
// in a directory that does not exist yet (a fresh artefact) creates it.
func TestLockOnWaitOnce(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "new", "dir", LockName)
	unlock, err := lock(ctx, path, 0, nil)
	if err != nil {
		t.Fatalf("lock in a missing directory: %v", err)
	}
	defer unlock()
	calls := 0
	_, err = lock(ctx, path, 5*lockPoll, func() { calls++ })
	if errs.KindOf(err) != errs.Conflict || calls != 1 {
		t.Fatalf("busy lock: err %v, onWait called %d times; want Conflict and 1", err, calls)
	}
}

// TestPruneSuffixes: within one second, the numeric suffix orders backups (-10 is newer than -3,
// and the unsuffixed one is the oldest).
func TestPruneSuffixes(t *testing.T) {
	for _, tc := range []struct {
		names []string
		keep  int
		want  []string
	}{
		{[]string{"", "-2"}, 1, []string{"-2"}},
		{[]string{"", "-2", "-3", "-10"}, 2, []string{"-3", "-10"}},
	} {
		dir := t.TempDir()
		file := filepath.Join(dir, "kb.sqlite")
		base := file + ".bak-v1-20261030-161205"
		for _, n := range tc.names {
			if err := os.WriteFile(base+n, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		prune(file, tc.keep)
		for _, n := range tc.names {
			if want := slices.Contains(tc.want, n); exists(base+n) != want {
				t.Errorf("%v keep %d: %q exists=%v, want %v", tc.names, tc.keep, n, !want, want)
			}
		}
	}
}

// TestBackupIsAtomic: a backup that fails half-way leaves no file under the backup name or the
// partial name (it would be pruned as a good one, or trusted by the user), a leftover partial from
// a killed run is replaced, and a good backup appears only under its final name.
func TestBackupIsAtomic(t *testing.T) {
	t.Run("failed copy leaves nothing", func(t *testing.T) {
		f := newFake(t, 1)
		f.backupPartial = true
		_, err := Run(context.Background(), []Artefact{f}, Options{Now: fixedNow})
		if err == nil {
			t.Fatal("Run succeeded although the backup failed")
		}
		if left, _ := filepath.Glob(f.path + ".bak-v*"); len(left) != 0 {
			t.Errorf("files left behind: %v", left)
		}
		if v, _ := f.version(); v != 1 || f.applies.Load() != 0 {
			t.Errorf("version %d, applies %d: nothing may be applied after a failed backup", v, f.applies.Load())
		}
	})
	t.Run("stale partial is replaced", func(t *testing.T) {
		f := newFake(t, 1)
		stale := backupPath(f.path, 1, fixedNow()) + partialSuffix
		if err := os.WriteFile(stale, []byte("from a killed run"), 0o644); err != nil {
			t.Fatal(err)
		}
		rep, err := Run(context.Background(), []Artefact{f}, Options{Now: fixedNow})
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(rep.Artefacts[0].Backup)
		if err != nil || string(got) != "1" {
			t.Errorf("backup = %q, %v; want the artefact's content", got, err)
		}
		if _, err := os.Stat(stale); err == nil {
			t.Error("the partial file is still there")
		}
	})
}
