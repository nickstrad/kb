//go:build noassert

package assert

import "testing"

func TestNoassertNeverPanics(t *testing.T) {
	if Enabled {
		t.Fatal("Enabled is true under noassert")
	}
	Pre(false, "x")
	Post(false, "x")
	Invariant(false, "x")
}
