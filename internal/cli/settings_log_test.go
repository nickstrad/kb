package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/store/sqlite"
)

// lastSearchSettings reads the settings columns of the newest search in a store's log.
func lastSearchSettings(t *testing.T, dbPath string) (settings, hash string, fts sql.NullString) {
	t.Helper()
	st, err := sqlite.Open(context.Background(), dbPath, sqlite.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var s, h sql.NullString
	if err := st.DB().QueryRow("SELECT settings, settings_hash, fts_query FROM searches ORDER BY id DESC LIMIT 1").Scan(&s, &h, &fts); err != nil {
		t.Fatal(err)
	}
	if !s.Valid || !h.Valid {
		t.Fatalf("the newest search has no settings (settings %v, hash %v)", s, h)
	}
	return s.String, h.String, fts
}

// TestSearchLogsEachStoresSettings (PLAN 2.5): a search logs the effective settings of the store
// it ran on, not the defaults: the logged hash equals the `kb config --json` hash of that store,
// the logged JSON is the canonical text the hash covers, two stores with different [search] log
// different hashes, and `kb stats settings` shows one row per hash.
func TestSearchLogsEachStoresSettings(t *testing.T) {
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

	hashes := map[string]string{}
	for _, store := range []string{"default", "adr"} {
		if _, stderr, code := run(t, "--store", store, "search", "widget", "--mode", "fts", "--caller", "t"); code != 0 {
			t.Fatalf("search on %s: %d %q", store, code, stderr)
		}
		stdout, _, code := run(t, "--store", store, "config", "--json")
		if code != 0 {
			t.Fatalf("config --json on %s: %d", store, code)
		}
		var cfg struct {
			Settings     json.RawMessage `json:"settings"`
			SettingsHash string          `json:"settings_hash"`
		}
		if err := json.Unmarshal([]byte(stdout), &cfg); err != nil {
			t.Fatal(err)
		}
		db := filepath.Join(root, ".kb", "stores", store, "kb.sqlite")
		settings, hash, fts := lastSearchSettings(t, db)
		if hash != cfg.SettingsHash {
			t.Errorf("%s: logged hash %s, kb config --json says %s", store, hash, cfg.SettingsHash)
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, cfg.Settings); err != nil {
			t.Fatal(err)
		}
		if settings != compact.String() {
			t.Errorf("%s: logged settings %s, kb config --json says %s", store, settings, compact.String())
		}
		if !fts.Valid || !strings.Contains(fts.String, "widget") {
			t.Errorf("%s: fts_query = %v, want the MATCH expression for widget", store, fts)
		}
		hashes[store] = hash
	}
	if hashes["default"] == hashes["adr"] {
		t.Errorf("stores with different [search] logged one hash %s", hashes["default"])
	}
	want := config.Default()
	want.Embed.Model = config.ModelNone // what useNoneModel wrote
	if _, wantHash, err := want.EffectiveJSON(); err != nil || hashes["default"] != wantHash {
		t.Errorf("the default store's hash %s is not its config's %s (%v)", hashes["default"], wantHash, err)
	}

	stdout, stderr, code := run(t, "--store", "adr", "stats", "settings")
	if code != 0 || !strings.Contains(stdout, hashes["adr"][:12]) || strings.Contains(stdout, hashes["default"][:12]) {
		t.Errorf("kb stats settings on adr: exit %d %q\n%s", code, stderr, stdout)
	}
	// A second search with the same settings joins the same row.
	run(t, "--store", "adr", "search", "widget", "--mode", "fts", "--caller", "t")
	stdout, _, _ = run(t, "--store", "adr", "stats", "settings", "--json")
	if n := strings.Count(stdout, hashes["adr"][:12]); n != 1 {
		t.Errorf("settings rows for one config = %d, want 1:\n%s", n, stdout)
	}
}
