package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/errs"
)

// synHits runs `kb search <query> --mode fts --json` and returns the hit paths and the exit code.
func synHits(t *testing.T, query string) (paths []string, stderr string, exit int) {
	t.Helper()
	stdout, stderr, exit := runKB(t, []string{"search", query, "--mode", "fts", "--json"})
	if exit != 0 {
		return nil, stderr, exit
	}
	var res struct{ Hits []struct{ Path string } }
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	for _, h := range res.Hits {
		paths = append(paths, h.Path)
	}
	return paths, stderr, exit
}

// TestSearchSynonyms: a synonyms.txt in the store directory makes "signout" find the "logout"
// entry, kb explain shows the expanded match, and without the file the search finds nothing.
func TestSearchSynonyms(t *testing.T) {
	t.Parallel()
	root := newExplainRepo(t)
	useRoot(t, root)
	withFixtureEmbedder(t)
	synPath := filepath.Join(testWS(root).StoreDir, "synonyms.txt")

	if paths, _, exit := synHits(t, "signout"); exit != 0 || len(paths) != 0 {
		t.Fatalf("without synonyms: exit %d hits %v, want none", exit, paths)
	}
	writeFile(t, synPath, "# session words\nSignout, Logout\n")
	paths, stderr, exit := synHits(t, "signout")
	if exit != 0 || stderr != "" || len(paths) == 0 {
		t.Fatalf("with synonyms: exit %d stderr %q hits %v, want hits", exit, stderr, paths)
	}

	stdout, stderr, exit := runKB(t, []string{"explain", "signout", "--mode", "fts"})
	if exit != 0 {
		t.Fatalf("explain: exit %d %s", exit, stderr)
	}
	for _, want := range []string{`fts: match ("signout" OR "logout") · AND`, "synonyms: signout = logout"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("explain lacks %q:\n%s", want, stdout)
		}
	}
	out, _, _ := runKB(t, []string{"explain", "signout", "--mode", "fts", "--json"})
	var ex struct{ Settings_hash string }
	json.Unmarshal([]byte(out), &ex)
	os.Remove(synPath)
	out2, _, _ := runKB(t, []string{"explain", "signout", "--mode", "fts", "--json"})
	var ex2 struct{ Settings_hash string }
	json.Unmarshal([]byte(out2), &ex2)
	if ex.Settings_hash == "" || ex.Settings_hash == ex2.Settings_hash {
		t.Errorf("settings hash with synonyms %q vs without %q: want different, non-empty", ex.Settings_hash, ex2.Settings_hash)
	}
}

// TestSearchSynonymsLookupOrder: the store's file wins over the global store's, the global one is
// used when the store has none, and search.synonyms names the one file used.
func TestSearchSynonymsLookupOrder(t *testing.T) {
	t.Parallel()
	useGlobalLocation(t, "default")
	global := filepath.Join(globalLocationRoot(t), ".kb", "stores", "default", "synonyms.txt")
	root := newExplainRepo(t)
	useRoot(t, root)
	withFixtureEmbedder(t)
	own := filepath.Join(testWS(root).StoreDir, "synonyms.txt")

	writeFile(t, global, "signout, logout\n")
	if paths, _, _ := synHits(t, "signout"); len(paths) == 0 {
		t.Error("global synonyms were not used when the store has none")
	}
	writeFile(t, own, "signout, nonexistentword\n") // the store's file replaces the global one
	if paths, _, _ := synHits(t, "signout"); len(paths) != 0 {
		t.Errorf("store file did not take precedence over the global one: hits %v", paths)
	}
	writeFile(t, filepath.Join(root, "my-syn.txt"), "signout, volumes\n")
	writeConfig(t, root, "[embed]\nmodel = \""+testModel768+"\"\n[search]\nsynonyms = \"my-syn.txt\"\n")
	paths, _, exit := synHits(t, "signout")
	if exit != 0 || len(paths) != 1 || paths[0] != "docker.md" {
		t.Errorf("search.synonyms: exit %d hits %v, want only docker.md", exit, paths)
	}
}

// TestSearchSynonymsFailures: a named file that is missing, and a file that is too big or not a
// plain file, fail the search with exit 1 and a message naming the file.
func TestSearchSynonymsFailures(t *testing.T) {
	t.Parallel()
	root := newExplainRepo(t)
	useRoot(t, root)
	withFixtureEmbedder(t)
	own := filepath.Join(testWS(root).StoreDir, "synonyms.txt")

	writeFile(t, own, strings.Repeat("#", 1<<20+1))
	_, stderr, exit := synHits(t, "logout")
	if exit != ExitUsage || !strings.Contains(stderr, own) || !strings.Contains(stderr, "1 MiB") {
		t.Errorf("oversized: exit %d stderr %q, want usage error naming the file and the cap", exit, stderr)
	}
	os.Remove(own)
	if err := os.Mkdir(own, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, stderr, exit = synHits(t, "logout"); exit != ExitUsage || !strings.Contains(stderr, "not a regular file") {
		t.Errorf("directory: exit %d stderr %q", exit, stderr)
	}
	os.Remove(own)

	writeConfig(t, root, "[embed]\nmodel = \""+testModel768+"\"\n[search]\nsynonyms = \"nope.txt\"\n")
	_, stderr, exit = synHits(t, "logout")
	if exit != ExitUsage || !strings.Contains(stderr, "nope.txt") || !strings.Contains(stderr, "search.synonyms") {
		t.Errorf("missing named file: exit %d stderr %q, want failure naming search.synonyms and the file", exit, stderr)
	}
}

// TestLoadSynonymsLookup is loadSynonyms' decision table: search.synonyms set (present, missing),
// else the store's own file (present, unreadable), else the global location's default store
// (present, absent, unreadable, no global location, a broken global kb.toml). Len is the number
// of terms that expand: own "a, b" has 2, global "c, d, e" has 3.
func TestLoadSynonymsLookup(t *testing.T) {
	t.Parallel()
	const (
		absent = iota
		present
		broken  // a directory where the file should be
		missing // named in search.synonyms, not on disk
	)
	for _, tc := range []struct {
		name      string
		named     int // search.synonyms: absent = unset
		own       int
		global    int // the global default store's file
		noGlobal  bool
		badGlobal bool // the global kb.toml cannot be read
		wantLen   int
		wantKind  errs.Kind // 0 = no error
		wantWarn  bool
	}{
		{name: "named file", named: present, own: present, global: present, wantLen: 4},
		{name: "named file missing", named: missing, own: present, wantKind: errs.NotFound},
		{name: "own file wins", own: present, global: present, wantLen: 2},
		{name: "own file unreadable", own: broken, global: present, wantKind: errs.Usage},
		{name: "global file", global: present, wantLen: 3},
		{name: "global location without a file", wantLen: 0},
		{name: "global file unreadable", global: broken, wantKind: errs.Usage},
		{name: "no global location", noGlobal: true, wantLen: 0},
		{name: "broken global kb.toml warns", badGlobal: true, wantLen: 0, wantWarn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newTempRoot(t)
			ws := testWS(root)
			var cfg config.Config
			if !tc.noGlobal {
				groot := useGlobalLocation(t, "default")
				if tc.badGlobal {
					writeFile(t, filepath.Join(groot, ".kb", "kb.toml"), "colour = 1\n")
				}
			}
			place := func(path string, state int, text string) {
				switch state {
				case present:
					writeFile(t, path, text)
				case broken:
					if err := os.MkdirAll(path, 0o755); err != nil {
						t.Fatal(err)
					}
				}
			}
			place(filepath.Join(ws.StoreDir, "synonyms.txt"), tc.own, "a, b\n")
			place(filepath.Join(globalLocationRoot(t), ".kb", "stores", "default", "synonyms.txt"), tc.global, "c, d, e\n")
			switch tc.named {
			case present:
				writeFile(t, filepath.Join(root, "named.txt"), "w, x\ny, z\n")
				cfg.Search.Synonyms = "named.txt"
			case missing:
				cfg.Search.Synonyms = "nope.txt"
			}
			var stderr strings.Builder
			cmd := &cobra.Command{}
			cmd.SetErr(&stderr)
			syn, err := depsFor(t).loadSynonyms(cmd, ws, cfg)
			if tc.wantKind != 0 {
				if errs.KindOf(err) != tc.wantKind {
					t.Fatalf("err = %v (kind %v), want kind %v", err, errs.KindOf(err), tc.wantKind)
				}
				return
			}
			if err != nil || syn.Len() != tc.wantLen {
				t.Fatalf("Len = %d, err %v; want %d", syn.Len(), err, tc.wantLen)
			}
			if got := stderr.String() != ""; got != tc.wantWarn {
				t.Errorf("stderr %q, want a warning: %v", stderr.String(), tc.wantWarn)
			}
		})
	}
}

// TestGlobalSynonymsWarnsOnce: a broken global kb.toml, and a global default store that cannot be
// opened, each earn one warning per command however many stores load synonyms (a --stores
// fan-out), and the failed lookup is no synonyms.
func TestGlobalSynonymsWarnsOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, groot string)
	}{
		{"broken kb.toml", func(t *testing.T, groot string) {
			writeFile(t, filepath.Join(groot, ".kb", "kb.toml"), "colour = 1\n")
		}},
		{"default store is not a name", func(t *testing.T, groot string) {
			writeFile(t, filepath.Join(groot, ".kb", "kb.toml"), "default_store = \"Bad Name\"\n")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newTempRoot(t)
			tc.setup(t, useGlobalLocation(t, "default"))
			var stderr strings.Builder
			cmd := &cobra.Command{}
			cmd.SetErr(&stderr)
			for range 3 {
				syn, err := depsFor(t).loadSynonyms(cmd, testWS(root), config.Config{})
				if err != nil || syn.Len() != 0 {
					t.Fatalf("Len = %d, err %v; want no synonyms", syn.Len(), err)
				}
			}
			if n := strings.Count(stderr.String(), "warning: global synonyms skipped"); n != 1 {
				t.Errorf("%d warnings, want 1: %q", n, stderr.String())
			}
		})
	}
}
