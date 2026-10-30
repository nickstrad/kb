package sqlite

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/migrate"
)

// TestArtefactStatus: the version is read without touching the file, for every position.
func TestArtefactStatus(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		setup    func(t *testing.T) string
		wantCur  int
		wantPend int
		wantKind errs.Kind // -1 = no error
	}{
		{"missing file: v0, all pending, not created", func(t *testing.T) string { return filepath.Join(t.TempDir(), "kb.sqlite") }, 0, SchemaVersion(), -1},
		{"oldest fixture", func(t *testing.T) string { return loadFixture(t, 1) }, 1, SchemaVersion() - 1, -1},
		{"Target-1", func(t *testing.T) string { return loadFixture(t, SchemaVersion()-1) }, SchemaVersion() - 1, 1, -1},
		{"current", func(t *testing.T) string { return loadFixture(t, SchemaVersion()) }, SchemaVersion(), 0, -1},
		{"newer", func(t *testing.T) string { return withVersion(t, SchemaVersion()+1) }, SchemaVersion() + 1, 0, -1},
		{"negative: damaged", func(t *testing.T) string { return withVersion(t, -1) }, 0, 0, errs.Conflict},
		{"not a database", func(t *testing.T) string {
			p := filepath.Join(t.TempDir(), "kb.sqlite")
			if err := os.WriteFile(p, []byte(strings.Repeat("not sqlite ", 200)), 0o644); err != nil {
				t.Fatal(err)
			}
			return p
		}, 0, 0, errs.Conflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.setup(t)
			_, statErr := os.Stat(path)
			st, err := NewArtefact(path).Status(ctx)
			if tc.wantKind >= 0 {
				if errs.KindOf(err) != tc.wantKind {
					t.Fatalf("err = %v (kind %v), want %v", err, errs.KindOf(err), tc.wantKind)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if st.Artefact != "index" || st.Path != path || st.Target != SchemaVersion() || st.Current != tc.wantCur || len(st.Pending) != tc.wantPend {
				t.Errorf("Status = %+v, want current %d, %d pending", st, tc.wantCur, tc.wantPend)
			}
			if _, err := os.Stat(path); (err == nil) != (statErr == nil) {
				t.Errorf("Status created or removed the file")
			}
		})
	}
}

// withVersion is a current database whose user_version is then set to v.
func withVersion(t *testing.T, v int) string {
	t.Helper()
	path := loadFixture(t, SchemaVersion())
	execAll(t, rawDB(t, path), fmt.Sprintf("PRAGMA user_version = %d", v))
	return path
}

// TestArtefactBackup: the backup is a consistent copy at the same version that passes Verify; an
// existing destination or a missing source is an error and changes nothing.
func TestArtefactBackup(t *testing.T) {
	ctx := context.Background()
	path := loadFixture(t, 2)
	dst := path + ".bak-v2-20261030-161205"
	a := NewArtefact(path)
	if err := a.Backup(ctx, dst); err != nil {
		t.Fatal(err)
	}
	bak := NewArtefact(dst)
	if st, err := bak.Status(ctx); err != nil || st.Current != 2 {
		t.Fatalf("backup Status = %+v, %v", st, err)
	}
	if err := bak.Verify(ctx); err != nil {
		t.Errorf("backup Verify: %v", err)
	}
	if fmt.Sprint(logRows(t, dst)) != fmt.Sprint(logRows(t, path)) {
		t.Error("backup's search log differs from the source")
	}
	if err := a.Backup(ctx, dst); err == nil {
		t.Error("Backup over an existing file succeeded")
	}
	if err := NewArtefact(filepath.Join(t.TempDir(), "none.sqlite")).Backup(ctx, dst+"x"); errs.KindOf(err) != errs.NotFound {
		t.Errorf("Backup of a missing file: %v", err)
	}
}

// TestArtefactVerify: each check Verify makes, failing alone, is a Conflict naming the problem.
func TestArtefactVerify(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, path string)
		want  string // "" = passes
	}{
		{"current", func(*testing.T, string) {}, ""},
		{"v3 before a reindex (no vector table)", func(t *testing.T, path string) {
			execAll(t, rawDB(t, path), "DROP TABLE chunks_vec")
		}, ""},
		{"extra table", func(t *testing.T, path string) {
			execAll(t, rawDB(t, path), "CREATE TABLE stray (x)")
		}, "unexpected: table stray"},
		{"extra index", func(t *testing.T, path string) {
			execAll(t, rawDB(t, path), "CREATE INDEX stray_idx ON entries(title)")
		}, "unexpected: index stray_idx"},
		{"missing table", func(t *testing.T, path string) {
			execAll(t, rawDB(t, path), "DROP TABLE search_opens")
		}, "missing or changed: table search_opens"},
		{"changed column", func(t *testing.T, path string) {
			execAll(t, rawDB(t, path), "ALTER TABLE searches ADD COLUMN extra TEXT")
		}, "missing or changed: table searches"},
		{"dangling foreign key", func(t *testing.T, path string) {
			execAll(t, rawDB(t, path), "INSERT INTO chunks (id, entry_id, ord, source_file, text, text_hash) VALUES (99, 42, 0, 'x.md', 't', 'h')")
		}, "foreign_key_check: chunks row 99"},
		{"newer version", func(t *testing.T, path string) {
			execAll(t, rawDB(t, path), fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion()+1))
		}, "this kb verifies 0 to"},
		{"negative version", func(t *testing.T, path string) {
			execAll(t, rawDB(t, path), "PRAGMA user_version = -1")
		}, "this kb verifies 0 to"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := loadFixture(t, SchemaVersion())
			tc.setup(t, path)
			err := NewArtefact(path).Verify(ctx)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("Verify: %v", err)
			case tc.want != "" && (errs.KindOf(err) != errs.Conflict || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("Verify = %v (kind %v), want Conflict containing %q", err, errs.KindOf(err), tc.want)
			}
		})
	}
	if err := NewArtefact(filepath.Join(t.TempDir(), "none.sqlite")).Verify(ctx); errs.KindOf(err) != errs.NotFound {
		t.Errorf("Verify of a missing file: %v", err)
	}
}

// TestArtefactVerifyDamaged: a page-level corruption fails integrity_check (or the open) as a
// Conflict, never as an incident.
func TestArtefactVerifyDamaged(t *testing.T) {
	path := loadFixture(t, SchemaVersion())
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 4096 + 100; i < len(b) && i < 4096*3; i++ { // scribble over pages 2-3, keep the header
		b[i] = 0xA5
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := NewArtefact(path).Verify(context.Background()); errs.KindOf(err) != errs.Conflict {
		t.Fatalf("Verify of a damaged file = %v (kind %v), want Conflict", err, errs.KindOf(err))
	}
}

// TestRunIndex: migrate.Run on the oldest fixture backs it up (the backup stays at v1 and
// verifies), upgrades it, verifies it, and reports the re-embed the v3 step needs.
func TestRunIndex(t *testing.T) {
	ctx := context.Background()
	path := loadFixture(t, 1)
	now := func() time.Time { return time.Date(2026, 10, 30, 16, 12, 5, 0, time.Local) }
	rep, err := migrate.Run(ctx, []migrate.Artefact{NewArtefact(path)}, migrate.Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	out := rep.Artefacts[0]
	if out.Backup != path+".bak-v1-20261030-161205" || len(out.Applied) != SchemaVersion()-1 || rep.Needs() != migrate.EffectReembed {
		t.Fatalf("outcome = %+v, needs %v", out, rep.Needs())
	}
	if st, err := NewArtefact(out.Backup).Status(ctx); err != nil || st.Current != 1 {
		t.Errorf("backup at %+v, %v; want v1", st, err)
	}
	if st, err := NewArtefact(path).Status(ctx); err != nil || st.Current != SchemaVersion() {
		t.Errorf("store at %+v, %v; want current", st, err)
	}
	// Open works on the result without migrating again.
	s, err := Open(ctx, path, Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("read-only open after Run: %v", err)
	}
	s.Close()
}

// TestApplyCancelledMidStep: Ctrl-C during the last pending step rolls every step of the run back
// (one transaction): the file keeps its starting version and schema, and a rerun succeeds.
func TestApplyCancelledMidStep(t *testing.T) {
	path := loadFixture(t, 1)
	before := schemaAt(t, path)
	ctx, cancel := context.WithCancel(context.Background())
	orig := migrations
	migrations = append(slices.Clone(orig), migrate.Step[execer]{
		Info: migrate.Info{From: len(orig), ID: "index/9999-cancel", Desc: "cancel mid-step"},
		Up: func(ctx context.Context, db execer) error {
			if _, err := db.ExecContext(ctx, "CREATE TABLE half_done (x)"); err != nil {
				return err
			}
			cancel()
			_, err := db.ExecContext(ctx, "CREATE TABLE never (x)")
			return err
		},
	})
	schemaVersion = migrations.Target()
	t.Cleanup(func() { migrations, schemaVersion = orig, orig.Target() })

	rep, err := migrate.Run(ctx, []migrate.Artefact{NewArtefact(path)}, migrate.Options{})
	if errs.KindOf(err) != errs.Interrupted {
		t.Fatalf("Run = %v (kind %v), want Interrupted", err, errs.KindOf(err))
	}
	if got := schemaAt(t, path); !slices.Equal(got, before) {
		t.Errorf("a cancelled run changed the file:\n%s", diffSchema(before, got))
	}
	if bak := rep.Artefacts[0].Backup; bak == "" {
		t.Error("no backup recorded")
	} else if st, err := NewArtefact(bak).Status(context.Background()); err != nil || st.Current != 1 {
		t.Errorf("backup at %+v, %v; want v1", st, err)
	}

	migrations, schemaVersion = orig, orig.Target()
	if _, err := migrate.Run(context.Background(), []migrate.Artefact{NewArtefact(path)}, migrate.Options{}); err != nil {
		t.Fatalf("rerun after the cancel: %v", err)
	}
	if err := NewArtefact(path).Verify(context.Background()); err != nil {
		t.Error(err)
	}
}

// TestApplyFaults: a driver failure at any statement of the upgrade leaves the starting version.
func TestApplyFaults(t *testing.T) {
	for _, match := range []string{"BEGIN IMMEDIATE", "ALTER TABLE searches RENAME", "DROP TABLE IF EXISTS chunks_vec", "PRAGMA user_version = 3", "COMMIT"} {
		t.Run(match, func(t *testing.T) {
			path := loadFixture(t, 1)
			fail(t, match, 1)
			a := &Artefact{path: path, driver: faultDriverName}
			if _, err := a.Apply(context.Background()); !errors.Is(err, errFault) {
				t.Fatalf("Apply = %v, want the fault", err)
			}
			arm("", 0)
			if v := countDBVersion(t, path); v != 1 {
				t.Errorf("user_version = %d after a failed apply, want 1", v)
			}
		})
	}
}

func countDBVersion(t *testing.T, path string) int {
	t.Helper()
	return count(t, rawDB(t, path), "PRAGMA user_version")
}

// TestRunIndexConcurrent: two runners on one v1 store: one applies, the other waits on the lock
// and finds nothing to do. Run with -race.
func TestRunIndexConcurrent(t *testing.T) {
	path := loadFixture(t, 1)
	var wg sync.WaitGroup
	reps := make([]migrate.Report, 2)
	errsOut := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			now := func() time.Time { return time.Date(2026, 10, 30, 16, 12, i, 0, time.Local) }
			reps[i], errsOut[i] = migrate.Run(context.Background(), []migrate.Artefact{NewArtefact(path)}, migrate.Options{Now: now})
		}()
	}
	wg.Wait()
	applied := 0
	for i := range 2 {
		if errsOut[i] != nil {
			t.Fatalf("runner %d: %v", i, errsOut[i])
		}
		applied += len(reps[i].Artefacts[0].Applied)
	}
	if applied != SchemaVersion()-1 {
		t.Errorf("steps applied across both runners = %d, want %d (one runner)", applied, SchemaVersion()-1)
	}
	if baks, _ := filepath.Glob(path + ".bak-v*"); len(baks) != 1 {
		t.Errorf("backups = %v, want one", baks)
	}
}

// TestOpenNegativeVersion: Open refuses a negative user_version as damage, read-write and
// read-only, rather than migrating from it.
func TestOpenNegativeVersion(t *testing.T) {
	for _, ro := range []bool{false, true} {
		path := withVersion(t, -1)
		if _, err := Open(context.Background(), path, Options{ReadOnly: ro}); errs.KindOf(err) != errs.Conflict || !strings.Contains(err.Error(), "negative") {
			t.Errorf("Open(ReadOnly=%v) = %v, want Conflict naming the negative version", ro, err)
		}
	}
}
