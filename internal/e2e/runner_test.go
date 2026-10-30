package e2e

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeKb stands in for the kb binary without building it or using host tools: a symlink named kb
// to this test binary, which TestMain runs as fakeKbMain (it echoes its arguments, working
// directory and selected environment), so the runner's exec path is tested on its own.
func fakeKb(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), fakeKbName)
	if err := os.Symlink(self, p); err != nil {
		t.Fatal(err)
	}
	return p
}

func runText(t *testing.T, r *Runner, text string) ([]CmdResult, error) {
	t.Helper()
	tr, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	return r.Run(t.TempDir(), tr)
}

func TestRunnerStreamsExitAndNormalisation(t *testing.T) {
	r := &Runner{Kb: fakeKb(t)}
	res, err := runText(t, r, `  $ kb fail
  $ kb quiet
  $ kb blank
  $ kb version
  $ kb run "two words" 'x y'
`)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"partial", "[stderr] kb fail: broken at $WORK/proj", "[exit 1]"},
		nil,
		{"a", "", "b"},
		{"kb (version)"},
		{"args: run two words x y", "$WORK/proj"},
	}
	for i, w := range want {
		if !reflect.DeepEqual(res[i].Actual, w) && !(len(w) == 0 && len(res[i].Actual) == 0) {
			t.Errorf("command %d (%s): %q, want %q", i+1, res[i].Cmd, res[i].Actual, w)
		}
	}
}

func TestRunnerHermeticEnv(t *testing.T) {
	r := &Runner{Kb: fakeKb(t), Editor: "/path/to/e2e.test"}
	res, err := runText(t, r, "  $ kb env\n")
	if err != nil {
		t.Fatal(err)
	}
	got := res[0].Actual[0]
	for _, want := range []string{"HOME=$WORK/home", "NOW=" + "(time)", "PATH=$WORK/bin", "EDITOR=/path/to/e2e.test", "PROXY=http://127.0.0.1:1"} {
		if !strings.Contains(got, want) {
			t.Errorf("env line %q lacks %q", got, want)
		}
	}
}

func TestRunnerBuiltins(t *testing.T) {
	r := &Runner{Kb: fakeKb(t)}
	res, err := runText(t, r, `  $ mkdir notes
  $ write notes/a.md src
  $ cat notes/a.md
  $ grep '^b' notes/a.md
  $ grep zzz notes/a.md
  $ cd notes
  $ exists a.md
  $ exists *.md
  $ ! exists a.md
  $ ! exists nope.md
  $ exists nope.md
  $ rm a.md
  $ exists a.md
  $ cd ..
  $ cd missing
  $ write x.md @nope
  $ env KB_HARNESS_NOW=+200d
  $ kb env
  $ env KB_HARNESS_NOW=
  $ frobnicate
  $ mkfixture index-v5 db/kb.sqlite
  -- src --
  not a section
-- src --
a
b line
`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range res {
		got = append(got, strings.Join(c.Actual, " | "))
	}
	check := func(i int, wantLine string) {
		t.Helper()
		if got[i] != wantLine {
			t.Errorf("command %d (%s) = %q, want %q", i+1, res[i].Cmd, got[i], wantLine)
		}
	}
	check(0, "")
	check(2, "a | b line")
	check(3, "b line")
	check(4, "[stderr] grep: no line of notes/a.md matches zzz | [exit 1]")
	check(8, "[stderr] exists: a.md matches 1 path(s) | [exit 1]")
	check(10, "[stderr] exists: nope.md matches nothing | [exit 1]")
	check(12, "[stderr] exists: a.md matches nothing | [exit 1]")
	check(14, "[stderr] cd: missing is not a directory | [exit 1]")
	check(15, `[stderr] no file section "@nope" | [exit 1]`)
	if !strings.Contains(got[17], "NOW=(time)") {
		t.Errorf("env after +200d: %q", got[17])
	}
	check(19, "[stderr] frobnicate: not found: not kb and not a builtin | [exit 1]")
	check(20, "[stderr] mkfixture: no fixture loader | [exit 1]")
}

func TestRunnerRelativeClock(t *testing.T) {
	for _, tc := range []struct{ cur, rel, want string }{
		{BaseNow, "+200d", "2026-09-17T09:00:00Z"},
		{BaseNow, "+36h", "2026-03-02T21:00:00Z"},
		{BaseNow, "+5m", "2026-03-01T09:05:00Z"},
	} {
		if got, err := advance(tc.cur, tc.rel); err != nil || got != tc.want {
			t.Errorf("advance(%s, %s) = %q, %v; want %q", tc.cur, tc.rel, got, err, tc.want)
		}
	}
	if _, err := advance("garbage", "+1d"); err == nil {
		t.Error("garbage clock accepted")
	}
	if _, err := advance(BaseNow, "+99999999999999999999d"); err == nil {
		t.Error("overflowing step accepted")
	}
}

func TestRunnerMkfixtureAndSectionsAtRoot(t *testing.T) {
	var gotName, gotDst string
	r := &Runner{Kb: fakeKb(t), Fixture: func(a, d string) error {
		if gotName == "" {
			gotName, gotDst = a, d
		}
		if a == "bad" {
			return errors.New("no such fixture")
		}
		return os.WriteFile(d, nil, 0o644)
	}}
	res, err := runText(t, r, `  $ mkfixture index-v5 .kb/kb.sqlite
  $ exists .kb/kb.sqlite
  $ exists $WORK/notes/n.md
  $ mkfixture bad x
-- notes/n.md --
hi
`)
	if err != nil {
		t.Fatal(err)
	}
	if gotName != "index-v5" || !strings.HasSuffix(gotDst, "/proj/.kb/kb.sqlite") {
		t.Errorf("fixture called with %q, %q", gotName, gotDst)
	}
	if len(res[1].Actual) != 0 || len(res[2].Actual) != 0 {
		t.Errorf("exists failed: %q %q", res[1].Actual, res[2].Actual)
	}
	if want := []string{"[stderr] no such fixture", "[exit 1]"}; !reflect.DeepEqual(res[3].Actual, want) {
		t.Errorf("bad fixture: %q", res[3].Actual)
	}
}

func TestRunnerWeightsGuard(t *testing.T) {
	_, err := runText(t, &Runner{Kb: fakeKb(t)}, "  $ kb weights\n")
	if err == nil || !strings.Contains(err.Error(), "model.onnx") {
		t.Errorf("guard error = %v", err)
	}
}

func TestSplitArgs(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
		err  bool
	}{
		{`kb a  b`, []string{"kb", "a", "b"}, false},
		{`kb "a b" 'c "d"'`, []string{"kb", "a b", `c "d"`}, false},
		{`kb ""`, []string{"kb", ""}, false},
		{`kb $WORK/x`, []string{"kb", "/w/x"}, false},
		{`kb 'open`, nil, true},
		{``, nil, false},
	} {
		got, err := splitArgs(tc.in, "/w")
		if (err != nil) != tc.err || (!tc.err && !(reflect.DeepEqual(got, tc.want) || (len(got) == 0 && len(tc.want) == 0))) {
			t.Errorf("splitArgs(%q) = %q, %v; want %q (err %v)", tc.in, got, err, tc.want, tc.err)
		}
	}
}

// TestUpdateRoundTrip: -update on a matching transcript changes nothing, a changed expectation is
// rewritten, prose/commands/elisions are kept, and a rerun then passes.
func TestUpdateRoundTrip(t *testing.T) {
	r := &Runner{Kb: fakeKb(t)}
	src := `A story.

  $ kb run one
  args: run one
  ~ .*/proj

Next step.

  $ kb fail
  old output
  ...
  [exit 1]
`
	tr, _ := Parse(src)
	res, err := r.Run(t.TempDir(), tr)
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].OK || res[1].OK {
		t.Fatalf("OK flags = %v %v", res[0].OK, res[1].OK)
	}
	if rep := res[1].Report("story"); !strings.Contains(rep, "journey story, command 2: $ kb fail") || !strings.Contains(rep, "- old output") || !strings.Contains(rep, "+ partial") {
		t.Errorf("report:\n%s", rep)
	}
	up, err := Updated(tr, res)
	if err != nil {
		t.Fatal(err)
	}
	text := Format(up)
	want := `A story.

  $ kb run one
  args: run one
  ~ .*/proj

Next step.

  $ kb fail
  ...
  partial
  [stderr] kb fail: broken at $WORK/proj
  [exit 1]
`
	if text != want {
		t.Errorf("updated transcript:\n%s\nwant:\n%s", text, want)
	}
	again, _ := Parse(text)
	res2, err := r.Run(t.TempDir(), again)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res2 {
		if !c.OK {
			t.Errorf("rerun fails: %s", c.Report("story"))
		}
	}
	up2, _ := Updated(again, res2)
	if Format(up2) != text {
		t.Error("-update on an unchanged tree changed the transcript")
	}
}

func TestUpdatedRefusesAmbiguousOutput(t *testing.T) {
	tr, _ := Parse("  $ kb x\n")
	for _, l := range []string{"$ rm -rf", "...", "~ x", "[stderr] ~ x"} {
		if _, err := Updated(tr, []CmdResult{{Actual: []string{l}}}); err == nil {
			t.Errorf("Updated accepted output line %q", l)
		}
	}
}

func TestNoWeights(t *testing.T) {
	d := t.TempDir()
	if err := NoWeights(d); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(d, "w.safetensors"), nil, 0o644)
	if err := NoWeights(d); err == nil {
		t.Error("weights not found")
	}
}

func TestNoWeightsUnwalkable(t *testing.T) {
	if err := NoWeights(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("an unwalkable work dir passed the weights guard")
	}
}

// TestRunnerEnvUnsetThenSet: unsetting a variable and setting it again lists it once in kb's
// environment (Q117).
func TestRunnerEnvUnsetThenSet(t *testing.T) {
	r := &Runner{Kb: fakeKb(t)}
	res, err := runText(t, r, "  $ env ZZ=1\n  $ env ZZ=\n  $ env ZZ=2\n  $ kb countzz\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := res[len(res)-1].Actual; len(got) != 1 || got[0] != "1" {
		t.Errorf("ZZ listed %v times, want once", got)
	}
}

// TestRunnerTimeoutReported: a kb that outlives Timeout says so on stderr, not only [exit -1].
func TestRunnerTimeoutReported(t *testing.T) {
	r := &Runner{Kb: fakeKb(t), Timeout: 200 * time.Millisecond}
	res, err := runText(t, r, "  $ kb sleep\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(res[0].Actual, "\n"); !strings.Contains(got, "e2e: kb timed out after") {
		t.Errorf("actual = %q, want the timeout line", got)
	}
}
