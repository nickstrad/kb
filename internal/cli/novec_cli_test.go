package cli

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// dropVectorsCLI leaves the root's database the way migration 2→3 does: entries, chunks and FTS
// intact, no chunks_vec and no embed_meta.
func dropVectorsCLI(t *testing.T, root string) {
	t.Helper()
	db, err := sql.Open("sqlite3", testDBPath(root))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("DROP TABLE chunks_vec; DELETE FROM embed_meta;"); err != nil {
		t.Fatalf("drop vectors: %v", err)
	}
}

// TestNoVectorsMessages covers Q24 and Q25 on an upgraded, not yet reindexed index: search still
// exits 2 (FTS only) but its warning names the missing vectors and the fix rather than blaming the
// embedder, and doctor does not call a database with entries "empty".
func TestNoVectorsMessages(t *testing.T) {
	useFakeEmbedder(t)
	root := newTempRoot(t)
	writeFile(t, filepath.Join(root, "data", "a.md"), validFrontMatter("Alpha", "alpha summary"))
	if _, stderr, code := run(t, "reindex", "--all"); code != ExitOK {
		t.Fatalf("reindex: exit %d\n%s", code, stderr)
	}
	dropVectorsCLI(t, root)

	stdout, stderr, code := run(t, "search", "alpha")
	if code != ExitEmbedder {
		t.Fatalf("search exit = %d, want %d\n%s", code, ExitEmbedder, stderr)
	}
	if !strings.Contains(stderr, "no vectors yet") || !strings.Contains(stderr, "kb reindex --all") ||
		strings.Contains(stderr, "embedder unavailable") {
		t.Errorf("search stderr = %q; want the no-vectors hint, not \"embedder unavailable\"", stderr)
	}
	if !strings.Contains(stdout, "data/a.md") {
		t.Errorf("search printed no FTS hits:\n%s", stdout)
	}

	stdout, _, _ = run(t, "doctor")
	if !strings.Contains(stdout, "1 entry is indexed") || strings.Contains(stdout, "empty database") {
		t.Errorf("doctor embed_meta line wrong:\n%s", stdout)
	}
}

// TestDoctorEntryNounPlural (Q29) checks the entry/entries noun and is/are verb in the doctor
// embed_meta line at the singular/plural boundary: 1 entry, then 2 entries.
func TestDoctorEntryNounPlural(t *testing.T) {
	useFakeEmbedder(t)
	for _, tc := range []struct {
		n    int
		want string
	}{
		{1, "1 entry is indexed"},
		{2, "2 entries are indexed"},
	} {
		root := newTempRoot(t)
		for i := 1; i <= tc.n; i++ {
			writeFile(t, filepath.Join(root, "data", "a"+strconv.Itoa(i)+".md"),
				validFrontMatter("Alpha "+strconv.Itoa(i), "alpha summary "+strconv.Itoa(i)))
		}
		if _, stderr, code := run(t, "reindex", "--all"); code != ExitOK {
			t.Fatalf("n=%d reindex: exit %d\n%s", tc.n, code, stderr)
		}
		dropVectorsCLI(t, root)

		stdout, _, _ := run(t, "doctor")
		if !strings.Contains(stdout, tc.want) {
			t.Errorf("n=%d doctor output missing %q:\n%s", tc.n, tc.want, stdout)
		}
	}
}
