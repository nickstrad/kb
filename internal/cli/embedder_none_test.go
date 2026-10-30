package cli

import (
	"encoding/json"
	"github.com/nickstrad/kb/internal/embed/local"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoneEmbedderIndexesAndSearchesWithFTSOnly runs add, search and doctor on a store whose
// config says [embed] model = "none", through the real newEmbedder (no fake hook): nothing is embedded, a default (hybrid) search runs
// as plain fts and exits 0, and --mode vec is refused.
func TestNoneEmbedderIndexesAndSearchesWithFTSOnly(t *testing.T) {
	root := newTempRoot(t)
	useNoneModel(t, root)
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
	} else if !strings.Contains(out, "ok   version: kb ") || strings.Contains(out, "binary") {
		t.Fatalf("doctor should report the running version and no install-path check: %q", out)
	}

	// Switching the config to a real model on an FTS-only index is refused until kb reindex --all.
	writeConfig(t, root, "")
	if _, errOut, code := run(t, "search", "lantern"); code != ExitUsage || !strings.Contains(errOut, "reindex --all") {
		t.Fatalf("mismatch: %d %q", code, errOut)
	}
}

// TestMissingConfigMeansDefaults: a store whose config.toml is empty uses the default model
// whatever its index recorded, and says how to fix the mismatch.
func TestMissingConfigMeansDefaults(t *testing.T) {
	root := newTempRoot(t)
	useNoneModel(t, root)
	src := filepath.Join(t.TempDir(), "lantern-notes.md")
	writeFile(t, src, validFrontMatter("Lantern notes", "Lanterns hang in the courtyard."))
	if _, errOut, code := run(t, "add", src); code != 0 {
		t.Fatalf("add: %d %q", code, errOut)
	}
	writeConfig(t, root, "")
	_, errOut, code := run(t, "search", "lantern")
	if code != ExitUsage || !strings.Contains(errOut, "reindex --all") {
		t.Fatalf("search with an empty config file: %d %q; want exit 1 naming kb reindex --all", code, errOut)
	}
	if out, _, code := run(t, "config"); code != 0 || !strings.Contains(out, local.DefaultModel) {
		t.Fatalf("kb config with an empty file: %d %q; want the defaults", code, out)
	}
}

// TestBadEmbedConfig reports an unknown model or key in the config as a usage error naming file
// and key, before touching the store or copying anything.
func TestBadEmbedConfig(t *testing.T) {
	root := newTempRoot(t)
	src := filepath.Join(t.TempDir(), "x.md")
	writeFile(t, src, validFrontMatter("X", "Y."))
	cfgPath := testWS(root).ConfigPath
	cases := []struct {
		name, body, want string
	}{
		{"unknown model", "[embed]\nmodel = \"acme/unknown\"\n", cfgPath + ": embed.model: unknown embedding model"},
		{"unknown key", "[embed]\nmodle = \"x\"\n", cfgPath + ": unknown key \"embed.modle\""},
		{"wrong type", "[embed]\nbatch = \"many\"\n", cfgPath + ":2: embed.batch: incompatible types"},
		{"out of range", "[search]\nk = 0\n", cfgPath + ": search.k: must be between 1 and 20"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			writeConfig(t, root, c.body)
			for _, args := range [][]string{{"add", src}, {"search", "x"}, {"reindex", "--all"}, {"config"}, {"model", "pull"}} {
				_, errOut, code := run(t, args...)
				if code != ExitUsage || !strings.Contains(errOut, c.want) {
					t.Errorf("kb %v: exit %d, stderr %q; want exit 1 mentioning %q", args, code, errOut, c.want)
				}
			}
			if _, err := os.Stat(filepath.Join(root, "data", "x.md")); !os.IsNotExist(err) {
				t.Fatalf("a bad config still copied the entry: %v", err)
			}
		})
	}
}
