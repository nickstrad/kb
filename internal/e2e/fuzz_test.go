package e2e

import (
	"reflect"
	"testing"
)

// FuzzTranscript: formatting a parsed transcript and parsing it again changes nothing, and the
// matcher never panics on any expected and actual pair.
func FuzzTranscript(f *testing.F) {
	for _, s := range []string{
		"", "prose\n", "  $ kb a\n  out\n  [exit 1]\n", "  $ kb a\n  x\n\n  y\n  \n\np\n-- f --\nz\n",
		"  $ cd x\n  ...\n  ~ (a|b)+\n  [stderr] ~ w\n", "-- @a --\n-- a --\n", "\n\n", "  $ \n   $ x\n",
	} {
		f.Add(s, s)
	}
	f.Fuzz(func(t *testing.T, in, other string) {
		tr, err := Parse(in)
		if err != nil {
			return
		}
		out := Format(tr)
		back, err := Parse(out)
		if err != nil {
			t.Fatalf("Format output does not parse: %v\n%q", err, out)
		}
		if !reflect.DeepEqual(back, tr) && !(len(back.Blocks) == 0 && len(tr.Blocks) == 0 && reflect.DeepEqual(back.Files, tr.Files)) {
			t.Fatalf("round trip changed the transcript:\n%#v\n%#v", tr, back)
		}
		if Format(back) != out {
			t.Fatalf("Format is not stable for %q", in)
		}
		if o, err := Parse(other); err == nil {
			for _, a := range tr.Blocks {
				for _, b := range o.Blocks {
					_ = Match(a.Out, b.Out)
					_ = Match(a.Prose, b.Out)
					m := Merge(a.Out, b.Out)
					if allPlain(b.Out) && !Match(m, b.Out) {
						t.Fatalf("Merge(%q, %q) = %q does not match", a.Out, b.Out, m)
					}
					_ = Diff(a.Out, b.Out)
				}
			}
		}
	})
}

func allPlain(lines []string) bool {
	for _, l := range lines {
		if !plainLine(l) {
			return false
		}
	}
	return true
}
