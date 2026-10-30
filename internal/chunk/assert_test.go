//go:build !noassert

package chunk

import (
	"testing"

	"github.com/nickstrad/kb/internal/assert"
)

// mustViolate fails unless f panics with an assert.Violation of kind.
func mustViolate(t *testing.T, kind string, f func()) {
	t.Helper()
	defer func() {
		v, ok := recover().(assert.Violation)
		if !ok || v.Kind != kind {
			t.Fatalf("recovered %#v, want a %s Violation", v, kind)
		}
	}()
	f()
}

// TestChunkAssertionsFire: the checks are live. splitOversized's precondition is unreachable from
// Split (it skips blank sections), and Split's postconditions cannot fail from any input, so the
// checks are called directly with broken values.
func TestChunkAssertionsFire(t *testing.T) {
	mustViolate(t, "precondition", func() { splitOversized("C", " \n\n ") })
	good := newChunk(0, "a.md", "", "t: s")
	mustViolate(t, "postcondition", func() { assertChunks([]Chunk{good, newChunk(2, "a.md", "h", "x")}) })
	bad := newChunk(1, "a.md", "h", "x")
	bad.TextHash = "nope"
	mustViolate(t, "postcondition", func() { assertChunks([]Chunk{good, bad}) })
	mustViolate(t, "postcondition", func() { assertChunks([]Chunk{newChunk(0, "a.md", "h", "x")}) })
	mustViolate(t, "postcondition", func() { assertChunks(nil) }) // a Violation, not an index panic
	assertChunks([]Chunk{good, newChunk(1, "a.md", "h", "x")})
}
