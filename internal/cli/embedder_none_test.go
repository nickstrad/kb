package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoneEmbedderIndexesAndSearchesWithFTSOnly runs add, search and doctor with KB_EMBEDDER=none
// through the real provider (no fake hook): nothing is embedded, a default (hybrid) search runs
// as plain fts and exits 0, and --mode vec is refused.
func TestNoneEmbedderIndexesAndSearchesWithFTSOnly(t *testing.T) {
	newTempRoot(t)
	t.Setenv("KB_EMBEDDER", "none")
	src := filepath.Join(t.TempDir(), "lantern-notes.md")
	writeFile(t, src, validFrontMatter("Lantern notes", "Lanterns hang in the courtyard."))

	if out, errOut, code := run(t, "add", src); code != 0 || !strings.Contains(out, "added") {
		t.Fatalf("add: %d %q %q", code, out, errOut)
	}

	out, errOut, code := run(t, "search", "lantern courtyard", "--json", "--caller", "test")
	if code != 0 {
		t.Fatalf("search: %d %q", code, errOut)
	}
	var res struct {
		Mode string `json:"mode"`
		Hits []struct {
			Path string `json:"path"`
		} `json:"hits"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if res.Mode != "fts" || len(res.Hits) == 0 || res.Hits[0].Path != "data/lantern-notes.md" {
		t.Fatalf("search result: %+v", res)
	}

	if _, errOut, code := run(t, "search", "lantern", "--mode", "vec"); code != ExitUsage || !strings.Contains(errOut, "none") {
		t.Fatalf("vec search: %d %q", code, errOut)
	}

	if out, _, _ := run(t, "doctor"); !strings.Contains(out, "model=none dim=0") || !strings.Contains(out, "FTS-only") {
		t.Fatalf("doctor: %q", out)
	}

	// Switching to a real embedder on an FTS-only index is refused until kb reindex --all.
	if _, errOut, code := run(t, "search", "lantern", "--embedder", "ollama"); code != ExitUsage || !strings.Contains(errOut, "reindex --all") {
		t.Fatalf("mismatch: %d %q", code, errOut)
	}
}

// TestEmbedderFlagErrors reports a bad --embedder as a usage error before touching the store.
func TestEmbedderFlagErrors(t *testing.T) {
	newTempRoot(t)
	src := filepath.Join(t.TempDir(), "x.md")
	writeFile(t, src, validFrontMatter("X", "Y."))
	if _, errOut, code := run(t, "add", src, "--embedder", "word2vec"); code != ExitUsage || !strings.Contains(errOut, "unknown embedder") {
		t.Fatalf("%d %q", code, errOut)
	}
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("KB_OPENROUTER_API_KEY", "")
	t.Setenv("KB_EMBED_API_KEY", "")
	if _, errOut, code := run(t, "add", src, "--embedder", "openrouter"); code != ExitUsage || !strings.Contains(errOut, "OPENROUTER_API_KEY") {
		t.Fatalf("%d %q", code, errOut)
	}
}
