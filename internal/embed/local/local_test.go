package local

import (
	"context"
	"errors"
	"github.com/rostamlabs/rembed"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/embed"
)

func TestLookup(t *testing.T) {
	s, err := Lookup(DefaultModel)
	if err != nil {
		t.Fatalf("Lookup(default): %v", err)
	}
	if s.Dim != 384 || s.QueryPrefix == "" || s.DocPrefix != "" {
		t.Fatalf("default spec = %+v, want 384 dims, a query prefix and no doc prefix", s)
	}
	if _, err := Lookup("acme/unknown"); err == nil {
		t.Fatal("Lookup(unknown) succeeded, want an error naming the supported models")
	}
}

func TestEmbedMissingModelIsUnavailable(t *testing.T) {
	e := New(models[DefaultModel], t.TempDir())
	if e.Cached() {
		t.Fatal("empty cache reports the model as cached")
	}
	_, err := e.Embed(context.Background(), []string{"x"})
	if !errors.Is(err, embed.ErrUnavailable) {
		t.Fatalf("Embed with no model = %v, want embed.ErrUnavailable", err)
	}
}

func TestPrefixesAndIdentity(t *testing.T) {
	e := New(models["nomic-ai/nomic-embed-text-v1.5"], t.TempDir())
	doc, query := e.Prefixes()
	if doc != "search_document: " || query != "search_query: " {
		t.Fatalf("nomic prefixes = %q, %q", doc, query)
	}
	if e.Model() != "nomic-ai/nomic-embed-text-v1.5" || e.Dim() != 768 {
		t.Fatalf("identity = %s/%d", e.Model(), e.Dim())
	}
}

// writeModelFiles fakes a finished download whose files are garbage, as a disk error or a
// truncated copy would leave them.
func writeModelFiles(t *testing.T, e *Embedder) {
	t.Helper()
	if err := os.MkdirAll(e.ModelDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"config.json", "model.safetensors", "tokenizer.json"} {
		if err := os.WriteFile(filepath.Join(e.ModelDir(), f), []byte("not a model"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCorruptModelIsUnavailable(t *testing.T) {
	e := New(models[DefaultModel], t.TempDir())
	writeModelFiles(t, e)
	if !e.Cached() {
		t.Fatal("complete (if corrupt) download not reported as cached")
	}
	_, err := e.Embed(context.Background(), []string{"x"})
	if !errors.Is(err, embed.ErrUnavailable) {
		t.Fatalf("Embed with a corrupt model = %v, want embed.ErrUnavailable", err)
	}
	if err := e.Ping(context.Background()); !errors.Is(err, embed.ErrUnavailable) {
		t.Fatalf("Ping with a corrupt model = %v, want embed.ErrUnavailable", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close after a failed load: %v", err)
	}
}

func TestPingMissingModelNamesPull(t *testing.T) {
	e := New(models[DefaultModel], t.TempDir())
	err := e.Ping(context.Background())
	if !errors.Is(err, embed.ErrUnavailable) || !strings.Contains(err.Error(), "kb model pull") {
		t.Fatalf("Ping with no model = %v, want ErrUnavailable with a `kb model pull` hint", err)
	}
}

func TestCancelledContextIsNotUnavailable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := New(models[DefaultModel], t.TempDir())
	for name, err := range map[string]error{
		"Embed": func() error { _, err := e.Embed(ctx, []string{"x"}); return err }(),
		"Ping":  e.Ping(ctx),
	} {
		if !errors.Is(err, context.Canceled) || errors.Is(err, embed.ErrUnavailable) {
			t.Errorf("%s with a cancelled ctx = %v, want context.Canceled and not ErrUnavailable", name, err)
		}
	}
}

func TestCloseBeforeLoad(t *testing.T) {
	e := New(models[DefaultModel], t.TempDir())
	if err := e.Close(); err != nil {
		t.Fatalf("Close before any load: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestPullUnusableCacheDir(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	err := Pull(context.Background(), models[DefaultModel], filepath.Join(file, "models"))
	if err == nil || !strings.Contains(err.Error(), "create model cache") {
		t.Fatalf("Pull into a path under a file = %v, want a create-cache error", err)
	}
}

func TestSupportedIsSortedAndLookupable(t *testing.T) {
	ids := Supported()
	if len(ids) != len(models) || !sort.StringsAreSorted(ids) {
		t.Fatalf("Supported() = %v, want every model id, sorted", ids)
	}
	for _, id := range ids {
		if s, err := Lookup(id); err != nil || s.ID != id || s.Dim <= 0 {
			t.Errorf("Lookup(%s) = %+v, %v", id, s, err)
		}
	}
	_, err := Lookup("acme/unknown")
	if err == nil || !strings.Contains(err.Error(), DefaultModel) {
		t.Fatalf("Lookup(unknown) = %v, want an error listing the supported models", err)
	}
}

func TestDefaultCacheDir(t *testing.T) {
	t.Setenv("HOME", "/Users/someone")
	dir, err := DefaultCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if dir != "/Users/someone/Library/Caches/kb/models" {
		t.Fatalf("DefaultCacheDir() = %q", dir)
	}
	t.Setenv("HOME", "")
	if _, err := DefaultCacheDir(); err == nil {
		t.Fatal("DefaultCacheDir with no HOME succeeded")
	}
}

// TestPullCorruptCacheFailsOffline: rembed skips files already in the cache and parses
// config.json before fetching anything else, so a garbage config fails Pull with no network.
func TestPullCorruptCacheFailsOffline(t *testing.T) {
	t.Setenv("REMBED_CACHE", "") // Pull sets it; restore afterwards
	cache := t.TempDir()
	e := New(models[DefaultModel], cache)
	writeModelFiles(t, e)
	err := Pull(context.Background(), models[DefaultModel], cache)
	if err == nil || !strings.Contains(err.Error(), "download model "+DefaultModel) {
		t.Fatalf("Pull over a corrupt cache = %v, want a download error naming the model", err)
	}
	if got := os.Getenv("REMBED_CACHE"); got != cache {
		t.Fatalf("REMBED_CACHE = %q, want the cache dir %q", got, cache)
	}
}

func TestCheckVectorsBoundaries(t *testing.T) {
	spec := Spec{ID: "x/y", Dim: 4}
	vec := func(d int) []float32 { return make([]float32, d) }
	cases := []struct {
		name string
		vecs [][]float32
		n    int
		ok   bool
	}{
		{"zero texts, zero vectors", nil, 0, true},
		{"one text, one vector", [][]float32{vec(4)}, 1, true},
		{"two texts, two vectors", [][]float32{vec(4), vec(4)}, 2, true},
		{"one vector too few", [][]float32{vec(4)}, 2, false},
		{"one vector too many", [][]float32{vec(4), vec(4)}, 1, false},
		{"dim one below", [][]float32{vec(3)}, 1, false},
		{"dim one above", [][]float32{vec(5)}, 1, false},
		{"dim zero", [][]float32{vec(0)}, 1, false},
		{"last vector wrong", [][]float32{vec(4), vec(3)}, 2, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkVectors(spec, c.vecs, c.n)
			if (err == nil) != c.ok {
				t.Fatalf("checkVectors = %v, want ok=%v", err, c.ok)
			}
			if err != nil && (errors.Is(err, embed.ErrUnavailable) || !strings.Contains(err.Error(), spec.ID)) {
				t.Fatalf("bad-output error %v must name the model and not be ErrUnavailable", err)
			}
		})
	}
}

func TestLookupEdgeIDs(t *testing.T) {
	for _, id := range []string{
		"", " ", "BAAI/bge-small-en-v1.5 ", "baai/bge-small-en-v1.5", "bge-small-en-v1.5",
		"BAAI/", "/bge-small-en-v1.5", "hf:BAAI/bge-small-en-v1.5", "BAAI/bge-small-en-v1.5/extra",
	} {
		if s, err := Lookup(id); err == nil {
			t.Errorf("Lookup(%q) = %+v, want an error: only exact table ids are accepted", id, s)
		}
	}
}

func TestModelDirLayout(t *testing.T) {
	e := New(models[DefaultModel], "/cache")
	if got := e.ModelDir(); got != "/cache/BAAI--bge-small-en-v1.5" {
		t.Fatalf("ModelDir = %q, want rembed's <org>--<name> layout", got)
	}
}

func TestCachedNeedsBothFiles(t *testing.T) {
	for _, files := range [][]string{nil, {"config.json"}, {"model.safetensors"}, {"config.json", "model.safetensors"}} {
		e := New(models[DefaultModel], t.TempDir())
		if err := os.MkdirAll(e.ModelDir(), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if err := os.WriteFile(filepath.Join(e.ModelDir(), f), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if got, want := e.Cached(), len(files) == 2; got != want {
			t.Errorf("Cached with %v = %v, want %v", files, got, want)
		}
	}
}

// TestPullCancelledContext: a cancelled ctx stops Pull before it creates the cache or touches the
// network, and the error is the ctx's, so the CLI does not report it as a download failure.
func TestPullCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cache := filepath.Join(t.TempDir(), "models")
	err := Pull(ctx, models[DefaultModel], cache)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Pull with a cancelled ctx = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(cache); !os.IsNotExist(statErr) {
		t.Fatalf("cache dir created despite the cancelled ctx: %v", statErr)
	}
}

// TestPullCancelMidDownload: Ctrl-C while rembed is still downloading returns promptly with the
// ctx error instead of waiting for the download (main's signal handler swallows SIGINT).
func TestPullCancelMidDownload(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	started := make(chan struct{})
	p := puller{hubLoad: func(string, ...rembed.Option) (pulledModel, error) {
		close(started)
		<-release
		return nil, errors.New("released")
	}}
	t.Setenv("REMBED_CACHE", "")
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- p.Pull(ctx, models[DefaultModel], t.TempDir()) }()
	<-started
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Pull cancelled mid-download = %v, want context.Canceled", err)
	}
}

// dimModel is a downloaded model that reports a width, for Pull's dimension check.
type dimModel struct {
	dim    int
	closed int
}

func (d *dimModel) Dim() int     { return d.dim }
func (d *dimModel) Close() error { d.closed++; return nil }

// TestPullDimCheck: Pull accepts a download whose width is the table's and refuses one that is
// one below or one above it, closing the model in every case.
func TestPullDimCheck(t *testing.T) {
	spec := models[DefaultModel]
	for _, tc := range []struct {
		name    string
		dim     int
		wantErr string
	}{
		{"at the table width", spec.Dim, ""},
		{"table width-1", spec.Dim - 1, "has 383 dimensions, the model table says 384"},
		{"table width+1", spec.Dim + 1, "has 385 dimensions, the model table says 384"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &dimModel{dim: tc.dim}
			p := puller{hubLoad: func(string, ...rembed.Option) (pulledModel, error) { return m, nil }}
			t.Setenv("REMBED_CACHE", "")
			err := p.Pull(context.Background(), spec, t.TempDir())
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Pull = %v, want nil", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Pull = %v, want an error containing %q", err, tc.wantErr)
			}
			if m.closed != 1 {
				t.Fatalf("model closed %d times, want 1", m.closed)
			}
		})
	}
}

// TestModelTableDims pins each supported model's width to the value its published model has, so
// a wrong number in models.go fails here (the -tags model tests download only the default model).
func TestModelTableDims(t *testing.T) {
	for id, want := range map[string]int{
		"BAAI/bge-small-en-v1.5":                 384,
		"BAAI/bge-base-en-v1.5":                  768,
		"sentence-transformers/all-MiniLM-L6-v2": 384,
		"nomic-ai/nomic-embed-text-v1.5":         768,
	} {
		s, err := Lookup(id)
		if err != nil {
			t.Fatalf("Lookup(%s): %v", id, err)
		}
		if s.Dim != want {
			t.Errorf("%s Dim = %d, want %d", id, s.Dim, want)
		}
	}
	if len(Supported()) != 4 {
		t.Errorf("Supported() has %d models; update TestModelTableDims with the new one", len(Supported()))
	}
}

// TestEmbedCtxCancelledWhileLoadingSkipsModel: the context is cancelled while the model loads.
// Embed reports the context error and never calls the model.
func TestEmbedCtxCancelledWhileLoadingSkipsModel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := New(models[DefaultModel], t.TempDir())
	writeModelFiles(t, e)
	fm := &fakeModel{dim: models[DefaultModel].Dim, entered: make(chan struct{}, 1)}
	e.load = func(string) (model, error) { cancel(); return fm, nil }

	_, err := e.Embed(ctx, []string{"x"})
	if !errors.Is(err, context.Canceled) || errors.Is(err, embed.ErrUnavailable) {
		t.Fatalf("Embed cancelled during load = %v, want context.Canceled and not ErrUnavailable", err)
	}
	if n := len(fm.entered); n != 0 {
		t.Fatalf("model was called %d time(s) after the context was cancelled", n)
	}
}
