//go:build !noassert

package assert

import (
	"strings"
	"testing"
)

func TestEachKindPanicsWithViolation(t *testing.T) {
	cases := []struct {
		kind string
		call func()
	}{
		{"precondition", func() { Pre(false, "f: k=%d", 0) }},
		{"postcondition", func() { Post(false, "f: k=%d", 0) }},
		{"invariant", func() { Invariant(false, "f: k=%d", 0) }},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			defer func() {
				v, ok := recover().(Violation)
				if !ok {
					t.Fatalf("panic value is not a Violation")
				}
				if v.Kind != tc.kind || v.Msg != "f: k=0" {
					t.Fatalf("Violation = %+v, want kind %s msg %q", v, tc.kind, "f: k=0")
				}
				if !strings.Contains(v.Error(), tc.kind+" violated: f: k=0") {
					t.Fatalf("Error() = %q", v.Error())
				}
			}()
			tc.call()
		})
	}
	if !Enabled {
		t.Fatal("Enabled is false in a default build")
	}
}

func TestTrueConditionsDoNotPanic(t *testing.T) {
	Pre(true, "x")
	Post(true, "x")
	Invariant(true, "x")
}
