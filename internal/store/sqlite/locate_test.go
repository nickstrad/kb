package sqlite

import (
	"context"
	"testing"

	"github.com/nickstrad/kb/internal/store"
)

// TestLocateChunks: a (path, heading) resolves to the entry's first chunk with that heading, an
// open's AnyHeading to the entry's first chunk, and a gone entry, a gone heading or a missing tag
// to nothing.
func TestLocateChunks(t *testing.T) {
	ctx := context.Background()
	s, emb := newTestStore(t)
	for _, e := range []struct {
		path string
		tags []string
	}{{"a.md", []string{"Ops"}}, {"b.md", nil}} {
		in := store.EntryInput{Path: e.path, Kind: "file", Title: e.path, Tags: e.tags}
		chunks := chunkInputs(t, emb, "summary", "first setup", "second setup")
		chunks[0].Heading, chunks[1].Heading, chunks[2].Heading = "", "Setup", "Setup"
		if _, err := s.ReplaceEntry(ctx, in, chunks); err != nil {
			t.Fatal(err)
		}
	}
	a0, a1, b0 := store.ChunkRef{Path: "a.md"}, store.ChunkRef{Path: "a.md", Heading: "Setup"}, store.ChunkRef{Path: "b.md", AnyHeading: true}
	refs := []store.ChunkRef{a0, a1, b0,
		{Path: "a.md", Heading: "Nope"}, {Path: "gone.md", AnyHeading: true}, {Path: "a.md", Heading: "setup"}}
	got, err := s.LocateChunks(ctx, refs, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[a0] == 0 || got[a1] == 0 || got[b0] == 0 || got[a0] == got[a1] {
		t.Fatalf("LocateChunks = %v, want a.md summary, a.md Setup (first) and b.md first", got)
	}
	meta, err := s.ChunkMeta(ctx, []int64{got[a1], got[b0]})
	if err != nil {
		t.Fatal(err)
	}
	if m := meta[got[a1]]; m.Ord != 1 || m.EntryPath != "a.md" {
		t.Errorf("a.md Setup located %+v, want ord 1", m)
	}
	if m := meta[got[b0]]; m.Ord != 0 || m.EntryPath != "b.md" {
		t.Errorf("b.md any located %+v, want ord 0", m)
	}
	tagged, err := s.LocateChunks(ctx, refs, "ops") // normalised like the search filter
	if err != nil {
		t.Fatal(err)
	}
	if len(tagged) != 2 || tagged[a0] == 0 || tagged[a1] == 0 {
		t.Errorf("tag ops: %v, want only a.md's refs", tagged)
	}
	if none, err := s.LocateChunks(ctx, nil, ""); err != nil || len(none) != 0 {
		t.Errorf("no refs: %v, %v", none, err)
	}

	// "First" is by ord, not by chunk id: c.md's chunks are written last-ord first, so the
	// lowest id holds the highest ord.
	rev := chunkInputs(t, emb, "summary", "first setup", "second setup")
	rev[0].Heading, rev[1].Heading, rev[2].Heading = "", "Setup", "Setup"
	rev[0], rev[2] = rev[2], rev[0]
	if _, err := s.ReplaceEntry(ctx, store.EntryInput{Path: "c.md", Kind: "file", Title: "c.md"}, rev); err != nil {
		t.Fatal(err)
	}
	c1, cAny := store.ChunkRef{Path: "c.md", Heading: "Setup"}, store.ChunkRef{Path: "c.md", AnyHeading: true}
	byOrd, err := s.LocateChunks(ctx, []store.ChunkRef{c1, cAny}, "")
	if err != nil {
		t.Fatal(err)
	}
	cm, err := s.ChunkMeta(ctx, []int64{byOrd[c1], byOrd[cAny]})
	if err != nil {
		t.Fatal(err)
	}
	if cm[byOrd[c1]].Ord != 1 || cm[byOrd[cAny]].Ord != 0 {
		t.Errorf("c.md located ord %d (Setup) and %d (any), want 1 and 0", cm[byOrd[c1]].Ord, cm[byOrd[cAny]].Ord)
	}
}
