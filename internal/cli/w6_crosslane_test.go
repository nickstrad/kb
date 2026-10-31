package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/fake"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/store/sqlite"
)

// Tests where the W6 lanes meet: kb init (lane A) and the migration framework (lane C), kb stores
// and doctor on databases of another schema version, and [search] settings (lane B) per named
// store.

// TestInitIndexIsAtMigrateTarget: kb init creates the store's database through the same steps the
// migrate framework applies, so the new file is at the index artefact's target with nothing
// pending, passes Verify (integrity and the schema of a fresh database), and records the
// embedder exactly.
func TestInitIndexIsAtMigrateTarget(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"default", "adr"} {
		t.Run(name, func(t *testing.T) {
			root := realTempDir(t)
			chdir(t, root)
			withHome(t, realTempDir(t))
			args := []string{"init", "--model", "none"}
			if name != "default" {
				args = append(args, "--name", name)
			}
			if _, stderr, code := run(t, args...); code != 0 {
				t.Fatalf("init: exit %d %q", code, stderr)
			}
			db := filepath.Join(root, ".kb", "stores", name, "kb.sqlite")
			ctx := context.Background()
			art := sqlite.NewArtefact(db)
			st, err := art.Status(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if st.Current != sqlite.SchemaVersion() || st.Target != sqlite.SchemaVersion() || len(st.Pending) != 0 || st.Newer() {
				t.Fatalf("status after init: %+v, want current = target = %d, nothing pending", st, sqlite.SchemaVersion())
			}
			if err := art.Verify(ctx); err != nil {
				t.Fatalf("Verify after init: %v", err)
			}
			s, err := sqlite.Open(ctx, db, sqlite.Options{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			m, ok, err := s.EmbedMeta(ctx)
			if err != nil || !ok || m.Model != store.FTSOnlyModel || m.Dim != 0 {
				t.Fatalf("embed_meta after init --model none = %+v, %v, %v; want %q dim 0", m, ok, err, store.FTSOnlyModel)
			}
		})
	}
}

// setUserVersion rewrites a database's PRAGMA user_version, to fake one written by another kb.
func setUserVersion(t *testing.T, path string, v int) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
		t.Fatal(err)
	}
}

// TestStoresAndDoctorOnOtherSchemaVersions: kb stores opens each database read-only, so an older
// or newer schema is a problem in that store's row (with the fix), never an abort and never a
// write; kb doctor reports a newer database as a FAIL and leaves it alone.
func TestStoresAndDoctorOnOtherSchemaVersions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		probe   bool // the kb is a probe build (one release ahead), so this build's version is older
		version int
		problem string
	}{
		{"older", true, sqlite.SchemaVersion(), "Run: kb migrate up"},
		{"newer", false, sqlite.SchemaVersion() + 1, "use a newer kb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newTempRoot(t)
			useNoneModel(t, root)
			if tc.probe {
				useProbeBuild(t)
			}
			addStore(t, root, "adr")
			ctx := context.Background()
			for _, s := range []string{"default", "adr"} {
				st, err := sqlite.Open(ctx, filepath.Join(root, ".kb", "stores", s, "kb.sqlite"), sqlite.Options{Steps: depsFor(t).indexSteps})
				if err != nil {
					t.Fatal(err)
				}
				st.Close()
			}
			setUserVersion(t, testDBPath(root), tc.version)

			stdout, stderr, code := run(t, "stores", "--json")
			if code != 0 || stderr != "" {
				t.Fatalf("kb stores: exit %d stderr %q", code, stderr)
			}
			var doc storesJSON
			if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
				t.Fatal(err)
			}
			adr, def := doc.Stores[0], doc.Stores[1]
			if !strings.Contains(def.Problem, tc.problem) || def.Entries != nil || !def.Indexed {
				t.Errorf("default row: %+v; want the problem %q", def, tc.problem)
			}
			if adr.Problem != "" || adr.Entries == nil {
				t.Errorf("adr row (a current database) should be unaffected: %+v", adr)
			}
			if v := sqlite.NewArtefact(testDBPath(root)); true {
				st, err := v.Status(ctx)
				if err != nil || st.Current != tc.version {
					t.Errorf("kb stores changed the database version: %+v %v", st, err)
				}
			}
			// doctor never migrates: a newer database is a driver FAIL, an older one is its own
			// migrate FAIL naming the command, and in both the file is left as it was.
			out, _, code := run(t, "doctor")
			check := "FAIL driver: "
			if tc.name == "older" {
				check = "FAIL migrate: "
				tc.problem = "run kb migrate up"
			}
			if code != ExitUsage || !strings.Contains(out, check) || !strings.Contains(out, tc.problem) {
				t.Errorf("doctor on a %s database: exit %d, want %q and %q\n%s", tc.name, code, check, tc.problem, out)
			}
			if st, err := sqlite.NewArtefact(testDBPath(root)).Status(ctx); err != nil || st.Current != tc.version {
				t.Errorf("kb doctor changed the database version: %+v %v", st, err)
			}
		})
	}
}

// TestDoctorDriverFailsOnGarbageDatabase: a database the driver cannot open is one FAIL line
// (driver) and a failing exit, and the other checks still run.
func TestDoctorDriverFailsOnGarbageDatabase(t *testing.T) {
	t.Parallel()
	root := newTempRoot(t)
	useNoneModel(t, root)
	writeFile(t, testDBPath(root), "this is not a sqlite database, just text that is long enough to have a header")
	out, _, code := run(t, "doctor")
	if code != ExitUsage || !strings.Contains(out, "FAIL driver: ") || !strings.Contains(out, "ok   config: ") || !strings.Contains(out, "doctor: 1 failed") {
		t.Fatalf("doctor on a garbage database: exit %d\n%s", code, out)
	}
}

// doctorMetaIndex is a store.Index whose EmbedMeta and Entries are fixed; checkEmbedMeta reads nothing
// else.
type doctorMetaIndex struct {
	store.Index
	meta    store.EmbedMeta
	ok      bool
	entries int
}

func (m doctorMetaIndex) EmbedMeta(context.Context) (store.EmbedMeta, bool, error) {
	return m.meta, m.ok, nil
}

func (m doctorMetaIndex) Entries(context.Context) ([]store.EntryRow, error) {
	return make([]store.EntryRow, m.entries), nil
}

// zeroDim is an embedder that claims 0 dims under a real model name: the shape of an embed_meta
// row no real model can write.
type zeroDim struct{ embed.Embedder }

func (zeroDim) Dim() int { return 0 }

// TestCheckEmbedMetaDecisions: one row per outcome of doctor's embed_meta check. A row matches
// the configured embedder only when model and dim match and the recorded width is storable
// (1..MaxVecDim, or 0 for the FTS-only "none").
func TestCheckEmbedMetaDecisions(t *testing.T) {
	t.Parallel()
	m4 := fake.New("m", 4)
	wide := fake.New("w", sqlite.MaxVecDim+1)
	maxw := fake.New("w", sqlite.MaxVecDim)
	for _, tc := range []struct {
		name string
		idx  doctorMetaIndex
		e    embed.Embedder
		want string
	}{
		{"unset, empty database", doctorMetaIndex{}, m4, "FAIL embed_meta: not set (empty database)"},
		{"unset, one entry", doctorMetaIndex{entries: 1}, m4, "FAIL embed_meta: not set, but 1 entry is indexed"},
		{"unset, two entries", doctorMetaIndex{entries: 2}, m4, "FAIL embed_meta: not set, but 2 entries are indexed"},
		{"matches", doctorMetaIndex{meta: store.EmbedMeta{Model: "m", Dim: 4}, ok: true}, m4, "ok   embed_meta: model=m dim=4"},
		{"model differs", doctorMetaIndex{meta: store.EmbedMeta{Model: "x", Dim: 4}, ok: true}, m4, "FAIL embed_meta: database has x (4 dims)"},
		{"dim differs", doctorMetaIndex{meta: store.EmbedMeta{Model: "m", Dim: 8}, ok: true}, m4, "FAIL embed_meta: database has m (8 dims)"},
		{"dim 1 is storable", doctorMetaIndex{meta: store.EmbedMeta{Model: "one", Dim: 1}, ok: true}, fake.New("one", 1), "ok   embed_meta: model=one dim=1"},
		{"at MaxVecDim is storable", doctorMetaIndex{meta: store.EmbedMeta{Model: "w", Dim: sqlite.MaxVecDim}, ok: true}, maxw, "ok   embed_meta: model=w"},
		{"over MaxVecDim is not storable", doctorMetaIndex{meta: store.EmbedMeta{Model: "w", Dim: sqlite.MaxVecDim + 1}, ok: true}, wide, "FAIL embed_meta: database has w"},
		{"none at dim 0", doctorMetaIndex{meta: store.EmbedMeta{Model: store.FTSOnlyModel, Dim: 0}, ok: true}, embed.None{}, "ok   embed_meta: model=none dim=0"},
		{"dim 0 for a real model is not storable", doctorMetaIndex{meta: store.EmbedMeta{Model: "z", Dim: 0}, ok: true}, zeroDim{fake.New("z", 1)}, "FAIL embed_meta: database has z (0 dims)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &doctorState{}
			checkEmbedMeta(context.Background(), d, tc.idx, tc.e)
			wantCheck(t, tc.name, d, tc.want)
		})
	}
}

// TestModelCommandsNeedTheNamedStore: inside a location, kb model pull/path read the resolved
// store's [embed]; a --store or default_store that names no store is NotFound (exit 1), not a
// silent fallback to the default model.
func TestModelCommandsNeedTheNamedStore(t *testing.T) {
	t.Parallel()
	cache := t.TempDir()
	orig := depsFor(t).modelCacheDir
	t.Cleanup(func() { depsFor(t).modelCacheDir = orig })
	depsFor(t).modelCacheDir = func() (string, error) { return cache, nil }
	root := newTempRoot(t)
	addStore(t, root, "adr")
	writeFile(t, filepath.Join(root, ".kb", "stores", "adr", "config.toml"), "[embed]\nmodel = \"none\"\n"+dataSourceTOML)

	for _, sub := range []string{"pull", "path"} {
		if _, stderr, code := run(t, "--store", "nope", "model", sub); code != ExitUsage || !strings.Contains(stderr, `no store "nope"`) {
			t.Errorf("model %s --store nope: exit %d %q", sub, code, stderr)
		}
		// adr is FTS-only: its config, not the default model, decides.
		if _, stderr, code := run(t, "--store", "adr", "model", sub); code != ExitUsage || !strings.Contains(stderr, "full-text search only") {
			t.Errorf("model %s --store adr: exit %d %q", sub, code, stderr)
		}
	}
	writeFile(t, filepath.Join(root, ".kb", "kb.toml"), "version = 1\ndefault_store = \"ghost\"\n")
	if _, stderr, code := run(t, "model", "path"); code != ExitUsage || !strings.Contains(stderr, `default_store "ghost"`) {
		t.Errorf("model path with a dangling default_store: exit %d %q", code, stderr)
	}
}

// TestSearchSettingsArePerStore: each named store has its own [search]. The same corpus searched
// through a store with rrf_k = 1 and through one with the defaults gives the two scores.
func TestSearchSettingsArePerStore(t *testing.T) {
	t.Parallel()
	root := newTempRoot(t)
	useNoneModel(t, root)
	addStore(t, root, "adr")
	writeFile(t, filepath.Join(root, ".kb", "stores", "adr", "config.toml"), "[embed]\nmodel = \"none\"\n[search]\nrrf_k = 1\n"+dataSourceTOML)
	src := filepath.Join(t.TempDir(), "widget.md")
	writeFile(t, src, validFrontMatter("Widget", "About the widget."))
	if _, stderr, code := run(t, "add", src); code != 0 {
		t.Fatalf("add: %d %q", code, stderr)
	}
	if _, stderr, code := run(t, "--store", "adr", "reindex", "--all"); code != 0 {
		t.Fatalf("reindex adr: %d %q", code, stderr)
	}
	for _, tc := range []struct {
		args  []string
		score string
	}{
		{[]string{"search", "widget", "--mode", "fts", "--json"}, `"rrf_score": 0.01639`},
		{[]string{"--store", "adr", "search", "widget", "--mode", "fts", "--json"}, `"rrf_score": 0.5,`},
	} {
		stdout, stderr, code := run(t, tc.args...)
		if code != 0 || !strings.Contains(stdout, tc.score) {
			t.Errorf("kb %s: exit %d stderr %q; want a top score %q in\n%q", strings.Join(tc.args, " "), code, stderr, tc.score, stdout)
		}
	}
}
