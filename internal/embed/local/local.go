// Package local is kb's in-process embedder. It runs a sentence-embedding model with rembed
// (pure Go, no cgo, no network at embed time).
//
// Models live in a cache directory shared by every store on the machine, one subdirectory per
// model (<cache>/<org>--<name>, rembed's own layout). Only Pull downloads; Embed loads from the
// cache and reports embed.ErrUnavailable when the model is missing, so a search never triggers a
// download.
package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/rostamlabs/rembed"

	"github.com/nickstrad/kb/internal/embed"
)

// DefaultCacheDir is where models are kept when the caller does not choose: the user cache
// directory (on macOS ~/Library/Caches/kb/models).
func DefaultCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("find user cache directory: %w", err)
	}
	return filepath.Join(base, "kb", "models"), nil
}

// Embedder embeds texts with one model. The model is loaded on first use (about 0.2 s for
// bge-small), so constructing an Embedder is free for commands that never embed.
type Embedder struct {
	spec     Spec
	cacheDir string

	// mu guards loaded. Embed and Ping hold the read lock for as long as they use the model;
	// Close and the lazy load take the write lock. So Close waits for in-flight embeds, and a
	// model is never closed while in use. rembed.Embedder is documented safe for concurrent
	// Embed calls, so embeds share the read lock and run in parallel. RWMutex is not
	// context-aware: a caller waiting on the lock is not released by ctx, but the wait is
	// bounded by the in-flight embeds, and rembed checks ctx between texts.
	mu     sync.RWMutex
	loaded model
	// loadMu serialises loads, so a goroutine that finds the model already loaded never asks for
	// the write lock (which would block behind, and stall, in-flight embeds).
	loadMu sync.Mutex
	load   func(dir string) (model, error) // test seam; rembed.Load in production
}

// model is the part of *rembed.Embedder that Embedder uses; a test seam so a fake can block in
// Embed.
type model interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Close() error
}

func loadRembed(dir string) (model, error) { return rembed.Load(dir) }

// New returns an embedder for spec whose model is read from cacheDir.
func New(spec Spec, cacheDir string) *Embedder {
	return &Embedder{spec: spec, cacheDir: cacheDir, load: loadRembed}
}

// Model reports the model id; it is what embed_meta records.
func (e *Embedder) Model() string { return e.spec.ID }

// Dim reports the vector width from the model table, without loading the model.
func (e *Embedder) Dim() int { return e.spec.Dim }

// Prefixes reports the model's task prefixes.
func (e *Embedder) Prefixes() (doc, query string) { return e.spec.DocPrefix, e.spec.QueryPrefix }

// ModelDir is where the model's files are (or would be) in the cache.
func (e *Embedder) ModelDir() string { return modelDir(e.cacheDir, e.spec.ID) }

// Cached reports whether the model's files are in the cache.
func (e *Embedder) Cached() bool { return cached(e.ModelDir()) }

// Embed returns one vector per text, in order. A model that is not cached, or fails to load, is
// reported as embed.ErrUnavailable so a hybrid search can fall back to full-text search. A
// cancelled ctx is reported as itself, not as ErrUnavailable.
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("embed with %s: %w", e.spec.ID, err)
	}
	m, release, err := e.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	vecs, err := m.Embed(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("embed with %s: %w", e.spec.ID, err)
	}
	if err := checkVectors(e.spec, vecs, len(texts)); err != nil {
		return nil, err
	}
	return vecs, nil
}

// checkVectors validates the model's output: exactly n vectors, each spec.Dim wide. The output
// comes from outside kb's control (the model files), so a mismatch is an error, not an assertion.
func checkVectors(spec Spec, vecs [][]float32, n int) error {
	if len(vecs) != n {
		return fmt.Errorf("embed with %s: got %d vectors for %d texts", spec.ID, len(vecs), n)
	}
	for i, v := range vecs {
		if len(v) != spec.Dim {
			return fmt.Errorf("embed with %s: vector %d has %d dimensions, the model table says %d", spec.ID, i, len(v), spec.Dim)
		}
	}
	return nil
}

// Ping loads the model (without downloading it), so a missing or broken model is reported, as
// embed.ErrUnavailable, before any destructive work such as a full reindex.
func (e *Embedder) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("load model %s: %w", e.spec.ID, err)
	}
	_, release, err := e.acquire(ctx)
	if err != nil {
		return err
	}
	release()
	return nil
}

// Close releases the loaded model, if any. It waits for in-flight Embed and Ping calls to
// finish. An Embed after Close loads the model again.
func (e *Embedder) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.loaded == nil {
		return nil
	}
	err := e.loaded.Close()
	e.loaded = nil
	return err
}

// acquire returns the loaded model with the read lock held; the caller must call release when it
// is done with the model. A Close between the load and the read lock is handled by looping.
func (e *Embedder) acquire(ctx context.Context) (m model, release func(), err error) {
	for {
		e.mu.RLock()
		if e.loaded != nil {
			return e.loaded, e.mu.RUnlock, nil
		}
		e.mu.RUnlock()
		if err := e.ensureLoaded(); err != nil {
			return nil, nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, fmt.Errorf("load model %s: %w", e.spec.ID, err)
		}
	}
}

func (e *Embedder) ensureLoaded() error {
	e.loadMu.Lock()
	defer e.loadMu.Unlock()
	e.mu.RLock()
	isLoaded := e.loaded != nil
	e.mu.RUnlock()
	if isLoaded {
		return nil
	}
	dir := e.ModelDir()
	if !cached(dir) {
		return fmt.Errorf("model %s is not downloaded (run `kb model pull`): %w", e.spec.ID, embed.ErrUnavailable)
	}
	m, err := e.load(dir)
	if err != nil {
		return fmt.Errorf("load model %s from %s: %w: %w", e.spec.ID, dir, err, embed.ErrUnavailable)
	}
	// loaded is nil, so no embed holds the read lock (Close waited for them): this does not block.
	e.mu.Lock()
	e.loaded = m
	e.mu.Unlock()
	return nil
}

// Pull downloads spec's model into cacheDir (skipping files already there) and checks that it
// loads. It is the only function in kb that downloads a model.
//
// rembed's Load takes no context, so Pull runs it in a goroutine and returns ctx's error as soon
// as ctx is done (Ctrl-C during a download). The abandoned download keeps running until the
// process exits; that is safe because rembed writes each file to a temp name and renames it into
// place, so the cache never holds a half-written file.
func Pull(ctx context.Context, spec Spec, cacheDir string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("download model %s: %w", spec.ID, err)
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return fmt.Errorf("create model cache %s: %w", cacheDir, err)
	}
	// rembed picks its download directory from REMBED_CACHE only. Setting it here, in this
	// process, is the one place kb touches the environment; users never set it.
	if err := os.Setenv("REMBED_CACHE", cacheDir); err != nil {
		return fmt.Errorf("set model cache: %w", err)
	}
	type loaded struct {
		m   pulledModel
		err error
	}
	done := make(chan loaded, 1) // buffered: the goroutine never blocks if Pull has returned
	go func() {
		m, err := hubLoad("hf:" + spec.ID)
		done <- loaded{m, err}
	}()
	var r loaded
	select {
	case <-ctx.Done():
		return fmt.Errorf("download model %s: %w", spec.ID, ctx.Err())
	case r = <-done:
	}
	if r.err != nil {
		return fmt.Errorf("download model %s: %w", spec.ID, r.err)
	}
	if r.m.Dim() != spec.Dim {
		return errors.Join(
			fmt.Errorf("model %s has %d dimensions, the model table says %d", spec.ID, r.m.Dim(), spec.Dim),
			r.m.Close())
	}
	return r.m.Close()
}

// pulledModel is the part of *rembed.Embedder that Pull uses: the width it checks against the
// model table, and Close.
type pulledModel interface {
	Dim() int
	Close() error
}

// hubLoad is rembed.Load for Pull, returning the pulledModel interface; a test seam so the
// download's cancellation and the dimension check can be tested without the network.
var hubLoad = func(ref string, opts ...rembed.Option) (pulledModel, error) {
	m, err := rembed.Load(ref, opts...)
	if err != nil {
		return nil, err // not `return rembed.Load(...)`: a nil *Embedder would become a non-nil interface
	}
	return m, nil
}

func modelDir(cacheDir, id string) string {
	return filepath.Join(cacheDir, strings.ReplaceAll(id, "/", "--"))
}

// cached reports whether dir holds a complete download: rembed writes the weights last, so their
// presence (with the config) means the fetch finished.
func cached(dir string) bool {
	for _, f := range []string{"config.json", "model.safetensors"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			return false
		}
	}
	return true
}

var _ embed.Embedder = (*Embedder)(nil)
var _ embed.Prefixer = (*Embedder)(nil)
