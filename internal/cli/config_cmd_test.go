package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestConfigCommand(t *testing.T) {
	root := newTempRoot(t)

	out, errOut, code := run(t, "config")
	if code != 0 || errOut != "" || !regexp.MustCompile(`(?m)^rrf_k = 60$`).MatchString(out) {
		t.Fatalf("kb config: %d, stderr %q, stdout:\n%s", code, errOut, out)
	}
	if !strings.Contains(out, "open_window = '30m'") && !strings.Contains(out, `open_window = "30m"`) {
		t.Errorf("default open_window is not 30m:\n%s", out)
	}

	out, _, code = run(t, "config", "--path")
	if code != 0 || strings.TrimSpace(out) != testWS(root).ConfigPath {
		t.Errorf("--path = %d %q, want %s", code, out, testWS(root).ConfigPath)
	}

	writeConfig(t, root, "[search]\nrrf_k = 30\n[search.bm25]\ntitle = 5\n")
	out, _, _ = run(t, "config")
	if !strings.Contains(out, "rrf_k = 30") || !strings.Contains(out, "title = 5.0") || !strings.Contains(out, "k = 8") {
		t.Errorf("project file not layered over the defaults:\n%s", out)
	}

	out, _, code = run(t, "config", "--set", "search.rrf_k=45", "--set", "search.bm25.title=4")
	if code != 0 || !strings.Contains(out, "rrf_k = 45") || !strings.Contains(out, "title = 4.0") {
		t.Errorf("--set did not override the file: %d\n%s", code, out)
	}
	if b, _ := os.ReadFile(testWS(root).ConfigPath); strings.Contains(string(b), "45") {
		t.Errorf("--set wrote to the file")
	}

	out, _, code = run(t, "config", "--json")
	var js struct {
		Settings map[string]any `json:"settings"`
		Hash     string         `json:"settings_hash"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &js) != nil || len(js.Hash) != 64 || js.Settings["search"] == nil || js.Settings["embed"] == nil {
		t.Errorf("--json = %d %q", code, out)
	}
	out2, _, _ := run(t, "config", "--json", "--set", "search.rrf_k=31")
	if out2 == out {
		t.Errorf("--set did not change the settings hash")
	}

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"config", "--set", "search.k=99"}, "search.k: must be between 1 and 20"},
		{[]string{"config", "--set", "embed.model=acme/unknown"}, "--set: embed.model: unknown embedding model"},
		{[]string{"config", "--set", "nope=1"}, `unknown key "nope"`},
		{[]string{"config", "--set", "search.k=x"}, "--set search.k=x"},
		{[]string{"config", "--path", "--json"}, "none of the others can be"},
	} {
		if _, errOut, code := run(t, tc.args...); code != ExitUsage || !strings.Contains(errOut, tc.want) {
			t.Errorf("%v: exit %d stderr %q, want exit 1 with %q", tc.args, code, errOut, tc.want)
		}
	}
}

func TestConfigNoStore(t *testing.T) {
	t.Chdir(realTempDir(t))
	if _, errOut, code := run(t, "config"); code != ExitUsage || !strings.Contains(errOut, "no kb location found") {
		t.Errorf("exit %d stderr %q", code, errOut)
	}
}

func TestSearchKFromConfig(t *testing.T) {
	root := newSearchTestRepo(t, testModel768, testDim768)
	useRoot(t, root)
	count := func(args ...string) int {
		out, errOut, code := runKB(t, append([]string{"search", "widget", "--mode", "fts", "--json"}, args...))
		if code != 0 {
			t.Fatalf("exit %d %s", code, errOut)
		}
		var r struct{ Hits []any }
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatal(err)
		}
		return len(r.Hits)
	}
	if n := count(); n != 2 {
		t.Fatalf("default k: %d hits, want 2", n)
	}
	writeConfig(t, root, "[embed]\nmodel = \""+testModel768+"\"\n[search]\nk = 1\n")
	if n := count(); n != 1 {
		t.Errorf("config k=1: %d hits, want 1", n)
	}
	if n := count("-k", "2"); n != 2 {
		t.Errorf("-k 2 over config k=1: %d hits, want 2", n)
	}
}

// TestShowOpenWindowFromConfig: the window kb show looks back over is [log] open_window. (The
// search log stores whole seconds, so a tiny window cannot show the difference; the message does.)
func TestShowOpenWindowFromConfig(t *testing.T) {
	root := openRepo(t)
	for _, tc := range []struct{ window, want string }{{"", "last 30m0s"}, {"2h", "last 2h0m0s"}} {
		if tc.window != "" {
			writeConfig(t, root, "[log]\nopen_window = \""+tc.window+"\"\n")
		}
		_, errOut := mustKB(t, "show", "--debug", "foo.md") // no search yet: nothing to attach to
		if !strings.Contains(errOut, tc.want) {
			t.Errorf("window %q: stderr %q lacks %q", tc.window, errOut, tc.want)
		}
	}
	searchID(t, "foo")
	mustKB(t, "show", "foo.md")
	if got := openRows(t, root); len(got) != 1 {
		t.Errorf("search_opens = %v, want one row", got)
	}
}

func TestEditorFromConfigReachesDeps(t *testing.T) {
	useFakeEmbedder(t)
	root := newTempRoot(t)
	writeConfig(t, root, "[edit]\neditor = \"my-editor --wait\"\n")
	writeFile(t, filepath.Join(root, "data", "e.md"), validFrontMatter("E", "Original."))
	var got string
	orig := testDeps.editor
	t.Cleanup(func() { testDeps.editor = orig })
	testDeps.editor = func(_ context.Context, configured, _ string) error { got = configured; return nil }
	if _, errOut, code := run(t, "edit", "e.md"); code != 0 {
		t.Fatalf("exit %d %s", code, errOut)
	}
	if got != "my-editor --wait" {
		t.Errorf("editor given to deps = %q", got)
	}
}

func TestDoctorReportsConfig(t *testing.T) {
	root := newTempRoot(t)
	useNoneModel(t, root)
	if out, _, _ := run(t, "doctor"); !strings.Contains(out, "ok   config: "+testWS(root).ConfigPath) {
		t.Errorf("doctor lacks the config line:\n%s", out)
	}
	writeConfig(t, root, "bogus = 1\n")
	out, _, code := run(t, "doctor")
	if code != ExitUsage || !strings.Contains(out, "FAIL config: ") || !strings.Contains(out, `unknown key "bogus"`) {
		t.Errorf("doctor with a bad config: %d\n%s", code, out)
	}
	// One FAIL for the cause: the checks that need the settings are skipped, not run on defaults.
	if strings.Count(out, "FAIL ") != 1 || !strings.Contains(out, "doctor: 1 failed") {
		t.Errorf("doctor with a bad config cascades:\n%s", out)
	}
	for _, skipped := range []string{"embed_meta:", "model:", "entries:", "db-vs-files:", "index.md:"} {
		if strings.Contains(out, skipped) {
			t.Errorf("doctor ran %s on the defaults of an unreadable config:\n%s", skipped, out)
		}
	}
}

// TestEnvReadsStayInTheAllowedPlaces guards the no-env-vars rule: os.Getenv and friends appear only
// in workspace.OSEnv and the $EDITOR read in runEditor (and rembed's cache variable in local.Pull), plus KB_HARNESS_NOW in harnessfake.go (never compiled into release builds).
func TestEnvReadsStayInTheAllowedPlaces(t *testing.T) {
	allowed := map[string]bool{
		"internal/workspace/workspace.go": true,
		"internal/cli/edit.go":            true,
		"internal/cli/harnessfake.go":     true, // KB_HARNESS_NOW, harnessfake builds only
		"internal/embed/local/local.go":   true,
	}
	re := regexp.MustCompile(`os\.(Getenv|LookupEnv|Setenv|Environ)\(`)
	for _, dir := range []string{"../../internal", "../../cmd"} {
		filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			rel := strings.TrimPrefix(filepath.ToSlash(p), "../../")
			if b, _ := os.ReadFile(p); re.Match(b) && !allowed[rel] {
				t.Errorf("%s reads the environment", rel)
			}
			return nil
		})
	}
}
