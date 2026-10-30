package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/embed/local"
)

// stubPull replaces testDeps.pullModel for one test with a stub that records each pull in calls and, when
// fail is nil, writes the files that make the model count as cached.
func stubPull(t *testing.T, fail error) (cache string, calls *[]string) {
	t.Helper()
	cache = t.TempDir()
	origDir, origPull := testDeps.modelCacheDir, testDeps.pullModel
	t.Cleanup(func() { testDeps.modelCacheDir, testDeps.pullModel = origDir, origPull })
	calls = new([]string)
	testDeps.modelCacheDir = func() (string, error) { return cache, nil }
	testDeps.pullModel = func(_ context.Context, spec local.Spec, dir string) error {
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
	_, calls := stubPull(t, nil)

	out, errOut, code := run(t, "model", "pull")
	if code != 0 || !strings.Contains(out, "pulled "+local.DefaultModel+" (384 dims)") || !strings.Contains(errOut, "downloading") {
		t.Fatalf("first pull: %d %q %q", code, out, errOut)
	}
	out, _, code = run(t, "model", "pull")
	if code != 0 || !strings.Contains(out, "already downloaded") {
		t.Fatalf("second pull: %d %q", code, out)
	}
	if _, _, code := run(t, "model", "pull", "--embed-model", testModel768); code != 0 {
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

func TestModelPullFailureIsUnavailable(t *testing.T) {
	stubPull(t, errors.New("download model x: no network"))
	_, errOut, code := run(t, "model", "pull")
	if code != ExitEmbedder || !strings.Contains(errOut, "kb model pull: download model x: no network") {
		t.Fatalf("failed pull: %d %q, want exit 2 with the cause", code, errOut)
	}
}

// TestOnlyIndexingCommandsDownload: reindex and add pull a missing model (with a note on stderr);
// search and doctor never do, and say how to get it.
func TestOnlyIndexingCommandsDownload(t *testing.T) {
	t.Run("reindex pulls", func(t *testing.T) {
		_, calls := stubPull(t, nil)
		newTempRoot(t)
		_, errOut, _ := run(t, "reindex", "--all", "--embed-model", testModel768)
		if len(*calls) != 1 || (*calls)[0] != testModel768 || !strings.Contains(errOut, "downloading embedding model "+testModel768) {
			t.Fatalf("pulls = %v, stderr %q; want one pull of %s with a note", *calls, errOut, testModel768)
		}
	})
	t.Run("add pulls, a failed pull is exit 2", func(t *testing.T) {
		_, calls := stubPull(t, errors.New("download model: offline"))
		newTempRoot(t)
		src := filepath.Join(t.TempDir(), "a.md")
		writeFile(t, src, validFrontMatter("A", "B."))
		_, errOut, code := run(t, "add", src, "--embed-model", testModel768)
		if len(*calls) != 1 || code != ExitEmbedder || !strings.Contains(errOut, "warning: download model: offline") || !strings.Contains(errOut, "kb model pull") {
			t.Fatalf("pulls = %v, exit %d, stderr %q; want one failed pull, exit 2 and a kb model pull hint", *calls, code, errOut)
		}
	})
	t.Run("search does not pull", func(t *testing.T) {
		_, calls := stubPull(t, nil)
		useRoot(t, newSearchTestRepo(t, testModel768, 768))
		_, errOut, code := run(t, "search", "widget", "--embed-model", testModel768)
		if len(*calls) != 0 || code != ExitEmbedder || !strings.Contains(errOut, "kb model pull") {
			t.Fatalf("pulls = %v, exit %d, stderr %q; want no pull, fts fallback (exit 2) and a hint", *calls, code, errOut)
		}
	})
	t.Run("doctor does not pull", func(t *testing.T) {
		_, calls := stubPull(t, nil)
		useRoot(t, newSearchTestRepo(t, testModel768, 768))
		out, _, _ := run(t, "doctor", "--embed-model", testModel768)
		if len(*calls) != 0 || !strings.Contains(out, "FAIL embedder: model "+testModel768+" is not downloaded (run `kb model pull`)") {
			t.Fatalf("pulls = %v, doctor output %q; want no pull and a not-downloaded FAIL", *calls, out)
		}
	})
}

// TestRootHelpNamesNoEnvEmbedders guards the help text against the removed embedder env vars and
// services.
func TestRootHelpNamesNoEnvEmbedders(t *testing.T) {
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
	for _, want := range []string{"kb model pull", "--embedder", "local (in-process model) or none", local.DefaultModel} {
		if !strings.Contains(out, want) {
			t.Errorf("root help lacks %q", want)
		}
	}
}

func TestModelCommandsCacheDirFailure(t *testing.T) {
	origDir := testDeps.modelCacheDir
	t.Cleanup(func() { testDeps.modelCacheDir = origDir })
	testDeps.modelCacheDir = func() (string, error) { return "", errors.New("no home") }
	for _, sub := range []string{"pull", "list"} {
		if _, errOut, code := run(t, "model", sub); code != ExitUsage || !strings.Contains(errOut, "kb model "+sub+": no home") {
			t.Errorf("model %s with no cache dir: %d %q", sub, code, errOut)
		}
	}
}
