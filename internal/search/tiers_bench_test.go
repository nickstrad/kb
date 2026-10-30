package search

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/store/sqlite"
)

// BenchmarkTiers is the cost of the phrase/NEAR tiers (PLAN 4.6, W13 pass) on a 3-word FTS-mode
// search over 5k entries (tierBenchStore), with phrase_boost off (the AND query) and on (AND,
// phrase, NEAR and, when a tier row is outside the AND top list_limit, one AND rescoring query).
// "typical": each query word in about 3% of the chunks; "dense": in about 75%, near-stopwords.
// Leverage entry search-latency-and-scale cites the numbers.
func BenchmarkTiers(b *testing.B) {
	ctx := context.Background()
	const query = "alpha bravo charlie"
	for _, corpus := range []struct {
		name   string
		common float64
	}{{"typical", 0.0025}, {"dense", 0.1}} { // about 3% and 75% of the chunks hold each query word
		st := tierBenchStore(b, query, corpus.common)
		for _, boost := range []bool{false, true} {
			cfg := config.Default()
			cfg.Search.PhraseBoost = boost
			settings, err := NewSettings(cfg)
			if err != nil {
				b.Fatal(err)
			}
			s := &Searcher{Store: st, Settings: settings}
			b.Run(fmt.Sprintf("%s/phrase_boost=%v", corpus.name, boost), func(b *testing.B) {
				for b.Loop() {
					out, err := s.Search(ctx, Request{Query: query, Mode: ModeFTS, K: DefaultK})
					if err != nil || out.NFTS == 0 {
						b.Fatalf("search: %v, %d fts rows", err, out.NFTS)
					}
				}
			})
		}
	}
}

// tierBenchStore is an FTS-only index of 5k entries, four 40-word chunks each, words drawn from a
// 300-word vocabulary (seeded); a query word replaces a drawn word with probability common (so
// each query word is in about 1-(1-common/3)^40 of the chunks), and one chunk in 50 holds the
// query as a phrase.
func tierBenchStore(b *testing.B, query string, common float64) *sqlite.Store {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(b.TempDir(), "kb.sqlite"), sqlite.Options{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { st.Close() })
	if err := st.EnsureEmbedMeta(ctx, store.EmbedMeta{Model: store.FTSOnlyModel}); err != nil {
		b.Fatal(err)
	}
	rng := rand.New(rand.NewSource(46))
	vocab := make([]string, 300)
	for i := range vocab {
		vocab[i] = fmt.Sprintf("v%03d", i)
	}
	qw := strings.Fields(query)
	for e := range 5000 {
		path := fmt.Sprintf("e%04d.md", e)
		chunks := make([]store.ChunkInput, 4)
		for c := range chunks {
			words := make([]string, 40)
			for w := range words {
				if rng.Float64() < common {
					words[w] = qw[rng.Intn(len(qw))]
				} else {
					words[w] = vocab[rng.Intn(len(vocab))]
				}
			}
			if rng.Intn(50) == 0 {
				copy(words[10:], qw)
			}
			text := strings.Join(words, " ")
			chunks[c] = store.ChunkInput{Ord: c, SourceFile: path, Heading: fmt.Sprintf("h%d", c), Text: text, TextHash: fmt.Sprintf("%s#%d", path, c)}
		}
		if _, err := st.ReplaceEntry(ctx, store.EntryInput{Path: path, Kind: "file", Title: path}, chunks); err != nil {
			b.Fatal(err)
		}
	}
	return st
}
