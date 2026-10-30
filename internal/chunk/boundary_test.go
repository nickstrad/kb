package chunk

import (
	"strings"
	"testing"
)

// TestSplitMaxChunkCharsBoundary: a section whose full text (context line included) is exactly
// MaxChunkChars stays one chunk; one byte more is split at its paragraph break (rule 4).
func TestSplitMaxChunkCharsBoundary(t *testing.T) {
	const ctx = "Thing › S" // len counts bytes; "›" is 3
	for _, tc := range []struct {
		name       string
		fullLen    int
		wantPieces int
	}{
		{"MaxChunkChars-1", MaxChunkChars - 1, 1},
		{"at MaxChunkChars", MaxChunkChars, 1},
		{"MaxChunkChars+1", MaxChunkChars + 1, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p1 := strings.Repeat("a", 100)
			p2 := strings.Repeat("b", tc.fullLen-len(ctx)-1-len(p1)-2)
			chunks := mustSplit(t, "## S\n\n"+p1+"\n\n"+p2+"\n")
			pieces := chunks[1:]
			if len(pieces) != tc.wantPieces {
				t.Fatalf("got %d pieces, want %d", len(pieces), tc.wantPieces)
			}
			if tc.wantPieces == 1 && len(pieces[0].Text) != tc.fullLen {
				t.Fatalf("piece is %d bytes, want %d", len(pieces[0].Text), tc.fullLen)
			}
		})
	}
}

// TestSplitOversizedMergeBoundary: while packing paragraphs, a piece may grow to exactly
// MaxChunkChars; the paragraph that would take it one byte over starts the next piece.
func TestSplitOversizedMergeBoundary(t *testing.T) {
	const ctx = "C"
	for _, tc := range []struct {
		name       string
		firstTwo   int // len(ctx + "\n" + a + "\n\n" + b)
		wantPieces []int
	}{
		{"first two at MaxChunkChars-1: merged", MaxChunkChars - 1, []int{2, 1}},
		{"first two at MaxChunkChars: merged", MaxChunkChars, []int{2, 1}},
		{"first two at MaxChunkChars+1: split", MaxChunkChars + 1, []int{1, 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := strings.Repeat("a", 700)
			b := strings.Repeat("b", tc.firstTwo-len(ctx)-1-len(a)-2)
			c := strings.Repeat("c", 10)
			pieces := splitOversized(ctx, a+"\n\n"+b+"\n\n"+c)
			if len(pieces) != len(tc.wantPieces) {
				t.Fatalf("got %d pieces, want %d", len(pieces), len(tc.wantPieces))
			}
			for i, p := range pieces {
				if n := len(strings.Split(strings.TrimPrefix(p, ctx+"\n"), "\n\n")); n != tc.wantPieces[i] {
					t.Errorf("piece %d holds %d paragraphs, want %d", i, n, tc.wantPieces[i])
				}
			}
		})
	}
}

// TestSplitOversizedNoBlocks covers splitOversized's defensive branch: a body with no paragraph
// is returned as one piece. Split never reaches it (it skips blank sections first); this is where
// an assert.Pre(len(blocks) > 0) goes once internal/assert exists.
func TestSplitOversizedNoBlocks(t *testing.T) {
	if got := splitOversized("C", " \n\n "); len(got) != 1 || got[0] != "C\n \n\n " {
		t.Fatalf("splitOversized(blank) = %q, want the one unchanged piece", got)
	}
}

// TestFileTitle is the decision table for a docs/*.md file's title: front-matter title, leading
// H1, both, neither, a hashes-only H1, a different H1, and an all-blank body.
func TestFileTitle(t *testing.T) {
	for _, tc := range []struct {
		name, fm, body      string
		wantTitle, wantBody string
	}{
		{"H1 only: H1 is the title, dropped", "", "\n# Doc\n\ntext", "Doc", "\n\ntext"},
		{"fm only: body unchanged", "Front", "text", "Front", "text"},
		{"H1 restates fm (case): fm wins, H1 dropped", "Front", "# front\ntext", "Front", "text"},
		{"H1 differs from fm: H1 stays as text", "Front", "# Other\ntext", "Front", "# Other\ntext"},
		{"neither: empty title", "", "text", "", "text"},
		{"hashes-only H1 is not a title", "", "# ###\ntext", "", "# ###\ntext"},
		{"## is not an H1", "", "## Sec\ntext", "", "## Sec\ntext"},
		{"all-blank body", "Front", "\n  \n", "Front", "\n  \n"},
		{"empty body", "", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			title, body := fileTitle(tc.fm, tc.body)
			if title != tc.wantTitle || body != tc.wantBody {
				t.Fatalf("fileTitle(%q, %q) = %q, %q; want %q, %q", tc.fm, tc.body, title, body, tc.wantTitle, tc.wantBody)
			}
		})
	}
}
