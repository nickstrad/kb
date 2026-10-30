package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/local"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/search"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/store/sqlite"
	"github.com/nickstrad/kb/internal/workspace"
)

func TestDownloadsModel(t *testing.T) {
	for name, want := range map[string]bool{
		"reindex": true, "add": true,
		"search": false, "doctor": false, "show": false, "edit": false, "pull": false, "": false,
	} {
		if got := downloadsModel(&cobra.Command{Use: name}); got != want {
			t.Errorf("downloadsModel(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestNewEmbedderKinds(t *testing.T) {
	cmd := newRootCmd(testDeps, nil, nil)
	if e, err := testDeps.newEmbedder(cmd, config.Embed{Model: config.ModelNone}); err != nil || !embed.IsNone(e) {
		t.Fatalf("model none gave %T, %v", e, err)
	}
	e, err := testDeps.newEmbedder(cmd, config.Default().Embed)
	if err != nil {
		t.Fatal(err)
	}
	if le, ok := e.(*local.Embedder); !ok || le.Model() != local.DefaultModel || le.Dim() != 384 {
		t.Fatalf("default embedder = %T %v", e, e)
	}
	if _, err := testDeps.newEmbedder(cmd, config.Embed{Model: "acme/unknown"}); errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), "supported: ") {
		t.Fatalf("unknown model = %v, want a Usage error listing the supported models", err)
	}
	origDir := testDeps.modelCacheDir
	t.Cleanup(func() { testDeps.modelCacheDir = origDir })
	testDeps.modelCacheDir = func() (string, error) { return "", errFakeCache }
	if _, err := testDeps.newEmbedder(cmd, config.Default().Embed); !errors.Is(err, errFakeCache) {
		t.Fatalf("cache dir failure = %v, want it returned", err)
	}
}

// errFakeCache is the kind production returns for a missing cache dir (root.go modelCacheDir):
// a test double returns only the error kinds production returns.
var errFakeCache = errs.New(errs.Usage, "test.modelCacheDir", "no cache dir")

// TestConfigConstantsMatchServices pins the values config repeats so it need not import a
// service or the embedder: the k bounds, the default model and the open window.
func TestConfigConstantsMatchServices(t *testing.T) {
	if config.MinK != search.MinK || config.MaxK != search.MaxK {
		t.Errorf("config k bounds %d..%d != search %d..%d", config.MinK, config.MaxK, search.MinK, search.MaxK)
	}
	d := config.Default()
	if d.Search.K != search.DefaultK || d.Search.ListLimit != search.ListLimit || d.Search.CandidateLimit != search.CandidateLimit ||
		d.Search.PerEntryCap != search.PerEntryCap || d.Search.RRFK != search.RRFConstant {
		t.Errorf("config search defaults %+v differ from the search package constants", d.Search)
	}
	if config.DefaultModel != local.DefaultModel {
		t.Errorf("config.DefaultModel %q != local.DefaultModel %q", config.DefaultModel, local.DefaultModel)
	}
	if got := d.Log.OpenWindowDuration(); got != 30*time.Minute {
		t.Errorf("default open window %v, want the documented 30m (kb show --help, README)", got)
	}
}

// recordModel creates the store's database with embed_meta set to model.
func recordModel(t *testing.T, root, model string, dim int) {
	t.Helper()
	if err := ensureKBDir(root); err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(context.Background(), testDBPath(root), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.EnsureEmbedMeta(context.Background(), store.EmbedMeta{Model: model, Dim: dim}); err != nil {
		t.Fatal(err)
	}
}

// loadFor runs loadConfig for the store at root, with the global store under home.
func loadFor(t *testing.T, root, home string) (config.Config, error) {
	t.Helper()
	d := *testDeps
	d.workspaceEnv = func() workspace.Env {
		return workspace.Env{Cwd: root, Home: home, CacheBase: filepath.Join(home, "cache")}
	}
	cmd := &cobra.Command{Use: "kb"}
	cmd.SetContext(context.Background())
	return d.loadConfig(cmd, testWS(root))
}

// TestLoadConfigDecisions is the branch table of loadConfig: which model a store gets from its
// config file or the default (never from its index), and how the global store's
// aliases merge.
func TestLoadConfigDecisions(t *testing.T) {
	const cfgPath = ".kb/stores/default/config.toml"
	tests := []struct {
		name       string
		config     *string // nil = no file
		recorded   string  // "" = no database
		recDim     int
		wantModel  string
		wantErrHas string
	}{
		{"no file: default", nil, "", 0, local.DefaultModel, ""},
		{"no file, index recorded none: still the default, config is the one source", nil, "none", 0, local.DefaultModel, ""},
		{"empty file, index recorded none: the file wins, default model", ptr(""), "none", 0, local.DefaultModel, ""},
		{"file says none", ptr("[embed]\nmodel = \"none\"\n"), "", 0, "none", ""},
		{"file says a model, index disagrees: the file wins", ptr("[embed]\nmodel = \"" + testModel768 + "\"\n"), "none", 0, testModel768, ""},
		{"file names an unknown model", ptr("[embed]\nmodel = \"acme/unknown\"\n"), "", 0, "", cfgPath + ": embed.model: unknown embedding model"},
		{"file has an unknown key", ptr("[embed]\nmodle = \"x\"\n"), "", 0, "", cfgPath + ": unknown key \"embed.modle\""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := useRoot(t, t.TempDir())
			if tt.config == nil {
				if err := os.Remove(testWS(root).ConfigPath); err != nil {
					t.Fatal(err)
				}
			} else {
				writeConfig(t, root, *tt.config)
			}
			if tt.recorded != "" {
				recordModel(t, root, tt.recorded, tt.recDim)
			}
			cfg, err := loadFor(t, root, t.TempDir())
			if tt.wantErrHas != "" {
				if errs.KindOf(err) != errs.Usage || err == nil || !strings.Contains(err.Error(), tt.wantErrHas) {
					t.Fatalf("err = %v, want a Usage error containing %q", err, tt.wantErrHas)
				}
				return
			}
			if err != nil || cfg.Embed.Model != tt.wantModel {
				t.Fatalf("model = %q, err = %v; want %q", cfg.Embed.Model, err, tt.wantModel)
			}
		})
	}
}

func ptr(s string) *string { return &s }

// TestPrecedenceDecisions is the decision table for the settings a command resolves from a flag
// and the config: the flag wins when given, else the config value, else the default.
func TestPrecedenceDecisions(t *testing.T) {
	t.Run("search k", func(t *testing.T) {
		for _, tt := range []struct {
			name           string
			changed        bool
			flagK, configK int
			want           int
		}{
			{"flag given beats config", true, 3, 12, 3},
			{"flag given equal to the default still beats config", true, 8, 12, 8},
			{"flag not given: config", false, 8, 12, 12},
			{"neither: the default, which is the config default", false, 8, config.Default().Search.K, 8},
		} {
			if got := searchK(tt.changed, tt.flagK, tt.configK); got != tt.want {
				t.Errorf("%s: searchK = %d, want %d", tt.name, got, tt.want)
			}
		}
	})
	t.Run("caller", func(t *testing.T) {
		withCaller := func(c string) config.Config { cfg := config.Default(); cfg.Agent.Caller = c; return cfg }
		for _, tt := range []struct {
			name, flag string
			cfg        config.Config
			want       string
		}{
			{"flag beats config", "flag", withCaller("cfg"), "flag"},
			{"config when no flag", "", withCaller("cfg"), "cfg"},
			{"unknown when neither", "", withCaller(""), "unknown"},
			{"flag without config", "flag", withCaller(""), "flag"},
		} {
			if got := callerFor(tt.flag, tt.cfg); got != tt.want {
				t.Errorf("%s: callerFor = %q, want %q", tt.name, got, tt.want)
			}
		}
	})
	t.Run("editor", func(t *testing.T) {
		for _, tt := range []struct{ name, configured, env, want string }{
			{"config beats $EDITOR", "code -w", "nano", "code -w"},
			{"$EDITOR when no config", "", "nano", "nano"},
			{"vi when neither", "", "", "vi"},
		} {
			if got := chooseEditor(tt.configured, tt.env); got != tt.want {
				t.Errorf("%s: chooseEditor = %q, want %q", tt.name, got, tt.want)
			}
		}
	})
}
