package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/local"
)

// TestEmbedderChoice covers the --embedder / --embed-model edges: defaults, explicit empty
// values, case, a model with none, and unknown names.
func TestEmbedderChoice(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantName  string
		wantModel string
		wantErr   string
	}{
		{"defaults", nil, embedderLocal, local.DefaultModel, ""},
		{"explicit empty embedder means local", []string{"--embedder", ""}, embedderLocal, local.DefaultModel, ""},
		{"explicit empty model means default", []string{"--embed-model", ""}, embedderLocal, local.DefaultModel, ""},
		{"another supported model", []string{"--embed-model", testModel768}, embedderLocal, testModel768, ""},
		{"none ignores the model", []string{"--embedder", "none", "--embed-model", "acme/unknown"}, embedderNone, "", ""},
		{"names are case-sensitive", []string{"--embedder", "LOCAL"}, "", "", `unknown embedder "LOCAL"`},
		{"old embedder names are gone", []string{"--embedder", "ollama"}, "", "", "want local or none"},
		{"unknown model", []string{"--embed-model", "acme/unknown"}, "", "", "supported: "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := newRootCmd(testDeps, nil, nil)
			if err := cmd.ParseFlags(c.args); err != nil {
				t.Fatal(err)
			}
			name, spec, err := embedderChoice(cmd)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil || name != c.wantName || spec.ID != c.wantModel {
				t.Fatalf("got %q/%q/%v, want %q/%q", name, spec.ID, err, c.wantName, c.wantModel)
			}
		})
	}
	if name, spec, err := embedderChoice(nil); err != nil || name != embedderLocal || spec.ID != local.DefaultModel {
		t.Fatalf("nil cmd: %q/%q/%v, want the defaults", name, spec.ID, err)
	}
}

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
	if err := cmd.ParseFlags([]string{"--embedder", "none"}); err != nil {
		t.Fatal(err)
	}
	if e, err := testDeps.newEmbedder(cmd); err != nil || !embed.IsNone(e) {
		t.Fatalf("--embedder none gave %T, %v", e, err)
	}
	cmd = newRootCmd(testDeps, nil, nil)
	e, err := testDeps.newEmbedder(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if le, ok := e.(*local.Embedder); !ok || le.Model() != local.DefaultModel || le.Dim() != 384 {
		t.Fatalf("default embedder = %T %v", e, e)
	}
	origDir := testDeps.modelCacheDir
	t.Cleanup(func() { testDeps.modelCacheDir = origDir })
	testDeps.modelCacheDir = func() (string, error) { return "", errFakeCache }
	if _, err := testDeps.newEmbedder(newRootCmd(testDeps, nil, nil)); !errors.Is(err, errFakeCache) {
		t.Fatalf("cache dir failure = %v, want it returned", err)
	}
}

var errFakeCache = errors.New("no cache dir")
