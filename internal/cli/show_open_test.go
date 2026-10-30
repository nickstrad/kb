package cli

import (
	"context"
	"github.com/nickstrad/kb/internal/store/sqlite"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openRepo is a repo with foo.md and bar/ indexed. A search for "foo" returns data/foo.md only.
func openRepo(t *testing.T) string {
	t.Helper()
	root := setupRepo(t)
	indexEntry(t, context.Background(), root, "foo.md")
	indexEntry(t, context.Background(), root, "bar")
	t.Setenv("KB_ROOT", root)
	return root
}

func openRows(t *testing.T, root string) []string {
	t.Helper()
	st, err := sqlite.Open(context.Background(), DBPath(root), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rows, err := st.DB().Query("SELECT search_id || ' ' || entry_path FROM search_opens ORDER BY search_id, entry_path")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func mustKB(t *testing.T, args ...string) (stdout, stderr string) {
	t.Helper()
	stdout, stderr, exit := runKB(t, args)
	if exit != 0 {
		t.Fatalf("kb %v: exit %d; stderr=%s", args, exit, stderr)
	}
	return stdout, stderr
}

func TestShowRecordsOpenOfRecentSearch(t *testing.T) {
	root := openRepo(t)
	id := searchID(t, "foo")
	out, stderr := mustKB(t, "show", "foo.md")
	if !strings.Contains(out, "Intro text about foo.") || stderr != "" {
		t.Errorf("show output changed: stdout=%q stderr=%q", out, stderr)
	}
	if got, want := openRows(t, root), []string{id + " data/foo.md"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("search_opens = %v, want %v", got, want)
	}

	// Showing again, with --chunks too, stays one row.
	mustKB(t, "show", "foo.md")
	mustKB(t, "show", "foo.md", "--chunks")
	if got := openRows(t, root); len(got) != 1 {
		t.Errorf("after repeated show, search_opens = %v, want one row", got)
	}
}

func TestShowWithoutMatchingSearchRecordsNothing(t *testing.T) {
	root := openRepo(t)

	mustKB(t, "show", "foo.md") // no search at all
	if got := openRows(t, root); len(got) != 0 {
		t.Errorf("show without a search: search_opens = %v, want none", got)
	}

	searchID(t, "foo")
	mustKB(t, "show", "bar") // indexed, but not returned by that search
	if got := openRows(t, root); len(got) != 0 {
		t.Errorf("show of a non-returned entry: search_opens = %v, want none", got)
	}
}

func TestShowOpensNewestOfTwoSearches(t *testing.T) {
	root := openRepo(t)
	searchID(t, "foo")
	second := searchID(t, "foo")
	mustKB(t, "show", "foo.md")
	if got := openRows(t, root); len(got) != 1 || got[0] != second+" data/foo.md" {
		t.Errorf("search_opens = %v, want only search %s", got, second)
	}
}

func TestShowNoLog(t *testing.T) {
	root := openRepo(t)
	searchID(t, "foo")
	_, stderr := mustKB(t, "show", "foo.md", "--no-log", "--debug")
	if got := openRows(t, root); len(got) != 0 {
		t.Errorf("--no-log: search_opens = %v, want none", got)
	}
	if stderr != "" {
		t.Errorf("--no-log --debug printed %q; nothing was attempted", stderr)
	}
}

func TestShowWithoutDatabaseStaysQuiet(t *testing.T) {
	root := setupRepo(t)
	t.Setenv("KB_ROOT", root)
	_, stderr := mustKB(t, "show", "foo.md")
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	if _, err := os.Stat(DBPath(root)); !os.IsNotExist(err) {
		t.Errorf("kb show created the database (stat err %v)", err)
	}
	_, stderr = mustKB(t, "show", "foo.md", "--debug")
	if !strings.Contains(stderr, "kb show: open not recorded: no index yet") {
		t.Errorf("--debug stderr = %q", stderr)
	}
}

// TestShowOpenFailureIsSilentButNotSwallowed: the entry has been printed, so an unusable database
// must not change the exit code; --debug is where the cause surfaces.
func TestShowOpenFailureIsSilentButNotSwallowed(t *testing.T) {
	root := setupRepo(t)
	t.Setenv("KB_ROOT", root)
	if err := os.MkdirAll(filepath.Dir(DBPath(root)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DBPath(root), []byte("this is not a sqlite database, just junk bytes padding it out"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, stderr := mustKB(t, "show", "foo.md")
	if !strings.Contains(out, "Intro text about foo.") {
		t.Errorf("stdout = %q, want the entry", out)
	}
	if stderr != "" {
		t.Errorf("default stderr = %q, want silent", stderr)
	}

	out, stderr = mustKB(t, "show", "foo.md", "--debug")
	if !strings.Contains(out, "Intro text about foo.") || !strings.Contains(stderr, "kb show: open not recorded: ") {
		t.Errorf("--debug: stdout=%q stderr=%q", out, stderr)
	}
}

func TestShowVerboseExplainsNoMatch(t *testing.T) {
	openRepo(t)
	_, stderr := mustKB(t, "show", "foo.md", "--debug")
	if !strings.Contains(stderr, "no search in the last 30m0s returned data/foo.md") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestShowAndSearchHelpMentionOpens(t *testing.T) {
	out, _ := mustKB(t, "show", "--help")
	for _, want := range []string{"--no-log", "--debug", "opened_pct", "30 minutes"} {
		if !strings.Contains(out, want) {
			t.Errorf("show --help lacks %q:\n%s", want, out)
		}
	}
	out, _ = mustKB(t, "search", "--help")
	if !strings.Contains(out, "recorded as an open") || !strings.Contains(out, "Feedback is optional") {
		t.Errorf("search --help:\n%s", out)
	}
}
