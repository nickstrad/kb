package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/entry"
	"github.com/nickstrad/kb/internal/errs"
)

// testSources is the default store's sources as kb init writes them: the entries directory data/.
var testSources = []config.Source{{Path: entry.DataDir, Layout: config.LayoutEntries, Exclude: []string{}}}

// testConfig is the defaults plus testSources, the config a run function sees for a store made by
// newTempRoot.
func testConfig() config.Config {
	c := config.Default()
	c.Sources = testSources
	return c
}

// TestInPlaceSourcesAcceptance is PLAN 1.5's acceptance in a temp copy of a repo: kb init
// --source README.md, kb reindex, kb search install returns README.md; a named store over
// docs/*.md finds docs/PLAN.md; both stores may index the same file (overlap across stores is
// allowed).
func TestInPlaceSourcesAcceptance(t *testing.T) {
	t.Parallel()
	root := realTempDir(t)
	chdir(t, root)
	withHome(t, realTempDir(t))
	writeFile(t, filepath.Join(root, "README.md"), "# kb\n\nA knowledge store.\n\n## Install\n\nRun go install to install kb.\n")
	writeFile(t, filepath.Join(root, "docs", "PLAN.md"), "# Plan\n\nThe work plan, phase by phase.\n")
	writeFile(t, filepath.Join(root, "docs", "TESTING.md"), "---\ntitle: Testing\nsummary: How kb is tested.\n---\n## Layers\n\nUnit tests.\n")

	mustRun := func(args ...string) string {
		t.Helper()
		out, errOut, code := run(t, args...)
		if code != 0 {
			t.Fatalf("kb %s: exit %d\n%s%s", strings.Join(args, " "), code, out, errOut)
		}
		return out
	}
	mustRun("init", "--model", "none", "--source", "README.md")
	if exists(filepath.Join(root, "data")) {
		t.Error("kb init --source created data/; an in-place store has no entries directory")
	}
	cfg, err := config.Load(filepath.Join(root, ".kb", "stores", "default", "config.toml"))
	if err != nil || len(cfg.Sources) != 1 || cfg.Sources[0].Glob != "README.md" || cfg.Sources[0].Layout != config.LayoutFiles {
		t.Fatalf("sources written by init = %+v, %v", cfg.Sources, err)
	}
	mustRun("reindex", "--all")
	if out := mustRun("search", "install", "--mode", "fts"); !strings.Contains(out, "README.md") {
		t.Fatalf("kb search install does not return README.md:\n%s", out)
	}

	mustRun("init", "--name", "docs", "--model", "none", "--source", "docs/*.md", "--source", "README.md")
	mustRun("--store", "docs", "reindex", "--all")
	if out := mustRun("--store", "docs", "search", "plan", "--mode", "fts"); !strings.Contains(out, "docs/PLAN.md") {
		t.Fatalf("kb --store docs search plan does not return docs/PLAN.md:\n%s", out)
	}
	// README.md is in both stores: allowed across stores.
	if out := mustRun("--store", "docs", "search", "install", "--mode", "fts"); !strings.Contains(out, "README.md") {
		t.Fatalf("the shared README.md is missing from the docs store:\n%s", out)
	}
	list := mustRun("--store", "docs", "list")
	for _, want := range []string{"README.md", "docs/PLAN.md", "The work plan, phase by phase.", "docs/TESTING.md", "How kb is tested."} {
		if !strings.Contains(list, want) {
			t.Errorf("kb list lacks %q:\n%s", want, list)
		}
	}
	if out := mustRun("show", "README.md"); !strings.Contains(out, "Run go install") {
		t.Errorf("kb show README.md:\n%s", out)
	}
}

// TestInPlaceRefusals: kb rm and kb add on an in-place store.
func TestInPlaceRefusals(t *testing.T) {
	t.Parallel()
	root := newTempRoot(t)
	writeConfig(t, root, "[embed]\nmodel = \"none\"\n[[sources]]\nglob = \"docs/*.md\"\nlayout = \"files\"\n")
	writeFile(t, filepath.Join(root, "docs", "a.md"), "# A\n\nText.\n")
	if _, errOut, code := run(t, "reindex", "--all"); code != 0 {
		t.Fatalf("reindex: %d %q", code, errOut)
	}
	_, errOut, code := run(t, "rm", "docs/a.md")
	if code != ExitUsage || !strings.Contains(errOut, "docs/a.md is not owned by kb") || !exists(filepath.Join(root, "docs", "a.md")) {
		t.Errorf("kb rm on an in-place file: exit %d %q (file kept: %v)", code, errOut, exists(filepath.Join(root, "docs", "a.md")))
	}
	src := filepath.Join(t.TempDir(), "new.md")
	writeFile(t, src, validFrontMatter("New", "A new entry."))
	if _, errOut, code := run(t, "add", src); code != ExitUsage || !strings.Contains(errOut, "no entries source to add to") {
		t.Errorf("kb add with no entries source: exit %d %q", code, errOut)
	}
	if _, errOut, code := run(t, "rm", "nope.md"); code != ExitUsage || !strings.Contains(errOut, "not a knowledge entry") {
		t.Errorf("kb rm of a missing entry: exit %d %q", code, errOut)
	}
}

// TestAddToNamedStore: a named store made by kb init adds into data-<name>/, not data/.
func TestAddToNamedStore(t *testing.T) {
	t.Parallel()
	root := realTempDir(t)
	chdir(t, root)
	withHome(t, realTempDir(t))
	for _, args := range [][]string{{"init", "--model", "none"}, {"init", "--name", "adr", "--model", "none"}} {
		if _, errOut, code := run(t, args...); code != 0 {
			t.Fatalf("%v: %d %q", args, code, errOut)
		}
	}
	src := filepath.Join(t.TempDir(), "decision.md")
	writeFile(t, src, validFrontMatter("Decision", "One decision."))
	out, errOut, code := run(t, "--store", "adr", "add", src)
	if code != 0 || !strings.Contains(out, "added data-adr/decision.md") || !exists(filepath.Join(root, "data-adr", "decision.md")) {
		t.Fatalf("add to adr: %d %q %q", code, out, errOut)
	}
	if out, _, _ := run(t, "list"); strings.Contains(out, "decision.md") {
		t.Errorf("the default store sees the adr entry:\n%s", out)
	}
	if out, _, code := run(t, "--store", "adr", "show", "decision.md"); code != 0 || !strings.Contains(out, "One decision.") {
		t.Errorf("show by corpus-relative name in adr: %d\n%s", code, out)
	}
}

// TestEntriesSource is the decision table of entriesSource: exactly one entries source, none, or
// several.
func TestEntriesSource(t *testing.T) {
	t.Parallel()
	e := func(p string) config.Source { return config.Source{Path: p, Layout: config.LayoutEntries} }
	f := config.Source{Glob: "*.md", Layout: config.LayoutFiles}
	for _, tt := range []struct {
		name    string
		sources []config.Source
		want    string // the path, or an error substring
	}{
		{"one", []config.Source{f, e("data")}, "data"},
		{"none", []config.Source{f}, "no entries source"},
		{"empty", nil, "no entries source"},
		{"two", []config.Source{e("data"), e("data-adr")}, "2 entries sources"},
	} {
		c := config.Default()
		c.Sources = tt.sources
		got, err := entriesSource(c, "cfg.toml")
		switch {
		case err == nil && got.Path != tt.want:
			t.Errorf("%s: %+v, want %s", tt.name, got, tt.want)
		case err != nil && (!strings.Contains(err.Error(), tt.want) || errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), "cfg.toml")):
			t.Errorf("%s: %v, want a Usage error with %q naming the config", tt.name, err, tt.want)
		}
	}
}

// TestDoctorSources: the sources findings, one row per outcome (Q55).
func TestDoctorSources(t *testing.T) {
	t.Parallel()
	outside := realTempDir(t)
	writeFile(t, filepath.Join(outside, "secret.md"), "# Secret\n")
	tests := []struct {
		name  string
		cfg   string
		setup func(root string)
		want  []string
		code  int
	}{
		{"none configured", "sources = []\n[embed]\nmodel = \"none\"\n", nil,
			[]string{"warn sources: none configured: this store indexes nothing"}, 0},
		{"glob matching nothing", "[embed]\nmodel = \"none\"\n[[sources]]\nglob = \"docs/*.md\"\nlayout = \"files\"\n", nil,
			[]string{`warn sources: sources[1] "docs/*.md" (files) matches no *.md file`, `ok   sources: 1 configured: "docs/*.md" (files)`}, 0},
		{"symlink out of the root", "[embed]\nmodel = \"none\"\n", func(root string) {
			if err := os.MkdirAll(filepath.Join(root, "data"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(root, "data", "leak.md")); err != nil {
				t.Fatal(err)
			}
		}, []string{"warn sources: skipped data/leak.md: symlink to"}, 0},
		{"overlap", "[embed]\nmodel = \"none\"\n" + dataSourceTOML + "[[sources]]\nglob = \"**/*.md\"\nlayout = \"files\"\n", func(root string) {
			writeFile(t, filepath.Join(root, "data", "a.md"), validFrontMatter("A", "a"))
		}, []string{"FAIL sources: sources[1] and sources[2]", "both match data/a.md"}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := newTempRoot(t)
			if strings.Contains(tt.cfg, "sources = []") {
				writeFile(t, testWS(root).ConfigPath, tt.cfg)
			} else {
				writeConfig(t, root, tt.cfg)
			}
			if tt.setup != nil {
				tt.setup(root)
			}
			out, _, code := run(t, "doctor")
			if code != tt.code {
				t.Errorf("exit %d, want %d\n%s", code, tt.code, out)
			}
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("doctor lacks %q:\n%s", w, out)
				}
			}
		})
	}
}

// TestDoctorModelFail: when the embedder cannot be built (here: no model cache directory), doctor
// prints one FAIL model line naming the cause, still runs the other checks, and exits 1 (Q60).
func TestDoctorModelFail(t *testing.T) {
	t.Parallel()
	root := newTempRoot(t)
	writeFile(t, filepath.Join(root, "data", "a.md"), validFrontMatter("A", "a"))
	orig := depsFor(t).modelCacheDir
	t.Cleanup(func() { depsFor(t).modelCacheDir = orig })
	depsFor(t).modelCacheDir = func() (string, error) { return "", errs.New(errs.Usage, "test", "no cache dir here") }
	out, _, code := run(t, "doctor")
	if code != ExitUsage || !strings.Contains(out, "FAIL model: ") || !strings.Contains(out, "no cache dir here") {
		t.Fatalf("doctor = %d, want FAIL model naming the cause:\n%s", code, out)
	}
	if !strings.Contains(out, "ok   entries: 1 entries valid") || !strings.Contains(out, "doctor: 1 failed") {
		t.Errorf("doctor stopped after the model check:\n%s", out)
	}
}

// TestReindexWarnsSkippedSymlink: kb reindex skips a link out of the root with a warning and
// still exits 0; kb list warns the same way.
func TestReindexWarnsSkippedSymlink(t *testing.T) {
	t.Parallel()
	root := newTempRoot(t)
	useNoneModel(t, root)
	outside := realTempDir(t)
	writeFile(t, filepath.Join(outside, "secret.md"), validFrontMatter("Secret", "Not for the index."))
	writeFile(t, filepath.Join(root, "data", "ok.md"), validFrontMatter("Ok", "Fine."))
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(root, "data", "leak.md")); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := run(t, "reindex", "--all")
	if code != 0 || !strings.Contains(errOut, "warning: skipped data/leak.md: symlink to") || strings.Contains(out, "leak.md") {
		t.Fatalf("reindex: %d\n%s%s", code, out, errOut)
	}
	out, errOut, _ = run(t, "list")
	if strings.Contains(out, "leak.md") || !strings.Contains(errOut, "skipped data/leak.md") {
		t.Errorf("list:\n%s%s", out, errOut)
	}
}
