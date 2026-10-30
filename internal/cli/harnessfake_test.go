//go:build harnessfake

package cli

import "testing"

func TestFakeModel(t *testing.T) {
	for _, tc := range []struct {
		id  string
		dim int
	}{{"fake:16", 16}, {"fake:1", 1}, {"fake:4096", 4096}, {"fake:0", 0}, {"fake:4097", 0}, {"fake:016", 0}, {"fake:x", 0}, {"fake16", 0}, {"BAAI/bge-small-en-v1.5", 0}} {
		e, ok := fakeModel(tc.id)
		if ok != (tc.dim > 0) || (ok && (e.Dim() != tc.dim || e.Model() != tc.id)) {
			t.Errorf("fakeModel(%q) = %v, %v; want dim %d", tc.id, e, ok, tc.dim)
		}
	}
	if knownModel("fake:8") != nil {
		t.Error("knownModel refuses fake:8 in a harnessfake build")
	}
}
