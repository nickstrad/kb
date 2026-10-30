package cache_test

import (
	"context"
	"encoding/binary"
	"math"
	"path/filepath"
	"testing"

	"github.com/nickstrad/kb/internal/embed/cache"
)

// FuzzPutGetRoundTrip: whatever floats are stored (NaN payloads, infinities, -0 included) come
// back bit for bit, under any text and model.
func FuzzPutGetRoundTrip(f *testing.F) {
	nan := make([]byte, 8)
	binary.LittleEndian.PutUint32(nan, math.Float32bits(float32(math.NaN())))
	binary.LittleEndian.PutUint32(nan[4:], 0x80000000) // -0
	f.Add("text", "model", []byte{0, 0, 128, 63})      // 1.0
	f.Add("", "", nan)
	f.Add("search_query: \x00ü", "bge+int8", []byte{1, 2, 3, 4, 5, 6, 7, 8, 9}) // trailing byte ignored

	c, err := cache.Open(context.Background(), filepath.Join(f.TempDir(), "fuzz.sqlite"))
	if err != nil {
		f.Fatalf("Open: %v", err)
	}
	f.Cleanup(func() { c.Close() })

	f.Fuzz(func(t *testing.T, text, model string, raw []byte) {
		n := len(raw) / 4
		if n == 0 {
			return // dim must be positive; covered by a unit test
		}
		v := make([]float32, n)
		for i := range v {
			v[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[4*i:]))
		}
		ctx := context.Background()
		if err := c.Put(ctx, model, n, []string{text}, [][]float32{v}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		got, err := c.Get(ctx, model, n, []string{text})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if len(got[0]) != n {
			t.Fatalf("got %d floats, want %d", len(got[0]), n)
		}
		for i := range v {
			if math.Float32bits(got[0][i]) != math.Float32bits(v[i]) {
				t.Fatalf("float %d: got bits %08x, want %08x", i, math.Float32bits(got[0][i]), math.Float32bits(v[i]))
			}
		}
	})
}
