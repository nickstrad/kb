package cli

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/embed/cache"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/store/sqlite"
)

// testPkgDir is this package's directory: the tests chdir into temporary locations, and the
// fixtures are found relative to it.
var testPkgDir = func() string {
	d, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return d
}()

// fixtureSQL is the checked-in SQL text of the index database at schema version v.
func fixtureSQL(t *testing.T, v int) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testPkgDir, "..", "store", "sqlite", "testdata", "migrations", fmt.Sprintf("index-v%d.sql", v)))
	if err != nil {
		t.Fatalf("read the v%d index fixture: %v", v, err)
	}
	return string(b)
}

// installIndex replaces the database at path with the fixture for schema version v: what an older
// kb left behind.
func installIndex(t *testing.T, path string, v int) {
	t.Helper()
	// Opening a database registers sqlite-vec for the plain driver the fixture needs.
	tmp, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "reg.sqlite"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	tmp.Close()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Remove(path + suffix)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(fixtureSQL(t, v)); err != nil {
		t.Fatalf("load the v%d fixture: %v", v, err)
	}
}

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func backupFiles(t *testing.T, path string) []string {
	t.Helper()
	m, err := filepath.Glob(path + ".bak-v*")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// indexState puts the default store's database in one of the states of the commands table.
func indexState(t *testing.T, root, state string) {
	t.Helper()
	db := testDBPath(root)
	switch state {
	case "current":
		st, err := sqlite.Open(context.Background(), db, sqlite.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.EnsureEmbedMeta(context.Background(), store.EmbedMeta{Model: store.FTSOnlyModel}); err != nil {
			t.Fatal(err)
		}
		st.Close()
	case "older":
		installIndex(t, db, sqlite.SchemaVersion()-1)
	case "newer":
		st, err := sqlite.Open(context.Background(), db, sqlite.Options{})
		if err != nil {
			t.Fatal(err)
		}
		st.Close()
		setUserVersion(t, db, sqlite.SchemaVersion()+1)
	case "missing":
	default:
		t.Fatalf("unknown state %q", state)
	}
}

// TestCommandsOnIndexStates is the refusal table (PLAN 1.8b): command x state of the store's
// index database, asserting the exit code and what the user is told. Every command that opens the
// index refuses an older or newer one without touching it; the exempt commands (doctor, stores,
// config --path, version, migrate) explain instead.
func TestCommandsOnIndexStates(t *testing.T) {
	const (
		older = "Run: kb migrate up"
		newer = "use a newer kb"
	)
	type want struct {
		code     int
		out, err string // substrings of stdout and stderr
	}
	commands := []struct {
		args []string
		// per state: current, older, newer, missing
		cur, old, new, missing want
	}{
		{[]string{"search", "zzz", "--mode", "fts"}, want{0, "", ""}, want{1, "", older}, want{1, "", newer}, want{1, "", "no index"}},
		{[]string{"explain", "zzz", "--mode", "fts"}, want{0, "", ""}, want{1, "", older}, want{1, "", newer}, want{1, "", "no index"}},
		{[]string{"stats", "overview"}, want{0, "", ""}, want{1, "", older}, want{1, "", newer}, want{1, "", "no search database"}},
		{[]string{"reindex", "--all"}, want{0, "", ""}, want{1, "", older}, want{1, "", newer}, want{0, "", ""}},
		// exempt commands
		{[]string{"doctor"}, want{0, "ok   driver", ""}, want{1, "FAIL migrate: ", ""}, want{1, "FAIL driver: ", ""}, want{0, "warn database: not indexed", ""}},
		{[]string{"stores"}, want{0, "default", ""}, want{0, older, ""}, want{0, newer, ""}, want{0, "not indexed", ""}},
		{[]string{"config", "--path"}, want{0, "config.toml", ""}, want{0, "config.toml", ""}, want{0, "config.toml", ""}, want{0, "config.toml", ""}},
		{[]string{"version"}, want{0, "kb ", ""}, want{0, "kb ", ""}, want{0, "kb ", ""}, want{0, "kb ", ""}},
		{[]string{"migrate", "status"}, want{0, "current", ""}, want{1, "pending", ""}, want{1, "newer", ""}, want{0, "not created yet", ""}},
		{[]string{"migrate", "verify"}, want{0, "ok   store default kb.sqlite", ""}, want{0, "ok   store default kb.sqlite", ""}, want{1, "FAIL store default kb.sqlite", ""}, want{0, "ok   store default config.toml", ""}},
	}
	for _, state := range []string{"current", "older", "newer", "missing"} {
		for _, c := range commands {
			t.Run(state+"/"+strings.Join(c.args, " "), func(t *testing.T) {
				root := newTempRoot(t)
				useNoneModel(t, root)
				indexState(t, root, state)
				var before string
				if state != "missing" {
					before = fileDigest(t, testDBPath(root))
				}
				w := map[string]want{"current": c.cur, "older": c.old, "newer": c.new, "missing": c.missing}[state]
				stdout, stderr, code := run(t, c.args...)
				if code != w.code || !strings.Contains(stdout, w.out) || !strings.Contains(stderr, w.err) {
					t.Fatalf("kb %s on a %s index: exit %d stdout %q stderr %q; want exit %d, stdout containing %q, stderr containing %q",
						strings.Join(c.args, " "), state, code, stdout, stderr, w.code, w.out, w.err)
				}
				// Nothing but the commands that write on a current index may change an older or newer file.
				if (state == "older" || state == "newer") && fileDigest(t, testDBPath(root)) != before {
					t.Errorf("kb %s changed the %s database", strings.Join(c.args, " "), state)
				}
			})
		}
	}
}

// TestMigrateUpUpgrades is the accept path: an older store is refused, `kb migrate up` backs it up
// and upgrades it, and the same command then works. From v2 the re-embed effect is reported.
func TestMigrateUpUpgrades(t *testing.T) {
	root := newTempRoot(t)
	useNoneModel(t, root)
	db := testDBPath(root)
	installIndex(t, db, 2)

	if _, stderr, code := run(t, "search", "zzz", "--mode", "fts"); code != 1 || !strings.Contains(stderr, "Run: kb migrate up") {
		t.Fatalf("search on a v2 store: exit %d %q", code, stderr)
	}
	stdout, stderr, code := run(t, "migrate", "up")
	if code != 0 || stderr != "" {
		t.Fatalf("migrate up: exit %d stderr %q\n%s", code, stderr, stdout)
	}
	baks := backupFiles(t, db)
	if len(baks) != 1 {
		t.Fatalf("backups = %v, want exactly one", baks)
	}
	for _, want := range []string{
		"migrated store default index: v2 -> v" + fmt.Sprint(sqlite.SchemaVersion()),
		"backup: " + baks[0],
		"applied index/0003-vec-width",
		"needs: kb reindex (store default: index/0003-vec-width)",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("migrate up output lacks %q:\n%s", want, stdout)
		}
	}
	if st, err := sqlite.NewArtefact(baks[0]).Status(context.Background()); err != nil || st.Current != 2 {
		t.Errorf("the backup is at %+v, %v; want v2", st, err)
	}
	if _, stderr, code := run(t, "search", "zzz", "--mode", "fts"); code != 0 {
		t.Errorf("search after migrate up: exit %d %q", code, stderr)
	}
	if _, _, code := run(t, "migrate", "status"); code != 0 {
		t.Errorf("status after up: exit %d, want 0", code)
	}
	// A second up has nothing to do and writes no second backup.
	stdout, _, code = run(t, "migrate", "up")
	if code != 0 || !strings.Contains(stdout, "everything is up to date") || len(backupFiles(t, db)) != 1 {
		t.Errorf("second up: exit %d\n%s\nbackups %v", code, stdout, backupFiles(t, db))
	}
}

// TestMigrateUpOptions: --dry-run writes nothing, --no-backup writes no backup.
func TestMigrateUpOptions(t *testing.T) {
	t.Run("dry run", func(t *testing.T) {
		root := newTempRoot(t)
		useNoneModel(t, root)
		db := testDBPath(root)
		installIndex(t, db, sqlite.SchemaVersion()-1)
		before := fileDigest(t, db)
		stdout, _, code := run(t, "migrate", "up", "--dry-run")
		if code != 0 || !strings.Contains(stdout, "would migrate store default index") || !strings.Contains(stdout, "backup would be written to ") {
			t.Fatalf("exit %d\n%s", code, stdout)
		}
		if fileDigest(t, db) != before || len(backupFiles(t, db)) != 0 {
			t.Error("--dry-run changed the database or wrote a backup")
		}
		if _, err := os.Stat(db + "-wal"); err == nil {
			t.Error("--dry-run left a WAL file")
		}
	})
	t.Run("no backup", func(t *testing.T) {
		root := newTempRoot(t)
		useNoneModel(t, root)
		db := testDBPath(root)
		installIndex(t, db, sqlite.SchemaVersion()-1)
		stdout, _, code := run(t, "migrate", "up", "--no-backup")
		if code != 0 || !strings.Contains(stdout, "no backup (--no-backup)") || len(backupFiles(t, db)) != 0 {
			t.Fatalf("exit %d, backups %v\n%s", code, backupFiles(t, db), stdout)
		}
		if st, _ := sqlite.NewArtefact(db).Status(context.Background()); st.Current != sqlite.SchemaVersion() {
			t.Errorf("not upgraded: %+v", st)
		}
	})
	t.Run("newer is refused untouched", func(t *testing.T) {
		root := newTempRoot(t)
		useNoneModel(t, root)
		indexState(t, root, "newer")
		before := fileDigest(t, testDBPath(root))
		_, stderr, code := run(t, "migrate", "up")
		if code != 1 || !strings.Contains(stderr, "newer kb") || fileDigest(t, testDBPath(root)) != before || len(backupFiles(t, testDBPath(root))) != 0 {
			t.Errorf("exit %d %q", code, stderr)
		}
	})
}

// TestMigrateScope: the default scope is the resolved store, --store picks another, --all covers
// every store; --all with --store is a usage error; no location is NotFound.
func TestMigrateScope(t *testing.T) {
	root := newTempRoot(t)
	useNoneModel(t, root)
	addStore(t, root, "adr")
	adrDB := filepath.Join(root, ".kb", "stores", "adr", "kb.sqlite")
	installIndex(t, testDBPath(root), sqlite.SchemaVersion()-1)
	installIndex(t, adrDB, sqlite.SchemaVersion()-1)

	stdout, _, code := run(t, "migrate", "status")
	if code != 1 || !strings.Contains(stdout, "store default") || strings.Contains(stdout, "store adr") {
		t.Errorf("default scope: exit %d\n%s", code, stdout)
	}
	stdout, _, _ = run(t, "--store", "adr", "migrate", "status")
	if !strings.Contains(stdout, "store adr") || strings.Contains(stdout, "store default") {
		t.Errorf("--store adr:\n%s", stdout)
	}
	if _, stderr, code := run(t, "migrate", "up", "--all", "--store", "adr"); code != 1 || !strings.Contains(stderr, "cannot be combined") {
		t.Errorf("--all with --store: exit %d %q", code, stderr)
	}
	if _, _, code := run(t, "--store", "nope", "migrate", "status"); code != 1 {
		t.Errorf("unknown store: exit %d", code)
	}
	stdout, _, code = run(t, "migrate", "up", "--all")
	if code != 0 || strings.Count(stdout, "migrated store ") != 2 {
		t.Errorf("up --all: exit %d\n%s", code, stdout)
	}
	for _, db := range []string{testDBPath(root), adrDB} {
		if st, _ := sqlite.NewArtefact(db).Status(context.Background()); st.Current != sqlite.SchemaVersion() || len(backupFiles(t, db)) != 1 {
			t.Errorf("%s: %+v, backups %v", db, st, backupFiles(t, db))
		}
	}

	empty := t.TempDir()
	t.Chdir(empty)
	if _, stderr, code := run(t, "migrate", "status"); code != 1 || !strings.Contains(stderr, "kb init") {
		t.Errorf("no location: exit %d %q", code, stderr)
	}
}

// TestMigrateStatusOutput pins the status table and its JSON shape.
func TestMigrateStatusOutput(t *testing.T) {
	root := newTempRoot(t)
	useNoneModel(t, root)
	installIndex(t, testDBPath(root), sqlite.SchemaVersion()-1)
	norm := func(s string) string { return strings.ReplaceAll(s, root, "<root>") }

	stdout, _, code := run(t, "migrate", "status")
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	cur := sqlite.SchemaVersion()
	st, err := sqlite.NewArtefact(testDBPath(root)).Status(context.Background())
	if err != nil || len(st.Pending) != 1 {
		t.Fatalf("status of the fixture: %+v, %v", st, err)
	}
	step := st.Pending[0]
	want := fmt.Sprintf(`SCOPE          FILE      VERSION   STATE                         PATH
location       location  v1        current                       <root>/.kb/kb.toml
store default  config    v1        current                       <root>/.kb/stores/default/config.toml
store default  index     v%d -> v%d  1 pending: run kb migrate up  <root>/.kb/stores/default/kb.sqlite

store default index:
  %s  %s
`, cur-1, cur, step.ID, step.Desc)
	if got := norm(stdout); got != want {
		t.Errorf("status output:\n%s\nwant:\n%s", got, want)
	}

	stdout, _, _ = run(t, "migrate", "status", "--json")
	var doc struct {
		Artefacts []migrateRow `json:"artefacts"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("json: %v\n%s", err, stdout)
	}
	if len(doc.Artefacts) != 3 {
		t.Fatalf("%d artefacts, want 3: %+v", len(doc.Artefacts), doc.Artefacts)
	}
	idx := doc.Artefacts[2]
	if idx.Artefact != "index" || idx.State != "pending" || idx.Current != cur-1 || idx.Target != cur || len(idx.Pending) != 1 || idx.Pending[0].ID == "" || idx.Pending[0].Effect == "" {
		t.Errorf("index row = %+v", idx)
	}
}

// TestMigrateStatusShowsCache: the embedding cache is listed for information, migrates by itself
// and never makes status fail.
func TestMigrateStatusShowsCache(t *testing.T) {
	root := newTempRoot(t)
	useNoneModel(t, root)
	indexState(t, root, "current")
	path := filepath.Join(t.TempDir(), "embed-cache.sqlite")
	old := testDeps.embedCachePath
	testDeps.embedCachePath = func() (string, error) { return path, nil }
	t.Cleanup(func() { testDeps.embedCachePath = old })

	c, err := cache.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	stdout, _, code := run(t, "migrate", "status")
	if code != 0 || !strings.Contains(stdout, "embed-cache") || !strings.Contains(stdout, "current (migrates automatically)") {
		t.Errorf("exit %d\n%s", code, stdout)
	}
	// Even a cache from a newer kb does not fail status or up: it degrades to no cache by itself.
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(fmt.Sprintf("PRAGMA user_version = %d", cache.SchemaVersion+1))
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if stdout, _, code := run(t, "migrate", "status"); code != 0 || !strings.Contains(stdout, "newer") {
		t.Errorf("newer cache: exit %d\n%s", code, stdout)
	}
	if stdout, _, code := run(t, "migrate", "up"); code != 0 || !strings.Contains(stdout, "everything is up to date") {
		t.Errorf("up with a newer cache: exit %d\n%s", code, stdout)
	}
}

// TestInitCreatesCurrentIndexThatNeedsNoMigration: a store made by kb init is at the target, so
// status is clean and every command works at once.
func TestInitCreatesCurrentIndexThatNeedsNoMigration(t *testing.T) {
	root := realTempDir(t)
	t.Chdir(root)
	withHome(t, realTempDir(t))
	if _, stderr, code := run(t, "init", "--model", "none"); code != 0 {
		t.Fatalf("init: exit %d %q", code, stderr)
	}
	stdout, _, code := run(t, "migrate", "status")
	if code != 0 || strings.Contains(stdout, "pending") {
		t.Errorf("status after init: exit %d\n%s", code, stdout)
	}
	if _, stderr, code := run(t, "search", "x", "--mode", "fts"); code != 0 {
		t.Errorf("search after init: exit %d %q", code, stderr)
	}
}

// TestMigrateAllCoversGlobalLocation: --all reaches the global location as well, and --store global
// names only it.
func TestMigrateAllCoversGlobalLocation(t *testing.T) {
	root := newTempRoot(t)
	useNoneModel(t, root)
	home := realTempDir(t)
	withHome(t, home)
	globalRoot := filepath.Join(home, ".local", "share", "kb")
	makeLocation(t, globalRoot, "default")
	globalDB := filepath.Join(globalRoot, ".kb", "stores", "default", "kb.sqlite")
	installIndex(t, globalDB, sqlite.SchemaVersion()-1)
	indexState(t, root, "current")

	stdout, _, code := run(t, "migrate", "status", "--all")
	if code != 1 || !strings.Contains(stdout, globalDB) || !strings.Contains(stdout, testDBPath(root)) {
		t.Errorf("status --all: exit %d, want both locations and exit 1\n%s", code, stdout)
	}
	// Without --all the global location is not touched.
	if stdout, _, code := run(t, "migrate", "status"); code != 0 || strings.Contains(stdout, globalDB) {
		t.Errorf("status without --all: exit %d\n%s", code, stdout)
	}
	stdout, _, code = run(t, "--store", "global", "migrate", "status")
	if code != 1 || !strings.Contains(stdout, globalDB) || strings.Contains(stdout, testDBPath(root)) {
		t.Errorf("--store global: exit %d\n%s", code, stdout)
	}
	if _, stderr, code := run(t, "--root", root, "--store", "global", "migrate", "status"); code != 1 || !strings.Contains(stderr, "cannot be combined") {
		t.Errorf("--store global with --root: exit %d %q", code, stderr)
	}
	if stdout, _, code := run(t, "migrate", "up", "--all"); code != 0 || strings.Count(stdout, "migrated store ") != 1 {
		t.Errorf("up --all: exit %d\n%s", code, stdout)
	}
	if st, _ := sqlite.NewArtefact(globalDB).Status(context.Background()); st.Current != sqlite.SchemaVersion() {
		t.Errorf("the global store was not migrated: %+v", st)
	}
	// Run from the global location itself, --all lists it once.
	if stdout, _, _ := run(t, "--global", "migrate", "status", "--all"); strings.Count(stdout, globalDB) != 1 {
		t.Errorf("--global status --all lists the global store %d times\n%s", strings.Count(stdout, globalDB), stdout)
	}
}

// TestMigrateReadsAnOlderLocationFile: the location file is read without the strict reader, so
// default_store is found even when Parse would refuse the file (an older or newer kb's keys), a missing key means "default", and a broken file is
// a usage error that names it.
func TestMigrateReadsAnOlderLocationFile(t *testing.T) {
	root := newTempRoot(t)
	useNoneModel(t, root)
	addStore(t, root, "adr")
	kbToml := filepath.Join(root, ".kb", "kb.toml")
	for _, tc := range []struct {
		name, body, want string
		code             int
	}{
		{"a key this kb does not know (only Parse is strict)", "version = 1\ndefault_store = \"adr\"\nfuture_key = 1\n", "store adr", 0},
		{"no default_store means default", "version = 1\n", "store default", 0},
		{"broken toml", "version = = 1\n", "kb.toml", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeFile(t, kbToml, tc.body)
			stdout, stderr, code := run(t, "migrate", "status")
			if tc.code == 1 && (code != 1 || !strings.Contains(stderr, tc.want)) {
				t.Fatalf("exit %d %q, want exit 1 naming %s", code, stderr, tc.want)
			}
			if tc.code == 0 && !strings.Contains(stdout, tc.want) {
				t.Errorf("exit %d\n%s\nwant %q", code, stdout, tc.want)
			}
		})
	}
}

// TestMigrateVerifyFindsDrift: verify exits 1 on a damaged file, naming it, and on a config the
// parser would refuse; a good store passes. Verify runs on an older file too (its schema is
// compared at its own version).
func TestMigrateVerifyFindsDrift(t *testing.T) {
	root := newTempRoot(t)
	useNoneModel(t, root)
	indexState(t, root, "current")
	if stdout, _, code := run(t, "migrate", "verify"); code != 0 || !strings.Contains(stdout, "ok   store default config.toml") || !strings.Contains(stdout, "ok   location kb.toml") {
		t.Fatalf("verify of a good store: exit %d\n%s", code, stdout)
	}
	db, err := sql.Open("sqlite3", testDBPath(root))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("DROP TABLE search_opens")
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := run(t, "migrate", "verify")
	if code != 1 || !strings.Contains(stdout, "FAIL store default kb.sqlite") || !strings.Contains(stdout, "search_opens") || !strings.Contains(stderr, "failed verification") {
		t.Errorf("verify of a drifted index: exit %d\n%s\n%s", code, stdout, stderr)
	}
	writeConfig(t, root, "bogus = 1\n")
	if stdout, _, code := run(t, "migrate", "verify"); code != 1 || !strings.Contains(stdout, "FAIL store default config.toml") {
		t.Errorf("verify of a bad config: exit %d\n%s", code, stdout)
	}
}

// TestMigrateInterrupted: a cancelled context is reported as an interrupt (exit 1), not a failure
// of one file, and the store is left as it was.
func TestMigrateInterrupted(t *testing.T) {
	root := newTempRoot(t)
	useNoneModel(t, root)
	installIndex(t, testDBPath(root), sqlite.SchemaVersion()-1)
	before := fileDigest(t, testDBPath(root))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, sub := range []string{"status", "up", "verify"} {
		var out, errBuf strings.Builder
		code := execute(ctx, testDeps, []string{"migrate", sub}, &out, &errBuf)
		if code != ExitUsage {
			t.Errorf("migrate %s after cancel: exit %d, stderr %q", sub, code, errBuf.String())
		}
	}
	if fileDigest(t, testDBPath(root)) != before || len(backupFiles(t, testDBPath(root))) != 0 {
		t.Error("a cancelled migrate changed the store or left a backup")
	}
}

// TestMigrateScopeMatchesStoreRefs: kb migrate resolves --store like every other command (W7 pass:
// it used to read only the NAME, so `kb --store adr@OTHER migrate up` migrated the current
// location's adr). For each ref form, with and without --root, the store migrate lists is the one
// `kb config --path` resolves, and the location row is that store's kb.toml; a ref the other
// commands refuse, migrate refuses with the same exit code.
func TestMigrateScopeMatchesStoreRefs(t *testing.T) {
	w := newRefWorld(t)
	t.Chdir(w.loc)
	for _, args := range [][]string{
		nil,
		{"--store", "adr"},
		{"--store", "al"},
		{"--store", "adr@" + w.other},
		{"--store", w.other},
		{"--store", "global"},
		{"--store", "nope"},
		{"--store", "broken"},
		{"--root", w.other},
		{"--root", w.other, "--store", "adr"},
		{"--root", w.other, "--store", "al"},
		{"--root", w.other, "--store", "adr@" + w.loc},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			cfgOut, _, cfgCode := run(t, append(args, "config", "--path")...)
			out, stderr, code := run(t, append(args, "migrate", "status", "--json")...)
			if cfgCode != 0 {
				if code != cfgCode || stderr == "" {
					t.Fatalf("config --path exits %d; migrate exits %d %q", cfgCode, code, stderr)
				}
				return
			}
			var doc struct {
				Artefacts []migrateRow `json:"artefacts"`
			}
			if err := json.Unmarshal([]byte(out), &doc); err != nil {
				t.Fatalf("migrate status: exit %d %q: %v", code, stderr, err)
			}
			cfgPath := strings.TrimSpace(cfgOut)
			kbToml := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(cfgPath))), "kb.toml")
			var paths []string
			for _, r := range doc.Artefacts {
				if r.Scope != "cache" {
					paths = append(paths, r.Path)
				}
			}
			if len(paths) != 3 || paths[0] != kbToml || paths[1] != cfgPath {
				t.Errorf("migrate lists %v; want %s, %s and its index", paths, kbToml, cfgPath)
			}
		})
	}
}

// TestEditRefusesIndexStateBeforeEditor: on an older or newer index kb edit is refused before the
// editor runs, so the file is never changed and left unindexed (W7 pass).
func TestEditRefusesIndexStateBeforeEditor(t *testing.T) {
	for _, tc := range []struct{ state, want string }{{"older", "Run: kb migrate up"}, {"newer", "use a newer kb"}} {
		t.Run(tc.state, func(t *testing.T) {
			root := newTempRoot(t)
			useNoneModel(t, root)
			indexState(t, root, tc.state)
			file := filepath.Join(root, "data", "e.md")
			writeFile(t, file, validFrontMatter("E", "e"))
			orig := testDeps.editor
			t.Cleanup(func() { testDeps.editor = orig })
			ran := false
			testDeps.editor = func(_ context.Context, _, path string) error {
				ran = true
				return os.WriteFile(path, []byte(validFrontMatter("E", "changed")), 0o644)
			}
			_, stderr, code := run(t, "edit", "e.md")
			if code != ExitUsage || !strings.Contains(stderr, tc.want) || ran {
				t.Fatalf("edit on a %s index: exit %d %q, editor ran %v", tc.state, code, stderr, ran)
			}
			if got := readFileT(t, file); got != validFrontMatter("E", "e") {
				t.Errorf("the file changed: %q", got)
			}
		})
	}
}

// TestMigrateScopeErrors: a malformed --store is Usage naming it (not ignored), and --root at a
// directory with no kb.toml says to run kb init.
func TestMigrateScopeErrors(t *testing.T) {
	newTempRoot(t)
	if _, stderr, code := run(t, "--store", "Bad", "migrate", "status"); code != ExitUsage || !strings.Contains(stderr, "Bad") {
		t.Errorf("malformed --store: exit %d %q", code, stderr)
	}
	empty := realTempDir(t)
	if _, stderr, code := run(t, "--root", empty, "migrate", "status"); code != ExitUsage || !strings.Contains(stderr, "kb init") {
		t.Errorf("--root without kb.toml: exit %d %q", code, stderr)
	}
}
