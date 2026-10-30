package embed_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/cache"
	"github.com/nickstrad/kb/internal/embed/embedtest"
	"github.com/nickstrad/kb/internal/embed/fake"
)

// stub wraps fake and lets a test count calls, see the batches, or break the result.
type stub struct {
	*fake.Embedder
	mu      sync.Mutex
	batches [][]string
	err     error                              // returned instead of vectors when set
	mangle  func(vecs [][]float32) [][]float32 // applied to the fake's result when set
}

func newStub(model string, dim int) *stub { return &stub{Embedder: fake.New(model, dim)} }

func (s *stub) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	s.mu.Lock()
	s.batches = append(s.batches, append([]string(nil), texts...))
	s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	vecs, err := s.Embedder.Embed(ctx, texts)
	if err == nil && s.mangle != nil {
		vecs = s.mangle(vecs)
	}
	return vecs, err
}

func (s *stub) calls() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]string(nil), s.batches...)
}

func openCache(t *testing.T) *cache.Cache {
	t.Helper()
	c, err := cache.Open(context.Background(), filepath.Join(t.TempDir(), "embed-cache.sqlite"))
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func rawCacheDB(t *testing.T, c *cache.Cache) *sql.DB {
	t.Helper()
	u := url.URL{Scheme: "file", Path: c.Path()}
	db, err := sql.Open("sqlite3", u.String()+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func sameVectors(t *testing.T, got, want [][]float32) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d vectors, want %d", len(got), len(want))
	}
	for i := range want {
		if fmt.Sprint(got[i]) != fmt.Sprint(want[i]) {
			t.Fatalf("vector %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func mustEmbed(t *testing.T, e embed.Embedder, texts []string) [][]float32 {
	t.Helper()
	vecs, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed(%q): %v", texts, err)
	}
	return vecs
}

func TestCachedContract(t *testing.T) {
	t.Run("fake behind a cache", func(t *testing.T) {
		embedtest.Run(t, func(t *testing.T) embed.Embedder {
			return embed.Cached(fake.New("fake", 16), openCache(t))
		}, embedtest.Caps{Deterministic: true})
	})
	t.Run("fake with a nil cache", func(t *testing.T) {
		embedtest.Run(t, func(*testing.T) embed.Embedder {
			return embed.Cached(fake.New("fake", 16), nil)
		}, embedtest.Caps{Deterministic: true})
	})
	t.Run("None behind a cache", func(t *testing.T) {
		embedtest.Run(t, func(t *testing.T) embed.Embedder {
			return embed.Cached(embed.None{}, openCache(t))
		}, embedtest.Caps{FTSOnly: true})
	})
}

// Each row embeds `first`, then `second`, through one cache, and checks what reached the model
// and that the result equals the plain fake's, in order.
func TestCachedBatches(t *testing.T) {
	many := make([]string, 50)
	for i := range many {
		many[i] = fmt.Sprintf("text %d", i)
	}
	cases := []struct {
		name          string
		first, second []string
		wantSecond    [][]string // batches the model saw for the second call
		wantHits      int        // cumulative over both calls
	}{
		{"empty batch", []string{"a"}, []string{}, nil, 0},
		{"one text, hit", []string{"a"}, []string{"a"}, nil, 1},
		{"one text, miss", []string{"a"}, []string{"b"}, [][]string{{"b"}}, 0},
		{"many texts, all hits", many, many, nil, 50},
		{"mixed keeps order", []string{"b", "d"}, []string{"a", "b", "c", "d"}, [][]string{{"a", "c"}}, 2},
		{"duplicates in a miss batch embed once", nil, []string{"x", "y", "x", "x"}, [][]string{{"x", "y"}}, 0},
		{"duplicates of a hit", []string{"x"}, []string{"x", "x", "z"}, [][]string{{"z"}}, 2},
		{"empty text", []string{""}, []string{"", "q"}, [][]string{{"q"}}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub("m", 8)
			ce := embed.Cached(s, openCache(t))
			if tc.first != nil {
				mustEmbed(t, ce, tc.first)
			}
			before := len(s.calls())
			got := mustEmbed(t, ce, tc.second)
			sameVectors(t, got, mustEmbed(t, fake.New("m", 8), tc.second))
			if seen := s.calls()[before:]; fmt.Sprint(seen) != fmt.Sprint(tc.wantSecond) {
				t.Fatalf("model saw %q, want %q", seen, tc.wantSecond)
			}
			if st := ce.Stats(); st.Hits != tc.wantHits || st.CacheErrors != 0 {
				t.Fatalf("Stats = %+v, want %d hits and no errors", st, tc.wantHits)
			}
			if ce.Err() != nil {
				t.Fatalf("Err = %v", ce.Err())
			}
		})
	}
}

// A second embed of the same text makes zero model calls, even from a second decorator on
// another handle of the same file (another process).
func TestCachedSecondEmbedHasNoModelCalls(t *testing.T) {
	c := openCache(t)
	texts := []string{"search_document: alpha", "search_document: beta"}
	mustEmbed(t, embed.Cached(newStub("m", 8), c), texts)

	other, err := cache.Open(context.Background(), c.Path())
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer other.Close()
	s := newStub("m", 8)
	ce := embed.Cached(s, other)
	mustEmbed(t, ce, texts)
	if st := ce.Stats(); st.ModelCalls != 0 || st.Hits != 2 {
		t.Fatalf("Stats = %+v, want 0 model calls and 2 hits", st)
	}
}

// Duplicates share no backing array: a caller that edits one vector does not edit another.
func TestCachedDuplicateVectorsAreIndependent(t *testing.T) {
	ce := embed.Cached(fake.New("m", 4), openCache(t))
	vecs := mustEmbed(t, ce, []string{"x", "x"})
	vecs[0][0] = 42
	if vecs[1][0] == 42 {
		t.Fatal("duplicate texts returned the same slice")
	}
}

func TestCachedKeysByModelAndDim(t *testing.T) {
	c := openCache(t)
	a := newStub("model-a", 8)
	mustEmbed(t, embed.Cached(a, c), []string{"x"})

	cases := []struct {
		name      string
		e         *stub
		wantCalls int
	}{
		{"same model and dim: hit", newStub("model-a", 8), 0},
		{"other model: miss", newStub("model-b", 8), 1},
		{"same model, other dim: miss", newStub("model-a", 4), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mustEmbed(t, embed.Cached(tc.e, c), []string{"x"})
			if n := len(tc.e.calls()); n != tc.wantCalls {
				t.Fatalf("model calls = %d, want %d", n, tc.wantCalls)
			}
			sameVectors(t, got, mustEmbed(t, tc.e.Embedder, []string{"x"}))
		})
	}
}

// A cached blob of the wrong width is a miss: the model embeds it, and the cache is repaired.
func TestCachedWrongDimEntryIsMiss(t *testing.T) {
	c := openCache(t)
	key := cache.Key("x")
	if _, err := rawCacheDB(t, c).Exec("INSERT INTO emb VALUES (?, ?, ?, ?)", "m", 8, key[:], make([]byte, 4*3)); err != nil {
		t.Fatal(err)
	}
	s := newStub("m", 8)
	ce := embed.Cached(s, c)
	sameVectors(t, mustEmbed(t, ce, []string{"x"}), mustEmbed(t, s.Embedder, []string{"x"}))
	mustEmbed(t, ce, []string{"x"})
	if st := ce.Stats(); st.ModelCalls != 1 || st.Hits != 1 || st.Embedded != 1 {
		t.Fatalf("Stats = %+v, want 1 model call (1 text embedded) then 1 hit", st)
	}
}

// Every cache failure degrades to plain embedding with the right vectors, is reported by Err and
// counted, and turns the cache off for later calls.
func TestCachedDegradesOnCacheErrors(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(t *testing.T, c *cache.Cache)
	}{
		{"closed cache (read fails)", func(t *testing.T, c *cache.Cache) { c.Close() }},
		{"table dropped (read fails)", func(t *testing.T, c *cache.Cache) {
			if _, err := rawCacheDB(t, c).Exec("DROP TABLE emb"); err != nil {
				t.Fatal(err)
			}
		}},
		{"write fails", func(t *testing.T, c *cache.Cache) {
			if _, err := rawCacheDB(t, c).Exec("CREATE TRIGGER no_insert BEFORE INSERT ON emb BEGIN SELECT RAISE(FAIL, 'read only'); END"); err != nil {
				t.Fatal(err)
			}
		}},
	}
	texts := []string{"a", "b", "a"}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := openCache(t)
			tc.break_(t, c)
			s := newStub("m", 8)
			ce := embed.Cached(s, c)
			want := mustEmbed(t, s.Embedder, texts)
			sameVectors(t, mustEmbed(t, ce, texts), want)
			if ce.Err() == nil || !strings.Contains(ce.Err().Error(), "embedding cache disabled") {
				t.Fatalf("Err = %v, want the cache failure", ce.Err())
			}
			// The cache is off now: the next call goes straight to the model, with no new error.
			sameVectors(t, mustEmbed(t, ce, texts), want)
			if st := ce.Stats(); st.CacheErrors != 1 || st.ModelCalls != 2 || st.Hits != 0 {
				t.Fatalf("Stats = %+v, want 1 cache error, 2 model calls, 0 hits", st)
			}
		})
	}
}

func TestCachedInnerFailures(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name   string
		err    error
		mangle func([][]float32) [][]float32
		want   string
	}{
		{"inner error is returned as is", boom, nil, "boom"},
		{"too few vectors", nil, func(v [][]float32) [][]float32 { return v[:1] }, "got 1 vectors for 2 texts"},
		{"too narrow", nil, func(v [][]float32) [][]float32 { v[1] = v[1][:7]; return v }, "vector 1 has 7 dims, want 8"},
	}
	for _, tc := range cases {
		for _, withCache := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/cache=%v", tc.name, withCache), func(t *testing.T) {
				var c *cache.Cache
				if withCache {
					c = openCache(t)
				}
				s := newStub("m", 8)
				s.err, s.mangle = tc.err, tc.mangle
				ce := embed.Cached(s, c)
				_, err := ce.Embed(context.Background(), []string{"a", "b"})
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("Embed error = %v, want %q", err, tc.want)
				}
				if tc.err != nil && !errors.Is(err, boom) {
					t.Fatalf("inner error not wrapped: %v", err)
				}
				if c != nil {
					// Nothing bad was cached: a healthy embedder now misses both texts.
					good := newStub("m", 8)
					mustEmbed(t, embed.Cached(good, c), []string{"a", "b"})
					if n := len(good.calls()); n != 1 {
						t.Fatalf("model calls after a failed embed = %d, want 1 (nothing cached)", n)
					}
				}
			})
		}
	}
}

// A cancelled ctx fails even when every text is cached, and is not a cache failure.
func TestCachedCancelledCtx(t *testing.T) {
	c := openCache(t)
	ce := embed.Cached(fake.New("m", 8), c)
	mustEmbed(t, ce, []string{"x"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ce.Embed(ctx, []string{"x"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Embed with cancelled ctx = %v, want context.Canceled", err)
	}
	if ce.Err() != nil {
		t.Fatalf("cancellation recorded as a cache failure: %v", ce.Err())
	}
}

func TestCachedKeepsIdentityAndPrefixes(t *testing.T) {
	plain := fake.New("m", 8)
	withPrefix := prefixed{plain, "D: ", "Q: "}
	cases := []struct {
		name  string
		inner embed.Embedder
	}{
		{"no Prefixer", plain},
		{"Prefixer", withPrefix},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ce := embed.Cached(tc.inner, nil)
			if ce.Model() != tc.inner.Model() || ce.Dim() != tc.inner.Dim() {
				t.Fatalf("Model/Dim = %s/%d, want %s/%d", ce.Model(), ce.Dim(), tc.inner.Model(), tc.inner.Dim())
			}
			if embed.DocText(ce, "t") != embed.DocText(tc.inner, "t") || embed.QueryText(ce, "q") != embed.QueryText(tc.inner, "q") {
				t.Fatalf("prefixes differ: %q/%q vs %q/%q", embed.DocText(ce, "t"), embed.QueryText(ce, "q"),
					embed.DocText(tc.inner, "t"), embed.QueryText(tc.inner, "q"))
			}
		})
	}
}

// Several goroutines share one decorator, and others use a second decorator on a second handle
// of the same file (another process). Every result equals the plain fake's.
func TestCachedConcurrent(t *testing.T) {
	c1 := openCache(t)
	c2, err := cache.Open(context.Background(), c1.Path())
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer c2.Close()
	shared := embed.Cached(fake.New("m", 8), c1)
	decorators := []*embed.CachedEmbedder{shared, shared, shared, embed.Cached(fake.New("m", 8), c2), embed.Cached(fake.New("m", 8), c2)}

	texts := make([]string, 30)
	for i := range texts {
		texts[i] = fmt.Sprintf("t%d", i%20) // some duplicates
	}
	want := mustEmbed(t, fake.New("m", 8), texts)

	var wg sync.WaitGroup
	errc := make(chan error, len(decorators)*3)
	for g, ce := range decorators {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := range 3 {
				off := (g*7 + round*3) % len(texts)
				got, err := ce.Embed(context.Background(), texts[off:])
				if err != nil {
					errc <- err
					return
				}
				for i := range got {
					if fmt.Sprint(got[i]) != fmt.Sprint(want[off+i]) {
						errc <- fmt.Errorf("goroutine %d round %d: vector %d differs", g, round, off+i)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
	for i, ce := range decorators {
		if ce.Err() != nil {
			t.Errorf("decorator %d: cache error under concurrency: %v", i, ce.Err())
		}
	}
}

// cancelledAfterFirstCheck passes the first ctx.Err() check and is cancelled from then on, so a
// cancellation lands inside the cache read.
type cancelledAfterFirstCheck struct {
	context.Context
	checks atomic.Int32
}

func (c *cancelledAfterFirstCheck) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func (c *cancelledAfterFirstCheck) Err() error {
	if c.checks.Add(1) == 1 {
		return nil
	}
	return context.Canceled
}

// A cancellation during the cache read is the caller's, not a cache failure: the error wraps
// context.Canceled and the cache stays on.
func TestCachedCancelledDuringCacheRead(t *testing.T) {
	ce := embed.Cached(fake.New("m", 8), openCache(t))
	ctx := &cancelledAfterFirstCheck{Context: context.Background()}
	if _, err := ce.Embed(ctx, []string{"x"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Embed = %v, want context.Canceled", err)
	}
	if st := ce.Stats(); ce.Err() != nil || st.CacheErrors != 0 || st.ModelCalls != 0 {
		t.Fatalf("Err = %v, Stats = %+v; want no cache error and no model call", ce.Err(), st)
	}
}
