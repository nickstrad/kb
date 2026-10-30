package embed

import (
	"context"
	"fmt"
	"sync"

	"github.com/nickstrad/kb/internal/embed/cache"
)

// CacheStats counts what a CachedEmbedder did since it was made.
type CacheStats struct {
	Hits        int // texts served from the cache
	Embedded    int // texts sent to the inner embedder (with the cache on, each distinct miss once)
	ModelCalls  int // calls to the inner embedder's Embed
	CacheErrors int // cache reads or writes that failed; after the first, the cache is off
}

// CachedEmbedder is an Embedder that looks texts up in a cache.Cache before embedding them. Make
// one with Cached. It is safe for concurrent use.
type CachedEmbedder struct {
	inner Embedder
	cache *cache.Cache

	mu       sync.Mutex
	stats    CacheStats
	firstErr error // the first cache failure; the cache is off once it is set
}

// Cached wraps e so each Embed call reads the cache first, embeds only the misses (each distinct
// text once, in one call) and writes them back. Model, Dim and Prefixes are e's.
//
// The key is (e.Model(), e.Dim(), sha256 of the text as passed to Embed). Callers pass texts that
// already carry their prefix (DocText, QueryText), so a document and a query with the same words
// are different keys. e.Model() must name every setting that changes vectors, as embed_meta needs.
//
// The cache never makes Embed fail: when a read or write fails, the call continues without the
// cache and the cache stays off for the rest of this embedder's life (a locked file would
// otherwise stall every call for the busy timeout). Err reports that failure so the caller can
// warn. A nil c, or an e with Dim() == 0, means no caching.
func Cached(e Embedder, c *cache.Cache) *CachedEmbedder {
	return &CachedEmbedder{inner: e, cache: c}
}

// Model reports the inner embedder's model.
func (c *CachedEmbedder) Model() string { return c.inner.Model() }

// Dim reports the inner embedder's width.
func (c *CachedEmbedder) Dim() int { return c.inner.Dim() }

// Prefixes reports the inner embedder's prefixes, or none when it has none.
func (c *CachedEmbedder) Prefixes() (doc, query string) {
	if p, ok := c.inner.(Prefixer); ok {
		return p.Prefixes()
	}
	return "", ""
}

// Stats returns the counters so far.
func (c *CachedEmbedder) Stats() CacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

// Err returns the first cache failure, or nil. Embed results are correct either way; a non-nil
// Err only means the cache was not used from that point on.
func (c *CachedEmbedder) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.firstErr
}

// Embed returns one vector per text, in order, like the inner embedder. Errors come only from the
// inner embedder, from ctx, or from an inner result of the wrong count or width.
func (c *CachedEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("cached embed with %s: %w", c.inner.Model(), err)
	}
	if !c.enabled() {
		return c.embedInner(ctx, texts)
	}
	if len(texts) == 0 {
		return [][]float32{}, nil
	}
	model, dim := c.inner.Model(), c.inner.Dim()

	out, err := c.cache.Get(ctx, model, dim, texts)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("cached embed with %s: %w", model, ctxErr)
		}
		c.fail(err)
		return c.embedInner(ctx, texts)
	}

	// Collect each distinct missing text once, remembering every position it fills.
	var missTexts []string
	missAt := map[string][]int{}
	hits := 0
	for i, v := range out {
		if v != nil {
			hits++
			continue
		}
		if _, seen := missAt[texts[i]]; !seen {
			missTexts = append(missTexts, texts[i])
		}
		missAt[texts[i]] = append(missAt[texts[i]], i)
	}
	c.countHits(hits)
	if len(missTexts) == 0 {
		return out, nil
	}

	vecs, err := c.embedInner(ctx, missTexts)
	if err != nil {
		return nil, err
	}
	for j, text := range missTexts {
		for n, i := range missAt[text] {
			if n == 0 {
				out[i] = vecs[j]
			} else {
				out[i] = append([]float32(nil), vecs[j]...) // callers may modify vectors independently
			}
		}
	}
	if err := c.cache.Put(ctx, model, dim, missTexts, vecs); err != nil && ctx.Err() == nil {
		c.fail(err)
	}
	return out, nil
}

// embedInner calls the inner embedder and checks the count and width of what it returns, so a
// bad result is neither indexed by position nor written to the cache.
func (c *CachedEmbedder) embedInner(ctx context.Context, texts []string) ([][]float32, error) {
	c.mu.Lock()
	c.stats.ModelCalls++
	c.stats.Embedded += len(texts)
	c.mu.Unlock()
	vecs, err := c.inner.Embed(ctx, texts)
	if err != nil {
		return nil, err
	}
	if len(vecs) != len(texts) {
		return nil, fmt.Errorf("embed with %s: got %d vectors for %d texts", c.inner.Model(), len(vecs), len(texts))
	}
	for i, v := range vecs {
		if len(v) != c.inner.Dim() {
			return nil, fmt.Errorf("embed with %s: vector %d has %d dims, want %d", c.inner.Model(), i, len(v), c.inner.Dim())
		}
	}
	return vecs, nil
}

// enabled reports whether the cache is in use for this call.
func (c *CachedEmbedder) enabled() bool {
	if c.cache == nil || IsNone(c.inner) {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.firstErr == nil
}

func (c *CachedEmbedder) countHits(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats.Hits += n
}

// fail records a cache failure and turns the cache off.
func (c *CachedEmbedder) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats.CacheErrors++
	if c.firstErr == nil {
		c.firstErr = fmt.Errorf("embedding cache disabled: %w", err)
	}
}
