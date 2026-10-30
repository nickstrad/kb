package sqlite

import (
	"context"
	"fmt"
	"testing"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
)

// TestKNNOverFetchDecision: vec0 cannot filter by tag, so KNN fetches max(K, OverFetch) only when a
// tag is set, then filters and cuts to K. Fixture: a.md (tag x) holds the 4 chunks nearest the
// query, b.md (tag y) one chunk further away.
func TestKNNOverFetchDecision(t *testing.T) {
	ctx := context.Background()
	st := openContract(t, 8).(*Store)
	v := func(seed float32) []float32 { out := make([]float32, 8); out[0], out[1] = 1, seed; return out }
	cs := func(path string, seeds ...float32) []store.ChunkInput {
		out := make([]store.ChunkInput, len(seeds))
		for i, s := range seeds {
			text := fmt.Sprintf("%s %d", path, i)
			out[i] = store.ChunkInput{Ord: i, SourceFile: path, Text: text, TextHash: text, Embedding: v(s)}
		}
		return out
	}
	for _, e := range []struct {
		path, tag string
		seeds     []float32
	}{{"a.md", "x", []float32{0, 0.1, 0.2, 0.3}}, {"b.md", "y", []float32{5}}} {
		if _, err := st.ReplaceEntry(ctx, store.EntryInput{Path: e.path, Kind: "file", Title: "t", Summary: "s",
			Tags: []string{e.tag}, BodyHash: e.path}, cs(e.path, e.seeds...)); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name      string
		k         int
		tag       string
		overFetch int
		want      int
	}{
		{"tag, OverFetch > K: over-fetches and finds b", 1, "y", 10, 1},
		{"tag, OverFetch == K: fetches K only, b is out of reach", 1, "y", 1, 0},
		{"tag, OverFetch < K: fetches K", 4, "y", 2, 0},
		{"no tag, OverFetch > K: K rows, not OverFetch", 2, "", 10, 2},
		{"tag, more matches than K: cut to K", 2, "x", 10, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := st.KNN(ctx, store.KNNQuery{Vec: v(0), K: tc.k, Tag: tc.tag, OverFetch: tc.overFetch})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != tc.want {
				t.Errorf("KNN = %v (%d rows), want %d", rows, len(rows), tc.want)
			}
		})
	}
}

// TestDecodeListEmpty: both encodings of an empty list decode to nil; anything else must be a
// JSON array.
func TestDecodeListEmpty(t *testing.T) {
	for _, s := range []string{"", "[]"} {
		if tags, err := decodeList("tags", s); tags != nil || err != nil {
			t.Errorf("decodeList(%q) = %v, %v; want nil, nil", s, tags, err)
		}
	}
	if _, err := decodeList("tags", "go"); err == nil {
		t.Error(`decodeList("go") = nil error, want a decode error`)
	}
}

// TestQueryLogCancelledIsInterrupted: a cancelled analytic query is Interrupted (classified), not
// an Internal error carrying the driver's text.
func TestQueryLogCancelledIsInterrupted(t *testing.T) {
	st := openContract(t, 0).(*Store)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := st.QueryLog(ctx, "SELECT count(*) FROM searches")
	if k := errs.KindOf(err); k != errs.Interrupted {
		t.Fatalf("QueryLog on a cancelled ctx: kind %v (%v), want Interrupted", k, err)
	}
}
