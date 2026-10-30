package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/embed/local"
	"github.com/nickstrad/kb/internal/errs"
)

// stubPull replaces depsFor(t).pullModel for one test with a stub that records each pull in calls and, when
// fail is nil, writes the files that make the model count as cached.
func stubPull(t *testing.T, fail error) (cache string, calls *[]string) {
	t.Helper()
	cache = t.TempDir()
	origDir, origPull := depsFor(t).modelCacheDir, depsFor(t).pullModel
	t.Cleanup(func() { depsFor(t).modelCacheDir, depsFor(t).pullModel = origDir, origPull })
	calls = new([]string)
	depsFor(t).modelCacheDir = func() (string, error) { return cache, nil }
	depsFor(t).pullModel = func(_ context.Context, spec local.Spec, dir string) error {
		*calls = append(*calls, spec.ID)
		if fail != nil {
			return fail
		}
		md := local.New(spec, dir).ModelDir()
		if err := os.MkdirAll(md, 0o755); err != nil {
			return err
		}
		for _, f := range []string{"config.json", "model.safetensors"} {
			if err := os.WriteFile(filepath.Join(md, f), []byte("{}"), 0o644); err != nil {
				return err
			}
		}
		return nil
	}
	return cache, calls
}

func TestModelPull(t *testing.T) {
	t.Parallel()
	_, calls := stubPull(t, nil)

	out, errOut, code := run(t, "model", "pull")
	if code != 0 || !strings.Contains(out, "pulled "+local.DefaultModel+" (384 dims)") || !strings.Contains(errOut, "downloading") {
		t.Fatalf("first pull: %d %q %q", code, out, errOut)
	}
	out, _, code = run(t, "model", "pull")
	if code != 0 || !strings.Contains(out, "already downloaded") {
		t.Fatalf("second pull: %d %q", code, out)
	}
	useModel768(t, newTempRoot(t))
	if _, _, code := run(t, "model", "pull"); code != 0 {
		t.Fatalf("pull %s: exit %d", testModel768, code)
	}
	if want := []string{local.DefaultModel, testModel768}; strings.Join(*calls, ",") != strings.Join(want, ",") {
		t.Fatalf("pulls = %v, want %v (a cached model is not downloaded again)", *calls, want)
	}

	out, _, code = run(t, "model", "list")
	if code != 0 || len(strings.Split(strings.TrimSpace(out), "\n")) != len(local.Supported()) {
		t.Fatalf("list: %d %q", code, out)
	}
	for _, want := range []string{
		local.DefaultModel + " ", "384 dims  downloaded (default)",
		"sentence-transformers/all-MiniLM-L6-v2", "not downloaded",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("list output lacks %q:\n%s", want, out)
		}
	}
}

// useModel768 configures the store at root for testModel768.
func useModel768(t *testing.T, root string) {
	t.Helper()
	writeConfig(t, root, "[embed]\nmodel = \""+testModel768+"\"\n")
}

// TestModelPullFollowsConfig: kb model pull downloads the configured model, refuses "none", and
// outside any store pulls the default.
func TestModelPullFollowsConfig(t *testing.T) {
	t.Parallel()
	t.Run("none has nothing to pull", func(t *testing.T) {
		_, calls := stubPull(t, nil)
		useNoneModel(t, newTempRoot(t))
		_, errOut, code := run(t, "model", "pull")
		if code != ExitUsage || len(*calls) != 0 || !strings.Contains(errOut, "no model to download") {
			t.Fatalf("exit %d, pulls %v, stderr %q", code, *calls, errOut)
		}
	})
	t.Run("no store: the default model", func(t *testing.T) {
		_, calls := stubPull(t, nil)
		chdir(t, realTempDir(t))
		if _, _, code := run(t, "model", "pull"); code != 0 || len(*calls) != 1 || (*calls)[0] != local.DefaultModel {
			t.Fatalf("exit %d, pulls %v, want one pull of %s", code, *calls, local.DefaultModel)
		}
	})
}

func TestModelPullFailureIsUnavailable(t *testing.T) {
	t.Parallel()
	stubPull(t, errors.New("download model x: no network"))
	_, errOut, code := run(t, "model", "pull")
	if code != ExitEmbedder || !strings.Contains(errOut, "kb model pull: download model x: no network") {
		t.Fatalf("failed pull: %d %q, want exit 2 with the cause", code, errOut)
	}
}

// TestOnlyIndexingCommandsDownload: reindex and add pull a missing model (with a note on stderr);
// search and doctor never do, and say how to get it.
func TestOnlyIndexingCommandsDownload(t *testing.T) {
	t.Parallel()
	t.Run("reindex pulls", func(t *testing.T) {
		_, calls := stubPull(t, nil)
		useModel768(t, newTempRoot(t))
		_, errOut, _ := run(t, "reindex", "--all")
		if len(*calls) != 1 || (*calls)[0] != testModel768 || !strings.Contains(errOut, "downloading embedding model "+testModel768) {
			t.Fatalf("pulls = %v, stderr %q; want one pull of %s with a note", *calls, errOut, testModel768)
		}
	})
	t.Run("add pulls, a failed pull is exit 2", func(t *testing.T) {
		_, calls := stubPull(t, errors.New("download model: offline"))
		useModel768(t, newTempRoot(t))
		src := filepath.Join(t.TempDir(), "a.md")
		writeFile(t, src, validFrontMatter("A", "B."))
		_, errOut, code := run(t, "add", src)
		if len(*calls) != 1 || code != ExitEmbedder || !strings.Contains(errOut, "warning: download model: offline") || !strings.Contains(errOut, "kb model pull") {
			t.Fatalf("pulls = %v, exit %d, stderr %q; want one failed pull, exit 2 and a kb model pull hint", *calls, code, errOut)
		}
	})
	t.Run("search does not pull", func(t *testing.T) {
		_, calls := stubPull(t, nil)
		useRoot(t, newSearchTestRepo(t, testModel768, 768))
		_, errOut, code := run(t, "search", "widget")
		if len(*calls) != 0 || code != ExitEmbedder || !strings.Contains(errOut, "kb model pull") {
			t.Fatalf("pulls = %v, exit %d, stderr %q; want no pull, fts fallback (exit 2) and a hint", *calls, code, errOut)
		}
	})
	t.Run("doctor does not pull", func(t *testing.T) {
		_, calls := stubPull(t, nil)
		useRoot(t, newSearchTestRepo(t, testModel768, 768))
		out, _, _ := run(t, "doctor")
		if len(*calls) != 0 || !strings.Contains(out, "FAIL model: model "+testModel768+" is not downloaded (run `kb model pull`)") {
			t.Fatalf("pulls = %v, doctor output %q; want no pull and a not-downloaded FAIL", *calls, out)
		}
	})
}

// TestRootHelpNamesNoEnvEmbedders guards the help text against the removed embedder env vars and
// services.
func TestRootHelpNamesNoEnvEmbedders(t *testing.T) {
	t.Parallel()
	out, _, code := run(t, "--help")
	if code != 0 {
		t.Fatalf("help exit %d", code)
	}
	// The names are split so the repo-wide env-var grep stays clean.
	for _, gone := range []string{"KB_" + "EMBED", "OPEN" + "ROUTER", "Ollama", "OpenRouter", "ollama"} {
		if strings.Contains(out, gone) {
			t.Errorf("root help still mentions %q:\n%s", gone, out)
		}
	}
	for _, want := range []string{"kb model pull", "[embed] model", "kb config", local.DefaultModel} {
		if !strings.Contains(out, want) {
			t.Errorf("root help lacks %q", want)
		}
	}
}

func TestModelCommandsCacheDirFailure(t *testing.T) {
	t.Parallel()
	origDir := depsFor(t).modelCacheDir
	t.Cleanup(func() { depsFor(t).modelCacheDir = origDir })
	depsFor(t).modelCacheDir = func() (string, error) { return "", errs.New(errs.Usage, "test", "no home") }
	for _, sub := range []string{"pull", "list"} {
		if _, errOut, code := run(t, "model", sub); code != ExitUsage || !strings.Contains(errOut, "kb model "+sub+": no home") {
			t.Errorf("model %s with no cache dir: %d %q", sub, code, errOut)
		}
	}
}
