package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/render"
	"github.com/nickstrad/kb/internal/search"
	"github.com/nickstrad/kb/internal/store"
)

// markzzDoc has a lede and two sections that all mention markzz, so one search hits one entry
// several times.
func markzzDoc(title, summary, tag string) string {
	return "---\ntitle: " + title + "\nsummary: " + summary + "\ntags: [test]\nupdated: 2026-09-13\n---\n\n" +
		"Lede " + tag + " markzz intro.\n\n" +
		"## Logs\n\nRotate logs with markzz " + tag + " daily.\n\nKeep " + tag + " seven days.\n\n" +
		"## Panes\n\nSplit markzz panes " + tag + ".\n"
}

func newExpandWorld(t *testing.T) fanOutWorld {
	t.Helper()
	w := newFanOutWorld(t)
	for _, f := range []struct{ root, name, title, tag string }{{w.loc, "tmux.md", "Tmux", "PRIMARYTAG"}, {w.other, "far2.md", "Far2", "OTHERTAG"}} {
		src := filepath.Join(t.TempDir(), f.name)
		writeFile(t, src, markzzDoc(f.title, "Markzz notes.", f.tag))
		if _, stderr, code := run(t, "--root", f.root, "add", src); code != 0 {
			t.Fatalf("add %s: %d %q", f.name, code, stderr)
		}
	}
	return w
}

// hitHeaders returns the "[r] ..." header lines of a human search output.
func hitHeaders(out string) []string {
	var hs []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "[") {
			hs = append(hs, l)
		}
	}
	return hs
}

// TestSearchExpandModes: none shows each chunk, section shows the whole section once per section,
// entry shows the whole entry once (the best rank, naming the merged ranks); the repeated context
// line is dropped when expanding; [output] expand is the default and the flag overrides it.
func TestSearchExpandModes(t *testing.T) {
	w := newExpandWorld(t)
	base := []string{"search", "PRIMARYTAG", "--mode", "fts", "--k", "10"}

	none, stderr, code := run(t, append(base, "--expand", "none")...)
	if code != 0 {
		t.Fatalf("none: %d %q", code, stderr)
	}
	nTmux := 0
	for _, h := range hitHeaders(none) {
		if strings.Contains(h, "data/tmux.md") {
			nTmux++
		}
	}
	if nTmux < 3 {
		t.Fatalf("none: %d tmux hits, want 3 (lede, Logs, Panes):\n%s", nTmux, none)
	}
	if !strings.Contains(none, "Tmux › Logs\n") {
		t.Errorf("none output lost the chunk's own context line:\n%s", none)
	}

	section, _, code := run(t, append(base, "--expand", "section")...)
	if code != 0 {
		t.Fatalf("section: %d", code)
	}
	if !strings.Contains(section, "Rotate logs with markzz PRIMARYTAG daily.\n\nKeep PRIMARYTAG seven days.") || strings.Contains(section, "Tmux › Logs") {
		t.Errorf("section output lacks the whole Logs section or repeats its context line:\n%s", section)
	}
	if strings.Count(section, "Split markzz panes PRIMARYTAG.") != 1 {
		t.Errorf("section output shows Panes %d times:\n%s", strings.Count(section, "Split markzz panes PRIMARYTAG."), section)
	}

	entry, _, code := run(t, append(base, "--expand", "entry")...)
	if code != 0 {
		t.Fatalf("entry: %d", code)
	}
	for _, want := range []string{"Lede PRIMARYTAG markzz intro.", "## Logs\nRotate logs with markzz PRIMARYTAG daily.", "## Panes\nSplit markzz panes PRIMARYTAG."} {
		if strings.Count(entry, want) != 1 {
			t.Errorf("entry output has %q %d times, want 1:\n%s", want, strings.Count(entry, want), entry)
		}
	}
	tmuxHeads := 0
	for _, h := range hitHeaders(entry) {
		if strings.Contains(h, "data/tmux.md") {
			tmuxHeads++
			if !strings.Contains(h, " · also [") {
				t.Errorf("merged hit does not name the ranks it absorbed: %q", h)
			}
		}
	}
	if tmuxHeads != 1 {
		t.Errorf("entry output has %d tmux hits, want 1:\n%s", tmuxHeads, entry)
	}

	// [output] expand is the default; --expand overrides it.
	writeConfig(t, w.loc, "[embed]\nmodel = \"none\"\n[output]\nexpand = \"entry\"\n")
	def, _, _ := run(t, base...)
	if bodyOf(def) != bodyOf(entry) {
		t.Errorf("[output] expand = entry differs from --expand entry:\n%s\nvs\n%s", def, entry)
	}
	over, _, _ := run(t, append(base, "--expand", "none")...)
	if bodyOf(over) != bodyOf(none) {
		t.Errorf("--expand none did not override [output] expand:\n%s", over)
	}
}

// TestSearchExpandBudget: the expanded text is the level-3 body, so the budget still holds.
func TestSearchExpandBudget(t *testing.T) {
	newExpandWorld(t)
	base := []string{"search", "PRIMARYTAG", "--mode", "fts", "--expand", "entry"}
	full, _, _ := run(t, append(base, "--budget", "0")...)
	total := render.EstimateTokens(bodyOf(full))
	for _, b := range []int{total, total - 1, total / 2, 30, 10} {
		out, _, code := run(t, append(base, "--budget", strconv.Itoa(b))...)
		if code != 0 {
			t.Fatalf("budget %d: exit %d", b, code)
		}
		if got := render.EstimateTokens(bodyOf(out)); got > b {
			t.Errorf("budget %d: body is %d tokens:\n%s", b, got, out)
		}
	}
	if exact, _, _ := run(t, append(base, "--budget", strconv.Itoa(total))...); bodyOf(exact) != bodyOf(full) {
		t.Errorf("an exact-fit budget changed the output:\n%s", exact)
	}
}

// TestSearchExpandCrossStore: a hit from another store expands from that store's chunks; the
// primary's text never leaks into it.
func TestSearchExpandCrossStore(t *testing.T) {
	newExpandWorld(t)
	out, stderr, code := run(t, "search", "OTHERTAG", "--mode", "fts", "--k", "10", "--stores", "notes", "--expand", "entry")
	if code != 0 {
		t.Fatalf("exit %d %q", code, stderr)
	}
	if !strings.Contains(out, "] notes:data/far2.md") || !strings.Contains(out, "## Panes\nSplit markzz panes OTHERTAG.") {
		t.Fatalf("the other store's entry was not expanded from its own chunks:\n%s", out)
	}
	if strings.Count(out, "Split markzz panes OTHERTAG.") != 1 || strings.Contains(out, "PRIMARYTAG") {
		t.Errorf("the entry is shown more than once or the primary's text leaked in:\n%s", out)
	}
}

// TestSearchExpandJSON: --expand adds an "expanded" field to the hits it expanded, and nothing
// else changes; without it the field is absent.
func TestSearchExpandJSON(t *testing.T) {
	newExpandWorld(t)
	type hit struct {
		Rank     int    `json:"rank"`
		Path     string `json:"path"`
		Text     string `json:"text"`
		Expanded string `json:"expanded"`
	}
	read := func(args ...string) (hits []hit, raw string) {
		out, stderr, code := run(t, append([]string{"search", "PRIMARYTAG", "--mode", "fts", "--json", "--k", "10"}, args...)...)
		if code != 0 {
			t.Fatalf("exit %d %q", code, stderr)
		}
		var v struct{ Hits []hit }
		if err := json.Unmarshal([]byte(out), &v); err != nil {
			t.Fatalf("json: %v\n%s", err, out)
		}
		return v.Hits, out
	}
	plain, raw := read()
	if strings.Contains(raw, `"expanded"`) {
		t.Errorf("no --expand but the JSON has an expanded field:\n%s", raw)
	}
	hits, _ := read("--expand", "entry")
	if len(hits) != len(plain) {
		t.Fatalf("--expand changed the hit count: %d vs %d", len(hits), len(plain))
	}
	for i, h := range hits {
		if h.Text != plain[i].Text || h.Rank != plain[i].Rank {
			t.Errorf("hit %d changed: %+v vs %+v", i, h, plain[i])
		}
		if strings.HasSuffix(h.Path, "tmux.md") && !strings.Contains(h.Expanded, "## Panes\nSplit markzz panes PRIMARYTAG.") {
			t.Errorf("hit %d (%s) has no entry in expanded: %q", h.Rank, h.Path, h.Expanded)
		}
	}
}

func TestSearchExpandInvalid(t *testing.T) {
	newExpandWorld(t)
	out, stderr, code := run(t, "search", "PRIMARYTAG", "--mode", "fts", "--expand", "page")
	if code != 1 || out != "" || !strings.Contains(stderr, `invalid --expand "page"`) {
		t.Errorf("--expand page = %d, out %q, stderr %q; want exit 1 and the usage error", code, out, stderr)
	}
}

// TestSkipReasonNamesStoreOnce (Q85): the skipped-store warning names the store once.
func TestSkipReasonNamesStoreOnce(t *testing.T) {
	w := newFanOutWorld(t)
	writeFile(t, filepath.Join(w.loc, ".kb", "stores", "adr", "config.toml"), "[search\n")
	_, stderr, _ := run(t, "search", "zebracorn", "--stores", "adr")
	if strings.Count(stderr, "adr@") > 1 || strings.Contains(stderr, "adr: store adr") {
		t.Errorf("the warning names the store twice:\n%s", stderr)
	}
	boom := errs.New(errs.Conflict, "sqlite.Open", "boom")
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"SearchStores wrap: the cause", errs.Wrap(boom, errs.Conflict, search.OpSearchStores, "store a@/x: boom"), "boom"},
		{"no wrap: as is", boom, "boom"},
		{"another op: as is", errs.Wrap(boom, errs.Conflict, "cli.fanOut", "store b@/x: boom"), "store b@/x: boom"},
		{"plain error: as is", errors.New("store a@/x: boom"), "store a@/x: boom"},
	} {
		got := skipReason(search.Skipped{Part: search.Part{Ref: "a@/x"}, Err: tc.err})
		if got != tc.want {
			t.Errorf("%s: skipReason = %q, want %q", tc.name, got, tc.want)
		}
	}
}

type chunkStub struct {
	entryStub
	chunks map[int64][]store.ChunkRow
	cerr   error
	cn     int
}

func (s *chunkStub) Chunks(_ context.Context, id int64) ([]store.ChunkRow, error) {
	s.cn++
	return s.chunks[id], s.cerr
}

// TestExpandHitsTable is the decision table for expandHits: which hits expand, which degrade to
// no expansion, and what is returned as an error.
func TestExpandHitsTable(t *testing.T) {
	chunks := []store.ChunkRow{
		{SourceFile: "a.md", Heading: "", Text: "A: s"},
		{SourceFile: "a.md", Heading: "X", Text: "A › X\none"},
		{SourceFile: "a.md", Heading: "X", Text: "A › X\ntwo"},
	}
	newStub := func() *chunkStub {
		return &chunkStub{entryStub: entryStub{rows: map[string]store.EntryRow{"a.md": {ID: 7}}}, chunks: map[int64][]store.ChunkRow{7: chunks}}
	}
	hit := func(store, path, text string) search.Hit {
		return search.Hit{Rank: 1, Store: store, Path: path, Heading: "X", Text: text}
	}
	boom := errors.New("boom")
	for _, tc := range []struct {
		name    string
		mode    string
		lookups func(*chunkStub) map[string]entryLookup
		hit     search.Hit
		want    string
		key     bool
		err     error
		calls   int
	}{
		{"none reads nothing", "none", func(s *chunkStub) map[string]entryLookup { return map[string]entryLookup{"": s} }, hit("", "a.md", chunks[1].Text), "", false, nil, 0},
		{"section", "section", func(s *chunkStub) map[string]entryLookup { return map[string]entryLookup{"": s} }, hit("", "a.md", chunks[1].Text), "one\n\ntwo", true, nil, 1},
		{"entry", "entry", func(s *chunkStub) map[string]entryLookup { return map[string]entryLookup{"": s} }, hit("", "a.md", chunks[2].Text), "A: s\n\n## X\none\n\ntwo", true, nil, 1},
		{"hit on chunk 0 (index 0) expands", "section", func(s *chunkStub) map[string]entryLookup { return map[string]entryLookup{"": s} },
			search.Hit{Rank: 1, Path: "a.md", Heading: "", Text: chunks[0].Text}, "A: s", true, nil, 1},
		{"cross-store hit uses its own store", "entry", func(s *chunkStub) map[string]entryLookup {
			return map[string]entryLookup{"": &chunkStub{}, "n": s}
		}, hit("n", "a.md", chunks[1].Text), "A: s\n\n## X\none\n\ntwo", true, nil, 1},
		{"store with no lookup", "entry", func(s *chunkStub) map[string]entryLookup { return map[string]entryLookup{"": s} }, hit("lost", "a.md", chunks[1].Text), "", false, nil, 0},
		{"lookup without chunks", "entry", func(s *chunkStub) map[string]entryLookup { return map[string]entryLookup{"": &s.entryStub} }, hit("", "a.md", chunks[1].Text), "", false, nil, 0},
		{"entry gone", "entry", func(s *chunkStub) map[string]entryLookup { return map[string]entryLookup{"": s} }, hit("", "gone.md", chunks[1].Text), "", false, nil, 0},
		{"chunk changed since the search", "entry", func(s *chunkStub) map[string]entryLookup { return map[string]entryLookup{"": s} }, hit("", "a.md", "old text"), "", false, nil, 1},
		{"chunks error", "entry", func(s *chunkStub) map[string]entryLookup { s.cerr = boom; return map[string]entryLookup{"": s} }, hit("", "a.md", chunks[1].Text), "", false, boom, 1},
		{"entries error", "entry", func(s *chunkStub) map[string]entryLookup { s.err = boom; return map[string]entryLookup{"": s} }, hit("", "a.md", chunks[1].Text), "", false, boom, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub()
			got, err := expandHits(context.Background(), tc.lookups(s), []search.Hit{tc.hit}, tc.mode)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if err == nil && (got[0].Text != tc.want || (got[0].Key != "") != tc.key) {
				t.Errorf("got %+v, want text %q key=%v", got[0], tc.want, tc.key)
			}
			if s.cn != tc.calls {
				t.Errorf("Chunks called %d times, want %d", s.cn, tc.calls)
			}
		})
	}
	// One entry's chunks are read once however many hits it has.
	s := newStub()
	_, err := expandHits(context.Background(), map[string]entryLookup{"": s}, []search.Hit{hit("", "a.md", chunks[1].Text), hit("", "a.md", chunks[2].Text)}, "section")
	if err != nil || s.cn != 1 {
		t.Errorf("two hits of one entry: err %v, %d Chunks calls, want 1", err, s.cn)
	}
}

// TestWarnSkippedKinds (Q84, Q85): a skipped extra store's warning names the store once; an
// Internal skip also writes an incident and names its id; NotFound and Conflict write none.
func TestWarnSkippedKinds(t *testing.T) {
	cache := t.TempDir()
	orig := testDeps.userCacheDir
	t.Cleanup(func() { testDeps.userCacheDir = orig })
	testDeps.userCacheDir = func() (string, error) { return cache, nil }
	for _, tc := range []struct {
		kind     errs.Kind
		incident bool
	}{{errs.Internal, true}, {errs.NotFound, false}, {errs.Conflict, false}} {
		t.Run(tc.kind.String(), func(t *testing.T) {
			_ = os.Remove(filepath.Join(cache, "kb", "kb.log"))
			part := search.Part{Ref: "adr@/loc", Label: "adr"}
			cause := errs.New(tc.kind, "sqlite.Open", "it broke")
			skipped := []search.Skipped{{Part: part, Err: errs.Wrap(cause, tc.kind, search.OpSearchStores, "store adr@/loc: it broke")}}
			cmd := &cobra.Command{Use: "search"}
			cmd.Flags().Bool("debug", false, "")
			var buf strings.Builder
			cmd.SetErr(&buf)
			testDeps.warnSkipped(cmd, skipped)
			got := buf.String()
			if !strings.HasPrefix(got, "warning: skipping store adr: it broke") || strings.Contains(got, "adr@/loc") {
				t.Errorf("stderr %q: want the store named once, as adr", got)
			}
			incs := readIncidents(t, filepath.Join(cache, "kb", "kb.log"))
			if tc.incident != (len(incs) == 1 && strings.Contains(got, "kb bug: id "+incs[0].ID)) || (!tc.incident && len(incs) != 0) {
				t.Errorf("incidents %+v, stderr %q; want incident=%v", incs, got, tc.incident)
			}
		})
	}
}
