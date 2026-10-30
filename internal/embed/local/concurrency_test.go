package local

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeModel is a loaded model whose Embed can block, so tests control the interleaving of
// Embed and Close without the real model.
type fakeModel struct {
	dim            int
	entered        chan struct{} // receives one value per Embed call that has started
	release        chan struct{} // Embed returns when this is closed; nil means return at once
	active         atomic.Int32  // Embed calls currently running
	closed         atomic.Int32  // Close calls
	usedAfterClose atomic.Bool
}

func (f *fakeModel) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.active.Add(1)
	defer f.active.Add(-1)
	if f.closed.Load() > 0 {
		f.usedAfterClose.Store(true)
	}
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.release != nil {
		<-f.release
	}
	if f.closed.Load() > 0 {
		f.usedAfterClose.Store(true)
	}
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = make([]float32, f.dim)
	}
	return out, nil
}

func (f *fakeModel) Close() error { f.closed.Add(1); return nil }

// fakeEmbedder returns an Embedder whose loads return the models made by mk, and counts loads.
func fakeEmbedder(t *testing.T, mk func() *fakeModel) (*Embedder, *atomic.Int32) {
	t.Helper()
	cache := t.TempDir()
	spec := models[DefaultModel]
	dir := modelDir(cache, spec.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"config.json", "model.safetensors"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e := New(spec, cache)
	var loads atomic.Int32
	e.load = func(string) (model, error) { loads.Add(1); return mk(), nil }
	return e, &loads
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestCloseWaitsForInFlightEmbed(t *testing.T) {
	fm := &fakeModel{dim: models[DefaultModel].Dim, entered: make(chan struct{}, 1), release: make(chan struct{})}
	e, _ := fakeEmbedder(t, func() *fakeModel { return fm })

	embedDone := make(chan error, 1)
	go func() { _, err := e.Embed(context.Background(), []string{"x"}); embedDone <- err }()
	waitFor(t, fm.entered, "Embed to start")

	closeDone := make(chan struct{})
	go func() { _ = e.Close(); close(closeDone) }()

	select {
	case <-closeDone:
		t.Fatal("Close returned while an Embed was still running")
	case <-time.After(100 * time.Millisecond):
	}
	if n := fm.closed.Load(); n != 0 {
		t.Fatalf("model closed %d times while an Embed was in flight, want 0", n)
	}

	close(fm.release)
	if err := <-embedDone; err != nil {
		t.Fatalf("Embed = %v, want success", err)
	}
	waitFor(t, closeDone, "Close to return")
	if n := fm.closed.Load(); n != 1 {
		t.Fatalf("model closed %d times, want exactly 1", n)
	}
	if fm.usedAfterClose.Load() {
		t.Fatal("the model was used after it was closed")
	}
}

func TestConcurrentEmbedsRunInParallel(t *testing.T) {
	const n = 4
	fm := &fakeModel{dim: models[DefaultModel].Dim, entered: make(chan struct{}, n), release: make(chan struct{})}
	e, loads := fakeEmbedder(t, func() *fakeModel { return fm })

	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.Embed(context.Background(), []string{"x"}); err != nil {
				t.Errorf("Embed = %v", err)
			}
		}()
	}
	// All n must be inside Embed at once; a serialising lock would stall after the first.
	for range n {
		waitFor(t, fm.entered, "all Embeds to be in flight together")
	}
	if got := fm.active.Load(); got != n {
		t.Fatalf("%d embeds in flight, want %d", got, n)
	}
	close(fm.release)
	wg.Wait()
	if got := loads.Load(); got != 1 {
		t.Fatalf("model loaded %d times, want once", got)
	}
}

func TestEmbedAfterCloseReloads(t *testing.T) {
	var made []*fakeModel
	var mu sync.Mutex
	e, loads := fakeEmbedder(t, func() *fakeModel {
		mu.Lock()
		defer mu.Unlock()
		fm := &fakeModel{dim: models[DefaultModel].Dim}
		made = append(made, fm)
		return fm
	})
	ctx := context.Background()
	if _, err := e.Embed(ctx, []string{"a"}); err != nil {
		t.Fatalf("first Embed: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	vecs, err := e.Embed(ctx, []string{"b"})
	if err != nil || len(vecs) != 1 {
		t.Fatalf("Embed after Close = %v, %v; want one vector and no error", vecs, err)
	}
	if loads.Load() != 2 || len(made) != 2 {
		t.Fatalf("loads = %d, want 2 (a lazy reload)", loads.Load())
	}
	if made[0].closed.Load() != 1 || made[1].closed.Load() != 0 {
		t.Fatalf("closes = %d, %d; want the first model closed once and the second still open", made[0].closed.Load(), made[1].closed.Load())
	}
}

func TestPingHoldsModelOnlyBriefly(t *testing.T) {
	fm := &fakeModel{dim: models[DefaultModel].Dim}
	e, _ := fakeEmbedder(t, func() *fakeModel { return fm })
	if err := e.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := e.Close(); err != nil { // would deadlock if Ping leaked its read lock
		t.Fatalf("Close: %v", err)
	}
	if fm.closed.Load() != 1 {
		t.Fatalf("closed %d times, want 1", fm.closed.Load())
	}
}
