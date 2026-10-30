package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoneEmbedderIndexesAndSearchesWithFTSOnly runs add, search and doctor with --embedder none
// through the real newEmbedder (no fake hook): nothing is embedded, a default (hybrid) search runs
// as plain fts and exits 0, and --mode vec is refused.
func TestNoneEmbedderIndexesAndSearchesWithFTSOnly(t *testing.T) {
	newTempRoot(t)
	src := filepath.Join(t.TempDir(), "lantern-notes.md")
	writeFile(t, src, validFrontMatter("Lantern notes", "Lanterns hang in the courtyard."))

	if out, errOut, code := run(t, "add", src, "--embedder", "none"); code != 0 || !strings.Contains(out, "added") {
		t.Fatalf("add: %d %q %q", code, out, errOut)
	}

	out, errOut, code := run(t, "search", "lantern courtyard", "--json", "--caller", "test", "--embedder", "none")
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

	if _, errOut, code := run(t, "search", "lantern", "--mode", "vec", "--embedder", "none"); code != ExitUsage || !strings.Contains(errOut, "none") {
		t.Fatalf("vec search: %d %q", code, errOut)
	}

	if out, _, _ := run(t, "doctor", "--embedder", "none"); !strings.Contains(out, "model=none dim=0") || !strings.Contains(out, "FTS-only") {
		t.Fatalf("doctor: %q", out)
	} else if !strings.Contains(out, "ok   version: kb ") || strings.Contains(out, "binary") {
		t.Fatalf("doctor should report the running version and no install-path check: %q", out)
	}

	// Switching to a real embedder on an FTS-only index is refused until kb reindex --all.
	if _, errOut, code := run(t, "search", "lantern", "--embedder", "local"); code != ExitUsage || !strings.Contains(errOut, "reindex --all") {
		t.Fatalf("mismatch: %d %q", code, errOut)
	}
}

// TestEmbedderFlagErrors reports a bad --embedder or --embed-model as a usage error that lists
// what is supported, before touching the store.
func TestEmbedderFlagErrors(t *testing.T) {
	root := newTempRoot(t)
	src := filepath.Join(t.TempDir(), "x.md")
	writeFile(t, src, validFrontMatter("X", "Y."))
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"unknown embedder", []string{"--embedder", "word2vec"}, "want local or none"},
		{"unknown model", []string{"--embed-model", "acme/unknown"}, "supported: BAAI/bge-base-en-v1.5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, errOut, code := run(t, append([]string{"add", src}, c.args...)...)
			if code != ExitUsage || !strings.Contains(errOut, c.want) {
				t.Fatalf("exit %d, stderr %q; want exit 1 mentioning %q", code, errOut, c.want)
			}
			if _, err := os.Stat(filepath.Join(root, "data", "x.md")); !os.IsNotExist(err) {
				t.Fatalf("a bad embedder flag still copied the entry: %v", err)
			}
		})
	}
	if _, errOut, code := run(t, "model", "pull", "--embed-model", "acme/unknown"); code != ExitUsage || !strings.Contains(errOut, "supported:") {
		t.Fatalf("model pull with unknown model: %d %q", code, errOut)
	}
}
