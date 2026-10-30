package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRootHelpMentionsShowAndOptionalFeedback: kb help is what agents learn kb from, so it must
// teach the search → show loop and present feedback as optional, not as a required step.
func TestRootHelpMentionsShowAndOptionalFeedback(t *testing.T) {
	out, errOut, code := run(t, "help")
	if code != 0 {
		t.Fatalf("kb help: exit %d, stderr %q", code, errOut)
	}
	for _, want := range []string{"kb search \"<query>\"", "kb show <path>", "kb feedback is optional"} {
		if !strings.Contains(out, want) {
			t.Errorf("kb help lacks %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"Judge every search", "Every search is three steps", "skill"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("kb help still contains %q:\n%s", unwanted, out)
		}
	}
}

// TestWorkspaceDiscoveryCLI: from a subdirectory of a store kb list works; from a directory with
// no store above it the not-found message is printed and the exit code is 1; --root overrides
// discovery; --root with --global is a usage error.
func TestWorkspaceDiscoveryCLI(t *testing.T) {
	useFakeEmbedder(t)
	root := newTempRoot(t)
	writeFile(t, filepath.Join(root, "data", "one.md"), validFrontMatter("One", "First entry."))
	sub := filepath.Join(root, "data", "deeper", "still")
	writeFile(t, filepath.Join(sub, ".keep"), "")

	t.Run("success from a nested subdirectory", func(t *testing.T) {
		t.Chdir(sub)
		out, errOut, code := run(t, "list")
		if code != 0 || !strings.Contains(out, "one.md") {
			t.Fatalf("exit %d stdout %q stderr %q", code, out, errOut)
		}
	})
	t.Run("failure outside any store", func(t *testing.T) {
		t.Chdir(realTempDir(t))
		out, errOut, code := run(t, "list")
		want := `kb list: no kb location found in`
		if code != 1 || out != "" || !strings.Contains(errOut, want) ||
			!strings.Contains(errOut, `run "kb init" here, or use --global / --root`) {
			t.Fatalf("exit %d stdout %q stderr %q", code, out, errOut)
		}
	})
	t.Run("root flag overrides the working directory", func(t *testing.T) {
		t.Chdir(realTempDir(t))
		out, errOut, code := run(t, "--root", root, "list")
		if code != 0 || !strings.Contains(out, "one.md") {
			t.Fatalf("exit %d stdout %q stderr %q", code, out, errOut)
		}
	})
	t.Run("root and global conflict", func(t *testing.T) {
		_, errOut, code := run(t, "--root", root, "--global", "list")
		if code != 1 || !strings.Contains(errOut, "root") || !strings.Contains(errOut, "global") {
			t.Fatalf("exit %d stderr %q", code, errOut)
		}
	})
	t.Run("global reaches the store under home", func(t *testing.T) {
		t.Chdir(sub) // inside another store: --global must still win
		src := filepath.Join(realTempDir(t), "global-entry.md")
		writeFile(t, src, validFrontMatter("Global", "Created through --global."))
		home := testDeps.workspaceEnv().Home
		globalRoot := filepath.Join(home, ".local", "share", "kb")
		makeLocation(t, globalRoot, "default")
		writeFile(t, filepath.Join(globalRoot, ".kb", "stores", "default", "config.toml"), "[embed]\nmodel = \"none\"\n"+dataSourceTOML)
		_, errOut, code := run(t, "--global", "--debug", "add", src)
		if code != 0 || !strings.Contains(errOut, "location "+globalRoot+" store default (--global)") {
			t.Fatalf("exit %d stderr %q", code, errOut)
		}
		t.Cleanup(func() { os.RemoveAll(filepath.Join(home, ".local")) })
		if _, err := os.Stat(filepath.Join(home, ".local", "share", "kb", "data", "global-entry.md")); err != nil {
			t.Fatalf("--global add did not write under home: %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, "data", "global-entry.md")); err == nil {
			t.Fatal("--global add wrote into the discovered store")
		}
	})
	t.Run("debug names the chosen store and how", func(t *testing.T) {
		t.Chdir(sub)
		for _, tc := range []struct {
			args []string
			want string
		}{
			{[]string{"--debug", "list"}, "kb list: location " + root + " store default (discovered from " + sub + ")"},
			{[]string{"--debug", "--root", root, "list"}, "kb list: location " + root + " store default (--root)"},
		} {
			_, errOut, code := run(t, tc.args...)
			if code != 0 || !strings.Contains(errOut, tc.want) {
				t.Errorf("%v: exit %d stderr %q, want %q", tc.args, code, errOut, tc.want)
			}
		}
		if _, errOut, _ := run(t, "list"); strings.Contains(errOut, "location ") {
			t.Errorf("without --debug the store line leaked: %q", errOut)
		}
	})
	t.Run("root flag on a directory with no location refuses and creates nothing", func(t *testing.T) {
		fresh := filepath.Join(realTempDir(t), "new-store")
		src := filepath.Join(realTempDir(t), "fresh-entry.md")
		writeFile(t, src, validFrontMatter("Fresh", "Not created through --root."))
		for _, cmd := range []string{"add", "reindex"} {
			args := []string{"--root", fresh, cmd}
			if cmd == "add" {
				args = append(args, src)
			} else {
				args = append(args, "--all")
			}
			_, errOut, code := run(t, args...)
			if code != 1 || !strings.Contains(errOut, "run kb init") {
				t.Errorf("%s: exit %d stderr %q, want exit 1 saying run kb init", cmd, code, errOut)
			}
			if _, err := os.Stat(fresh); !os.IsNotExist(err) {
				t.Errorf("%s created %s (stat err %v)", cmd, fresh, err)
			}
		}
	})
}

// realTempDir is a fresh temp directory with symlinks resolved and no .kb above it.
func realTempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}
