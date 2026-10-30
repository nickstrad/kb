package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/fake"
	"github.com/nickstrad/kb/internal/embed/local"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/store/sqlite"
	"github.com/nickstrad/kb/internal/workspace"
)

// withHome makes home the user's home directory for root discovery and the init guard.
func withHome(t *testing.T, home string) {
	t.Helper()
	orig := testDeps.workspaceEnv
	t.Cleanup(func() { testDeps.workspaceEnv = orig })
	testDeps.workspaceEnv = func() workspace.Env {
		e := orig()
		e.Home = home
		return e
	}
}

func readFileT(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// TestInitDecisions is the decision table of kb init: {no location, a location without the store,
// the store exists} x {--name given, not} x {target is $HOME, other} x {--force}.
func TestInitDecisions(t *testing.T) {
	for _, state := range []string{"no location", "location without the store", "store exists"} {
		for _, named := range []bool{false, true} {
			for _, home := range []bool{false, true} {
				for _, force := range []bool{false, true} {
					name := state + "/named=" + map[bool]string{false: "no", true: "yes"}[named] +
						"/target=" + map[bool]string{false: "other", true: "home"}[home] +
						"/force=" + map[bool]string{false: "no", true: "yes"}[force]
					t.Run(name, func(t *testing.T) {
						root := realTempDir(t)
						t.Chdir(root)
						if home {
							withHome(t, root)
						} else {
							withHome(t, realTempDir(t))
						}
						storeName := "default"
						args := []string{"init", "--model", "none"}
						if named {
							storeName = "adr"
							args = append(args, "--name", storeName)
						}
						if force {
							args = append(args, "--force")
						}
						var kbToml string
						switch state {
						case "location without the store":
							makeLocation(t, root, "other") // default_store = other, store other only
							kbToml = readFileT(t, filepath.Join(root, ".kb", "kb.toml"))
						case "store exists":
							pre := []string{"init", "--model", "none", "--force"}
							if named {
								pre = append(pre, "--name", storeName)
							}
							if _, errOut, code := run(t, pre...); code != 0 {
								t.Fatalf("setup init: %d %q", code, errOut)
							}
							kbToml = readFileT(t, filepath.Join(root, ".kb", "kb.toml"))
						}
						cfgPath := filepath.Join(root, ".kb", "stores", storeName, "config.toml")
						var cfgBefore string
						if state == "store exists" {
							cfgBefore = readFileT(t, cfgPath)
						}

						stdout, stderr, code := run(t, args...)

						switch {
						case state == "no location" && home && !force:
							if code != ExitUsage || !strings.Contains(stderr, "refusing to create a kb location at") || !strings.Contains(stderr, "--force") {
								t.Fatalf("exit %d stderr %q; want the home refusal", code, stderr)
							}
							if exists(filepath.Join(root, ".kb")) || exists(filepath.Join(root, "data")) {
								t.Fatal("a refused init created files")
							}
							return
						case code != 0:
							t.Fatalf("exit %d stderr %q stdout %q", code, stderr, stdout)
						}
						switch state {
						case "no location":
							if !strings.Contains(stdout, "created location") || !strings.Contains(stdout, `created store "`+storeName+`"`) {
								t.Errorf("stdout %q", stdout)
							}
							loc, err := config.ReadLocation(filepath.Join(root, ".kb", "kb.toml"))
							if err != nil || loc.DefaultStore != storeName {
								t.Errorf("kb.toml: %+v %v; the first store becomes default_store", loc, err)
							}
							if gi := readFileT(t, filepath.Join(root, ".kb", ".gitignore")); gi != gitignoreBody {
								t.Errorf(".gitignore %q", gi)
							}
						case "location without the store":
							if strings.Contains(stdout, "created location") || !strings.Contains(stdout, `created store "`+storeName+`"`) {
								t.Errorf("stdout %q", stdout)
							}
							if got := readFileT(t, filepath.Join(root, ".kb", "kb.toml")); got != kbToml {
								t.Errorf("kb.toml was rewritten:\n%s\nwas:\n%s", got, kbToml)
							}
						case "store exists":
							if !strings.Contains(stdout, "already initialised") {
								t.Errorf("stdout %q", stdout)
							}
							if readFileT(t, cfgPath) != cfgBefore || readFileT(t, filepath.Join(root, ".kb", "kb.toml")) != kbToml {
								t.Error("an existing store was rewritten")
							}
							return
						}
						// A created store: config parses, the data dir and the database exist,
						// and the embedder is recorded.
						cfg, err := config.Load(cfgPath)
						if err != nil || cfg.Embed.Model != "none" {
							t.Errorf("config: %+v %v", cfg.Embed, err)
						}
						if fi, err := os.Stat(filepath.Join(root, "data")); err != nil || !fi.IsDir() {
							t.Errorf("data/ not created: %v", err)
						}
						st, err := sqlite.Open(context.Background(), filepath.Join(root, ".kb", "stores", storeName, "kb.sqlite"), sqlite.Options{ReadOnly: true})
						if err != nil {
							t.Fatalf("database not created: %v", err)
						}
						defer st.Close()
						if m, ok, err := st.EmbedMeta(context.Background()); err != nil || !ok || m.Model != store.FTSOnlyModel || m.Dim != 0 {
							t.Errorf("embed_meta %+v %v %v", m, ok, err)
						}
					})
				}
			}
		}
	}
}

func TestInitRefusesRoot(t *testing.T) {
	// "/" is refused without --force. Never run with --force here: it would write to /.kb.
	t.Chdir(realTempDir(t))
	_, stderr, code := run(t, "init", "--root", "/", "--model", "none")
	if code != ExitUsage || !strings.Contains(stderr, "refusing to create a kb location at /") {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	if exists("/.kb") {
		t.Fatal("init created /.kb")
	}
}

func TestIsHomeOrRoot(t *testing.T) {
	base := realTempDir(t)
	link := filepath.Join(base, "link")
	if err := os.Symlink(base, link); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, root, home string
		want             bool
	}{
		{"filesystem root", "/", "/home/x", true},
		{"home", base, base, true},
		{"home via a symlink", link, base, true},
		{"a child of home", filepath.Join(base, "p"), base, false},
		{"unknown home", base, "", false},
		{"a nonexistent path equal to home", "/no/such/home", "/no/such/home", true},
	} {
		if got := isHomeOrRoot(tc.root, tc.home); got != tc.want {
			t.Errorf("%s: isHomeOrRoot(%q, %q) = %v, want %v", tc.name, tc.root, tc.home, got, tc.want)
		}
	}
}

func TestInitThenListIsEmptyNotAnError(t *testing.T) {
	t.Chdir(realTempDir(t))
	if _, errOut, code := run(t, "init", "--model", "none"); code != 0 {
		t.Fatalf("init: %d %q", code, errOut)
	}
	out, errOut, code := run(t, "list")
	if code != 0 || strings.TrimSpace(out) != "no entries" || errOut != "" {
		t.Fatalf("list on an empty store: %d %q %q", code, out, errOut)
	}
	// And without data/ at all (a clone that has not made it).
	if err := os.Remove("data"); err != nil {
		t.Fatal(err)
	}
	if out, _, code := run(t, "list"); code != 0 || strings.TrimSpace(out) != "no entries" {
		t.Fatalf("list without data/: %d %q", code, out)
	}
}

func TestInitTwiceAndSecondStoreKeepsDefault(t *testing.T) {
	root := realTempDir(t)
	t.Chdir(root)
	run1, _, c1 := run(t, "init", "--model", "none")
	if c1 != 0 || !strings.Contains(run1, "created location") {
		t.Fatalf("first init: %d %q", c1, run1)
	}
	toml1 := readFileT(t, filepath.Join(root, ".kb", "kb.toml"))
	again, _, c2 := run(t, "init", "--model", "none")
	if c2 != 0 || !strings.Contains(again, `store "default" is already initialised`) {
		t.Fatalf("second init: %d %q", c2, again)
	}
	if _, errOut, code := run(t, "init", "--name", "adr", "--model", "none"); code != 0 {
		t.Fatalf("init --name adr: %d %q", code, errOut)
	}
	if got := readFileT(t, filepath.Join(root, ".kb", "kb.toml")); got != toml1 || !strings.Contains(got, `default_store = "default"`) {
		t.Fatalf("default_store changed:\n%s", got)
	}
	// The PLAN accept line: stores, list, --store adr list, doctor all exit 0.
	for _, args := range [][]string{{"stores"}, {"list"}, {"--store", "adr", "list"}, {"doctor"}, {"--store", "adr", "doctor"}} {
		if out, errOut, code := run(t, args...); code != 0 {
			t.Errorf("kb %v: exit %d stdout %q stderr %q", args, code, out, errOut)
		}
	}
	out, _, _ := run(t, "stores")
	if !strings.Contains(out, "default *") || !strings.Contains(out, "adr ") {
		t.Errorf("stores:\n%s", out)
	}
}

func TestInitInvalidNames(t *testing.T) {
	for _, name := range []string{"../x", "A", "global", strings.Repeat("a", 33), "", "a/b", "-x"} {
		t.Run("name="+name, func(t *testing.T) {
			root := realTempDir(t)
			t.Chdir(root)
			_, stderr, code := run(t, "init", "--model", "none", "--name", name)
			if code != ExitUsage || stderr == "" {
				t.Fatalf("exit %d stderr %q", code, stderr)
			}
			if exists(filepath.Join(root, ".kb")) {
				t.Fatal("an invalid name created files")
			}
		})
	}
}

func TestInitFlagRefusals(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"init", "--source", "docs/**/*.md"}, "--source is not supported yet"},
		{[]string{"--store", "adr", "init"}, "--name, not --store"},
		{[]string{"init", "--model", "acme/unknown"}, "unknown embedding model"},
		{[]string{"--root", "x", "--global", "init"}, "root"},
	} {
		root := realTempDir(t)
		t.Chdir(root)
		_, stderr, code := run(t, tc.args...)
		if code != ExitUsage || !strings.Contains(stderr, tc.want) {
			t.Errorf("%v: exit %d stderr %q, want exit 1 with %q", tc.args, code, stderr, tc.want)
		}
		if exists(filepath.Join(root, ".kb")) {
			t.Errorf("%v created files", tc.args)
		}
	}
}

func TestInitGlobalWritesUnderXDG(t *testing.T) {
	t.Chdir(realTempDir(t))
	globalRoot := globalLocationRoot()
	t.Cleanup(func() { os.RemoveAll(filepath.Join(testDeps.workspaceEnv().Home, ".local")) })
	if _, errOut, code := run(t, "--global", "init", "--model", "none"); code != 0 {
		t.Fatalf("init --global: %d %q", code, errOut)
	}
	if !exists(filepath.Join(globalRoot, ".kb", "kb.toml")) || !exists(filepath.Join(globalRoot, ".kb", "stores", "default", "kb.sqlite")) {
		t.Fatal("the global location was not created under the test home")
	}
	// The home guard does not apply to the global location (it is under home, not home).
	if out, _, code := run(t, "--global", "list"); code != 0 || strings.TrimSpace(out) != "no entries" {
		t.Fatalf("--global list: %d %q", code, out)
	}
}

// TestInitTargetIsCwdNotAnAncestor: a location in a parent does not absorb init in a child.
func TestInitTargetIsCwdNotAnAncestor(t *testing.T) {
	parent := newTempRoot(t)
	child := filepath.Join(parent, "sub", "project")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(child)
	if _, errOut, code := run(t, "init", "--model", "none"); code != 0 {
		t.Fatalf("init: %d %q", code, errOut)
	}
	if !exists(filepath.Join(child, ".kb", "kb.toml")) {
		t.Fatal("init did not create a location in the working directory")
	}
	if exists(filepath.Join(parent, ".kb", "stores", "default", "kb.sqlite")) {
		t.Fatal("init wrote into the ancestor location")
	}
}

func TestInitBrokenLocationFileIsReported(t *testing.T) {
	root := realTempDir(t)
	t.Chdir(root)
	writeFile(t, filepath.Join(root, ".kb", "kb.toml"), "colour = 1\n")
	_, stderr, code := run(t, "init", "--model", "none")
	if code != ExitUsage || !strings.Contains(stderr, `unknown key "colour"`) {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	if exists(filepath.Join(root, ".kb", "stores")) {
		t.Fatal("init wrote a store beside a broken kb.toml")
	}
}

func TestInitKeepsAnExistingGitignore(t *testing.T) {
	root := realTempDir(t)
	t.Chdir(root)
	writeFile(t, filepath.Join(root, ".kb", ".gitignore"), "mine\n")
	writeFile(t, filepath.Join(root, ".kb", "kb.toml"), "version = 1\n")
	if _, errOut, code := run(t, "init", "--model", "none"); code != 0 {
		t.Fatalf("init: %d %q", code, errOut)
	}
	if got := readFileT(t, filepath.Join(root, ".kb", ".gitignore")); got != "mine\n" {
		t.Fatalf(".gitignore overwritten: %q", got)
	}
}

// TestInitRecordsEmbedderAndLoadsModel: with a fake embedder the embed_meta row is its identity;
// with the real local embedder and no network the store is created but init exits 2.
func TestInitRecordsEmbedderAndLoadsModel(t *testing.T) {
	t.Run("fake embedder", func(t *testing.T) {
		useFakeEmbedder(t)
		root := realTempDir(t)
		t.Chdir(root)
		if _, errOut, code := run(t, "init"); code != 0 {
			t.Fatalf("init: %d %q", code, errOut)
		}
		st, err := sqlite.Open(context.Background(), filepath.Join(root, ".kb", "stores", "default", "kb.sqlite"), sqlite.Options{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		m, ok, err := st.EmbedMeta(context.Background())
		if err != nil || !ok || m.Model != "fake-add-test" || m.Dim != 16 {
			t.Fatalf("embed_meta %+v %v %v", m, ok, err)
		}
		if got := readFileT(t, filepath.Join(root, ".kb", "stores", "default", "config.toml")); !strings.Contains(got, `model = "`+local.DefaultModel+`"`) {
			t.Errorf("the default model is written explicitly:\n%s", got)
		}
	})
	t.Run("model cannot be downloaded", func(t *testing.T) {
		_, calls := stubPull(t, errors.New("offline"))
		root := realTempDir(t)
		t.Chdir(root)
		_, errOut, code := run(t, "init")
		if code != ExitEmbedder || !strings.Contains(errOut, "the store is created") || !strings.Contains(errOut, "kb model pull") {
			t.Fatalf("exit %d stderr %q; want exit 2 saying the store is created", code, errOut)
		}
		if len(*calls) != 1 {
			t.Errorf("pull calls %v, want one", *calls)
		}
		if !exists(filepath.Join(root, ".kb", "stores", "default", "config.toml")) {
			t.Error("the store should stay created")
		}
		if out, _, code := run(t, "init"); code != 0 || !strings.Contains(out, "already initialised") {
			t.Errorf("rerun: %d %q", code, out)
		}
	})
	t.Run("a missing model is downloaded with progress on stderr", func(t *testing.T) {
		// stubPull writes a stand-in model dir that rembed cannot load, so init ends in exit 2
		// after the download; what this checks is that the download happens, and is announced.
		_, calls := stubPull(t, nil)
		t.Chdir(realTempDir(t))
		_, errOut, _ := run(t, "init")
		if len(*calls) != 1 || !strings.Contains(errOut, "downloading embedding model") || !strings.Contains(errOut, "kb init: loading model") {
			t.Fatalf("stderr %q pulls %v", errOut, *calls)
		}
	})
}

func TestStoreConfigTextParses(t *testing.T) {
	for _, id := range append(local.Supported(), "none", "") {
		text, ecfg, err := storeConfigText(id)
		if err != nil {
			t.Errorf("model %q: %v", id, err)
			continue
		}
		want := id
		if id == "" {
			want = local.DefaultModel
		}
		if ecfg.Model != want {
			t.Errorf("model %q: embed model %q, want %q", id, ecfg.Model, want)
		}
		if _, err := config.Parse([]byte(text), "t"); err != nil {
			t.Errorf("model %q: generated config does not parse: %v\n%s", id, err, text)
		}
	}
	if _, _, err := storeConfigText(`x"y`); errs.KindOf(err) != errs.Usage {
		t.Errorf("a model that needs escaping: %v", err)
	}
	if got := quoteTOML(`a\b"c`); got != `"a\\b\"c"` {
		t.Errorf("quoteTOML = %s", got)
	}
}

func TestWriteNewRefusesExisting(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := writeNew(p, []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := writeNew(p, []byte("b")); !os.IsExist(err) {
		t.Fatalf("second writeNew: %v, want an exists error", err)
	}
	if readFileT(t, p) != "a" {
		t.Fatal("writeNew overwrote")
	}
}

func TestDoctorUnresolvedLocationAndStore(t *testing.T) {
	t.Run("no location", func(t *testing.T) {
		t.Chdir(realTempDir(t))
		out, _, code := run(t, "doctor")
		if code != ExitUsage || !strings.Contains(out, "FAIL location: not initialised: run kb init") || !strings.Contains(out, "doctor: 1 failed, 0 warnings") {
			t.Fatalf("exit %d out %q", code, out)
		}
	})
	t.Run("a broken kb.toml is not 'not initialised'", func(t *testing.T) {
		root := newTempRoot(t)
		writeFile(t, filepath.Join(root, ".kb", "kb.toml"), "colour = 1\n")
		out, _, code := run(t, "doctor")
		if code != ExitUsage || !strings.Contains(out, "FAIL location:") || strings.Contains(out, "not initialised") {
			t.Fatalf("exit %d out %q", code, out)
		}
	})
	t.Run("default_store names no store", func(t *testing.T) {
		root := newTempRoot(t)
		makeLocationFile(t, root, "ghost")
		out, _, code := run(t, "doctor")
		if code != ExitUsage || !strings.Contains(out, "FAIL store: ") || !strings.Contains(out, "kb.toml") {
			t.Fatalf("exit %d out %q", code, out)
		}
	})
}

func TestDoctorAfterInitChecks(t *testing.T) {
	root := realTempDir(t)
	t.Chdir(root)
	if _, errOut, code := run(t, "init", "--model", "none"); code != 0 {
		t.Fatalf("init: %d %q", code, errOut)
	}
	out, errOut, code := run(t, "doctor")
	if code != 0 {
		t.Fatalf("doctor: %d %q %q", code, out, errOut)
	}
	for _, want := range []string{"ok   config: ", "ok   sources: none configured", "ok   embed_meta: model=none dim=0", "ok   model: none: FTS-only", "doctor: 0 failed, 0 warnings"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output lacks %q:\n%s", want, out)
		}
	}
	// No data/: a warning, exit 0.
	if err := os.Remove(filepath.Join(root, "data")); err != nil {
		t.Fatal(err)
	}
	out, _, code = run(t, "doctor")
	if code != 0 || !strings.Contains(out, "warn entries: no ") || !strings.Contains(out, "doctor: 0 failed, 1 warnings") {
		t.Fatalf("doctor without data/: %d\n%s", code, out)
	}
}

// modelEmbedder is a model-like embedder: it can be pinged and embeds, so doctor reports timings.
type modelEmbedder struct {
	*fake.Embedder
	pingErr, embedErr error
	badShape          bool
}

func (p modelEmbedder) Ping(context.Context) error { return p.pingErr }
func (p modelEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if p.embedErr != nil {
		return nil, p.embedErr
	}
	if p.badShape {
		return [][]float32{{1}}, nil
	}
	return p.Embedder.Embed(ctx, texts)
}

func TestCheckEmbedderModelCheck(t *testing.T) {
	base := fake.New("m", 4)
	for _, tc := range []struct {
		name string
		e    modelEmbedder
		want string
	}{
		{"loads and embeds", modelEmbedder{Embedder: base}, "ok   model: loaded m in "},
		{"ping fails", modelEmbedder{Embedder: base, pingErr: errors.New("not downloaded")}, "FAIL model: not downloaded"},
		{"embed fails", modelEmbedder{Embedder: base, embedErr: errors.New("boom")}, `FAIL model: loaded m but embedding "ping" failed: boom`},
		{"wrong shape", modelEmbedder{Embedder: base, badShape: true}, `FAIL model: m returned the wrong shape for "ping"`},
	} {
		var buf strings.Builder
		d := &doctorState{stdout: &buf}
		checkEmbedder(context.Background(), d, tc.e)
		if !strings.Contains(buf.String(), tc.want) {
			t.Errorf("%s: %q lacks %q", tc.name, buf.String(), tc.want)
		}
	}
	var buf strings.Builder
	checkEmbedder(context.Background(), &doctorState{stdout: &buf}, base)
	if !strings.Contains(buf.String(), "warn model: not checked (test embedder)") {
		t.Errorf("non-pinger: %q", buf.String())
	}
}

func TestModelPathAndAlias(t *testing.T) {
	cache := t.TempDir()
	orig := testDeps.modelCacheDir
	t.Cleanup(func() { testDeps.modelCacheDir = orig })
	testDeps.modelCacheDir = func() (string, error) { return cache, nil }
	root := newTempRoot(t)
	out, _, code := run(t, "model", "path")
	want := filepath.Join(cache, "BAAI--bge-small-en-v1.5")
	if code != 0 || strings.TrimSpace(out) != want {
		t.Fatalf("model path: %d %q, want %s", code, out, want)
	}
	useNoneModel(t, root)
	if _, errOut, code := run(t, "model", "path"); code != ExitUsage || !strings.Contains(errOut, "no model directory") {
		t.Fatalf("model path with none: %d %q", code, errOut)
	}
	ls, _, c1 := run(t, "model", "ls")
	list, _, c2 := run(t, "model", "list")
	if c1 != 0 || c2 != 0 || ls != list || ls == "" {
		t.Fatalf("model ls differs from list: %q vs %q", ls, list)
	}
	// Outside any location the defaults apply, so the command works before kb init.
	t.Chdir(realTempDir(t))
	if out, _, code := run(t, "model", "path"); code != 0 || strings.TrimSpace(out) != want {
		t.Fatalf("model path outside a location: %d %q", code, out)
	}
}

func TestProductionModelCacheDirFollowsWorkspaceEnv(t *testing.T) {
	d := productionDeps()
	d.workspaceEnv = func() workspace.Env { return workspace.Env{CacheBase: "/c"} }
	if got, err := d.modelCacheDir(); err != nil || got != "/c/kb/models" {
		t.Fatalf("modelCacheDir = %q, %v", got, err)
	}
	d.workspaceEnv = func() workspace.Env { return workspace.Env{} }
	if _, err := d.modelCacheDir(); errs.KindOf(err) != errs.Usage {
		t.Fatalf("no cache base: %v", err)
	}
}

// TestInitFailures: filesystem and wiring failures are well-formed errors that name the path,
// never an exit 3, and leave no half-made store behind where it can be avoided.
func TestInitFailures(t *testing.T) {
	t.Run("unknown working directory", func(t *testing.T) {
		orig := testDeps.workspaceEnv
		t.Cleanup(func() { testDeps.workspaceEnv = orig })
		testDeps.workspaceEnv = func() workspace.Env { e := orig(); e.Cwd = ""; return e }
		_, stderr, code := run(t, "init", "--model", "none")
		if code != ExitUsage || !strings.Contains(stderr, "no working directory") {
			t.Fatalf("exit %d stderr %q", code, stderr)
		}
	})
	t.Run("data is a file", func(t *testing.T) {
		root := realTempDir(t)
		t.Chdir(root)
		writeFile(t, filepath.Join(root, "data"), "in the way")
		_, stderr, code := run(t, "init", "--model", "none")
		if code != ExitUsage || !strings.Contains(stderr, "create "+filepath.Join(root, "data")) {
			t.Fatalf("exit %d stderr %q", code, stderr)
		}
	})
	t.Run(".kb is a file", func(t *testing.T) {
		root := realTempDir(t)
		t.Chdir(root)
		writeFile(t, filepath.Join(root, ".kb"), "in the way")
		_, stderr, code := run(t, "init", "--model", "none")
		if code != ExitUsage || !strings.Contains(stderr, "create "+filepath.Join(root, ".kb")) {
			t.Fatalf("exit %d stderr %q", code, stderr)
		}
	})
	t.Run("stores is a file", func(t *testing.T) {
		root := realTempDir(t)
		t.Chdir(root)
		makeLocationFile(t, root, "default")
		writeFile(t, filepath.Join(root, ".kb", "stores"), "in the way")
		_, stderr, code := run(t, "init", "--model", "none")
		if code != ExitUsage || !strings.Contains(stderr, "create "+filepath.Join(root, ".kb", "stores", "default")) {
			t.Fatalf("exit %d stderr %q", code, stderr)
		}
	})
	t.Run("the database cannot be opened", func(t *testing.T) {
		orig := testDeps.openStore
		t.Cleanup(func() { testDeps.openStore = orig })
		testDeps.openStore = func(context.Context, string, bool) (store.Store, error) {
			return nil, usageErr("disk on fire")
		}
		t.Chdir(realTempDir(t))
		_, stderr, code := run(t, "init", "--model", "none")
		if code != ExitUsage || !strings.Contains(stderr, "disk on fire") {
			t.Fatalf("exit %d stderr %q", code, stderr)
		}
	})
	t.Run("the embedder cannot be built", func(t *testing.T) {
		orig := testDeps.embedder
		t.Cleanup(func() { testDeps.embedder = orig })
		testDeps.embedder = func(config.Embed) (embed.Embedder, error) { return nil, usageErr("no embedder today") }
		t.Chdir(realTempDir(t))
		_, stderr, code := run(t, "init", "--model", "none")
		if code != ExitUsage || !strings.Contains(stderr, "no embedder today") {
			t.Fatalf("exit %d stderr %q", code, stderr)
		}
	})
}

func TestModelPathFailures(t *testing.T) {
	t.Run("a broken store config", func(t *testing.T) {
		root := newTempRoot(t)
		writeConfig(t, root, "colour = 1\n")
		if _, stderr, code := run(t, "model", "path"); code != ExitUsage || !strings.Contains(stderr, `unknown key "colour"`) {
			t.Fatalf("exit %d stderr %q", code, stderr)
		}
	})
	t.Run("no model cache directory", func(t *testing.T) {
		orig := testDeps.modelCacheDir
		t.Cleanup(func() { testDeps.modelCacheDir = orig })
		testDeps.modelCacheDir = func() (string, error) { return "", usageErr("no cache here") }
		newTempRoot(t)
		if _, stderr, code := run(t, "model", "path"); code != ExitUsage || !strings.Contains(stderr, "no cache here") {
			t.Fatalf("exit %d stderr %q", code, stderr)
		}
	})
}
