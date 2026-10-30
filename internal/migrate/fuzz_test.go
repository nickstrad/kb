package migrate

import (
	"context"
	"slices"
	"testing"
)

// FuzzSteps checks the Steps invariants on generated lists. ops decodes into a valid list of
// n = ops[0]%16 steps and one edit (ops[1]%6) at position ops[2]:
//
//	0 append a well-formed step     -> still valid; Target grows by one; Pending of the old
//	                                   Target is exactly the new step (append-only)
//	1 remove a middle step          -> invalid (gap)
//	2 swap two adjacent steps       -> invalid (order)
//	3 repeat a step at the end      -> invalid (duplicate)
//	4 renumber one ID               -> invalid
//	5 no edit                       -> valid
//
// For every valid list and every current in [0, Target+1], Pending(current) is the suffix from
// current, and running Up from current reaches Target having run exactly those steps.
func FuzzSteps(f *testing.F) {
	for _, seed := range [][]byte{{0, 0, 0}, {1, 0, 0}, {3, 1, 1}, {3, 2, 0}, {3, 3, 2}, {2, 4, 1}, {5, 5, 0}, {15, 0, 7}, {16, 1, 0}} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) < 3 {
			return
		}
		n := int(ops[0] % 16)
		s := steps(n)
		valid := true
		at := 0
		if n > 0 {
			at = int(ops[2]) % n
		}
		switch ops[1] % 6 {
		case 0:
			old := s.Target()
			s = append(s, steps(n + 1)[n])
			if s.Target() != old+1 {
				t.Fatalf("Target %d after append to %d", s.Target(), old)
			}
			if p := s.Pending(old); len(p) != 1 || p[0].ID != s[n].ID {
				t.Fatalf("Pending(old Target) = %v, want only the appended step", p)
			}
		case 1:
			if n < 3 {
				return
			}
			at = 1 + at%(n-2)
			s = slices.Delete(slices.Clone(s), at, at+1)
			valid = false
		case 2:
			if n < 2 {
				return
			}
			at %= n - 1
			s = slices.Clone(s)
			s[at], s[at+1] = s[at+1], s[at]
			valid = false
		case 3:
			if n == 0 {
				return
			}
			s = append(slices.Clone(s), s[at])
			valid = false
		case 4:
			if n == 0 {
				return
			}
			s = slices.Clone(s)
			s[at].ID = "art/9999-renumbered"
			valid = false
		}
		if err := s.Check("art"); (err == nil) != valid {
			t.Fatalf("Check = %v, want valid=%v (n=%d op=%d at=%d)", err, valid, n, ops[1]%6, at)
		}
		if !valid {
			return
		}
		for cur := 0; cur <= s.Target()+1; cur++ {
			p := s.Pending(cur)
			if want := max(0, s.Target()-cur); len(p) != want {
				t.Fatalf("len(Pending(%d)) = %d, want %d", cur, len(p), want)
			}
			for i, in := range p {
				if in.From != cur+i {
					t.Fatalf("Pending(%d)[%d].From = %d", cur, i, in.From)
				}
			}
			var ran []int
			last := cur
			done, err := s.Up(context.Background(), &ran, min(cur, s.Target()), func(_ context.Context, _ *[]int, v int) error {
				last = v
				return nil
			})
			if err != nil || len(done) != len(p) || len(ran) != len(p) {
				t.Fatalf("Up from %d: done %d ran %d err %v, want %d", cur, len(done), len(ran), err, len(p))
			}
			if len(p) > 0 && last != s.Target() {
				t.Fatalf("Up from %d stamped %d last, want %d", cur, last, s.Target())
			}
		}
	})
}
