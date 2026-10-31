package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/kb/internal/errs"
)

// setUserVersion stamps PRAGMA user_version on a closed database file.
func setUserVersion(t *testing.T, path string, v int) {
	t.Helper()
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
		t.Fatal(err)
	}
}

func fileHash(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestOpenSchemaVersion: rows at, above and below the build's version. A read-write open refuses
// an older database (kb migrate up upgrades it) and a newer one; a read-only open (kb stats)
// cannot migrate, so it refuses both, with a Conflict that names the file and leaves it unchanged.
// "Older" is a current file opened by a ProbeSteps build, one release ahead.
func TestOpenSchemaVersion(t *testing.T) {
	cur := SchemaVersion()
	for _, tc := range []struct {
		name     string
		version  int
		steps    Steps // the opening build's; nil = this build's
		readOnly bool
		wantErr  string // "" = opens
	}{
		{"read-only at SchemaVersion", cur, nil, true, ""},
		{"read-only SchemaVersion+1 (newer kb)", cur + 1, nil, true, "only understands"},
		{"read-only one release older (not migrated)", cur, ProbeSteps(), true, "kb migrate up"},
		{"read-write one release older (not migrated)", cur, ProbeSteps(), false, "kb migrate up"},
		{"read-write SchemaVersion+1 (newer kb)", cur + 1, nil, false, "only understands"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "kb.sqlite")
			st, err := Open(ctx, path, Options{})
			if err != nil {
				t.Fatal(err)
			}
			st.Close()
			setUserVersion(t, path, tc.version)
			before := fileHash(t, path)

			db, err := Open(ctx, path, Options{ReadOnly: tc.readOnly, Steps: tc.steps})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Open: %v", err)
				}
				if !db.ReadOnly() {
					t.Error("ReadOnly() = false")
				}
				db.Close()
				return
			}
			if err == nil {
				db.Close()
				t.Fatalf("Open accepted a database at schema version %d", tc.version)
			}
			if errs.KindOf(err) != errs.Conflict || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), path) {
				t.Errorf("err = %q (kind %v); want a Conflict naming %s and containing %q", err, errs.KindOf(err), path, tc.wantErr)
			}
			if fileHash(t, path) != before {
				t.Error("the refused database was modified")
			}
		})
	}
}

// TestOpenReadOnlyErrors: a read-only open never creates a file, and each way the file can be
// wrong is a well-formed error of the right kind.
func TestOpenReadOnlyErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("missing file is NotFound and not created", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "absent.sqlite")
		_, err := Open(ctx, path, Options{ReadOnly: true})
		if errs.KindOf(err) != errs.NotFound || !strings.Contains(err.Error(), "no database at") {
			t.Fatalf("err = %v (kind %v), want NotFound", err, errs.KindOf(err))
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("Open created %s", path)
		}
	})
	t.Run("a directory is an error", func(t *testing.T) {
		db, err := Open(ctx, t.TempDir(), Options{ReadOnly: true})
		if err == nil {
			db.Close()
			t.Fatal("Open on a directory succeeded")
		}
	})
	t.Run("not a database is a Conflict", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "junk.sqlite")
		if err := os.WriteFile(p, []byte(strings.Repeat("not sqlite ", 500)), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, ro := range []bool{true, false} {
			db, err := Open(ctx, p, Options{ReadOnly: ro})
			if err == nil {
				db.Close()
				t.Fatalf("read-only=%v: Open accepted a non-database", ro)
			}
			if errs.KindOf(err) != errs.Conflict || !strings.Contains(err.Error(), "damaged") {
				t.Errorf("read-only=%v: err = %v (kind %v), want a Conflict saying damaged", ro, err, errs.KindOf(err))
			}
		}
	})
}

// TestReadOnlyRefusesWrites: through QueryLog a read-only store cannot write, and opening and
// querying leave the file's bytes alone.
func TestReadOnlyRefusesWrites(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kb.sqlite")
	st, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	before := fileHash(t, path)
	ro, err := Open(ctx, path, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ro.QueryLog(ctx, "DELETE FROM searches"); err == nil {
		t.Error("DELETE through QueryLog succeeded on a read-only store")
	}
	if _, err := ro.QueryLog(ctx, "SELECT count(*) FROM searches"); err != nil {
		t.Fatal(err)
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	if fileHash(t, path) != before {
		t.Error("database bytes changed")
	}
}

// TestQueryLogNormalizesValues: a BLOB comes back as text, an integer as int64, and a driver
// failure carries SQLite's own message.
func TestQueryLogNormalizesValues(t *testing.T) {
	ctx := context.Background()
	st := openTestDB(t)
	tbl, err := st.QueryLog(ctx, "SELECT x'4142' AS b, 1 AS n")
	if err != nil {
		t.Fatal(err)
	}
	if len(tbl.Rows) != 1 || tbl.Rows[0][0] != "AB" || tbl.Rows[0][1] != int64(1) {
		t.Fatalf("rows = %#v, want [[\"AB\" 1]]", tbl.Rows)
	}
	if _, err := st.QueryLog(ctx, "SELEKT 1"); err == nil || !strings.Contains(err.Error(), "syntax error") {
		t.Fatalf("err = %v, want SQLite's syntax error", err)
	}
}

func TestNormalizeValue(t *testing.T) {
	ts := time.Date(2026, 10, 29, 12, 0, 0, 0, time.FixedZone("x", 3600))
	for _, tc := range []struct {
		name string
		in   any
		want any
	}{
		{"bytes become text", []byte("ab"), "ab"},
		{"time becomes UTC RFC3339", ts, "2026-10-29T11:00:00Z"},
		{"true is 1", true, int64(1)},
		{"false is 0", false, int64(0)},
		{"int64 as is", int64(7), int64(7)},
		{"nil as is", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeValue(tc.in); got != tc.want {
				t.Fatalf("normalizeValue(%v) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

// TestRefusedOpenLeavesFileAlone: an older database (kb migrate up has not run) and a newer one
// are refused before anything is written, including the switch to WAL that a read-write open
// otherwise makes. The files here are in the default rollback-journal mode, as an older kb left them.
func TestRefusedOpenLeavesFileAlone(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		version int
		steps   Steps // the opening build's; nil = this build's
	}{{"older", SchemaVersion(), ProbeSteps()}, {"newer", SchemaVersion() + 1, nil}} {
		t.Run(tc.name, func(t *testing.T) {
			path := loadFixture(t, SchemaVersion())
			setUserVersion(t, path, tc.version)
			before := fileHash(t, path)
			if _, err := Open(ctx, path, Options{Steps: tc.steps}); errs.KindOf(err) != errs.Conflict {
				t.Fatalf("Open = %v, want Conflict", err)
			}
			if fileHash(t, path) != before {
				t.Error("a refused open changed the file")
			}
			var mode string
			if err := rawDB(t, path).QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "delete" {
				t.Errorf("journal_mode = %q, %v; want delete (not switched to WAL)", mode, err)
			}
			for _, side := range []string{"-wal", "-shm"} {
				if _, err := os.Stat(path + side); err == nil {
					t.Errorf("a refused open left %s", path+side)
				}
			}
		})
	}
}
