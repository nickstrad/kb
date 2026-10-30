package reindex

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed/fake"
	"github.com/nickstrad/kb/internal/entry"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
)

// TestLinks is the table of what becomes an edge: front-matter lists against the front door's own
// directory, body links against their own file's directory, wikilinks as written, self links
// dropped, repeats written once, a secondary file with a broken fence skipped.
func TestLinks(t *testing.T) {
	dirEntry := entry.Entry{Path: "data/droplet/README.md", Kind: entry.KindDir, Dir: "data/droplet",
		Files: []string{"data/droplet/README.md", "data/droplet/docs/setup.md"}}
	for _, tc := range []struct {
		name  string
		e     entry.Entry
		meta  entry.FrontMatter
		files map[string]string
		want  []string
	}{
		{
			name:  "front-matter lists resolve against the front door's directory, rel = key",
			e:     dirEntry,
			meta:  entry.FrontMatter{Related: []string{"x.md"}, Supersedes: []string{"../old.md", "[[Old Name]]"}, DependsOn: []string{"https://example.com"}},
			files: map[string]string{"data/droplet/README.md": "---\ntitle: D\n---\n", "data/droplet/docs/setup.md": ""},
			want:  []string{"data/droplet/x.md related path", "data/old.md supersedes path", "Old Name supersedes wikilink"},
		},
		{
			name: "body links resolve against their own file; self links dropped; repeats once",
			e:    dirEntry,
			files: map[string]string{
				"data/droplet/README.md":     "---\ntitle: D\nsummary: s\n---\n[setup](docs/setup.md) [me](README.md) [dir](.) [other](../other) [o2](../other)\n",
				"data/droplet/docs/setup.md": "[back](../README.md) [sib](more.md) [[Wiki Page|label]] [[Wiki Page]]\n```\n[fenced](no.md)\n```\n",
			},
			want: []string{"data/other links path", "data/droplet/docs/more.md links path", "Wiki Page links wikilink"},
		},
		{
			name:  "a file entry's own path is a self link",
			e:     entry.Entry{Path: "data/a.md", Kind: entry.KindFile, Files: []string{"data/a.md"}},
			files: map[string]string{"data/a.md": "[self](a.md#x) [b](b.md) [[a]]\n"},
			want:  []string{"data/b.md links path", "a links wikilink"},
		},
		{
			name: "a secondary file with a broken fence adds nothing",
			e:    dirEntry,
			files: map[string]string{
				"data/droplet/README.md":     "[ok](ok.md)\n",
				"data/droplet/docs/setup.md": "---\ntitle: never closed\n[lost](lost.md)\n",
			},
			want: []string{"data/droplet/ok.md links path"},
		},
		{
			name:  "no links",
			e:     entry.Entry{Path: "data/a.md", Kind: entry.KindFile, Files: []string{"data/a.md"}},
			files: map[string]string{"data/a.md": "plain text\n"},
			want:  nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := tc.e
			e.Meta = tc.meta
			files := map[string][]byte{}
			for k, v := range tc.files {
				files[k] = []byte(v)
			}
			var got []string
			for _, g := range Links(e, files) {
				got = append(got, g.DstPath+" "+g.Rel+" "+g.Kind)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("Links =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

// edgeLines renders the store's edges as "src -rel/kind-> dst => resolved".
func edgeLines(t *testing.T, st store.Index) []string {
	t.Helper()
	es, err := st.Edges(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, fmt.Sprintf("%s -%s/%s-> %s => %s", e.SrcPath, e.Rel, e.Kind, e.DstPath, e.DstEntryPath))
	}
	return out
}

// TestAllWritesEdgesAliasesAndTags: a reindex writes every entry's edges, aliases and tags and
// resolves the edges (by path, directory, basename and alias, a broken link left dangling);
// `--tag Docker` and `--tag docker` filter alike (the PLAN 5.2 accept); an alias is found by
// full-text search; One re-resolves after a new entry appears, and All after one goes.
func TestAllWritesEdgesAliasesAndTags(t *testing.T) {
	root := t.TempDir()
	writeRel(t, root, "data/a.md", "---\ntitle: Alpha\nsummary: first\ntags: [Docker, go]\naliases: [Zebrafish handbook]\nrelated: [b.md]\nsupersedes: [\"[[Gamma Alias]]\"]\n---\n\n## Body\n\nsee [the ship](ship) and [[b]] and [gone](gone.md)\n")
	writeRel(t, root, "data/b.md", "---\ntitle: Beta\nsummary: second\ntags: [docker]\n---\n\n## Body\n\nlinks to [[later]]\n")
	writeRel(t, root, "data/ship/README.md", "---\ntitle: Ship\nsummary: a dir entry\naliases: [Gamma Alias]\n---\n\n## Body\n\nship body, see [setup](docs/setup.md)\n")
	writeRel(t, root, "data/ship/docs/setup.md", "# Setup\n\nback to [a](../../a.md)\n")
	base := fake.New("fake-embed", testDim)
	s := newTestStore(t, base)
	ctx := context.Background()
	o := Options{Root: root, Sources: dataSources, Store: s, Embedder: base}
	if _, err := All(ctx, o); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"data/a.md -links/path-> data/gone.md => ",
		"data/a.md -links/path-> data/ship => data/ship/README.md",
		"data/a.md -links/wikilink-> b => data/b.md",
		"data/a.md -related/path-> data/b.md => data/b.md",
		"data/a.md -supersedes/wikilink-> Gamma Alias => data/ship/README.md",
		"data/b.md -links/wikilink-> later => ",
		"data/ship/README.md -links/path-> data/a.md => data/a.md",
	}
	if got := edgeLines(t, s); !slices.Equal(got, want) {
		t.Fatalf("edges after All =\n%q\nwant\n%q", got, want)
	}

	a, err := s.Entry(ctx, "data/a.md")
	if err != nil || a == nil || !slices.Equal(a.Aliases, []string{"Zebrafish handbook"}) || !slices.Equal(a.Tags, []string{"Docker", "go"}) {
		t.Fatalf("Entry(data/a.md) = %+v, %v; want its aliases and tags as written", a, err)
	}
	ids := func(tag string, terms ...string) []int64 {
		t.Helper()
		res, err := s.FTS(ctx, store.FTSQuery{Terms: terms, Op: store.OpOr, Tag: tag, Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, r := range res.Rows {
			out = append(out, r.ChunkID)
		}
		slices.Sort(out)
		return out
	}
	upper, lower := ids("Docker", "body"), ids("docker", "body")
	if len(upper) == 0 || !slices.Equal(upper, lower) {
		t.Errorf("--tag Docker = %v, --tag docker = %v; want the same non-empty rows", upper, lower)
	}
	if got := ids("", "zebrafish"); len(got) == 0 {
		t.Error("the alias Zebrafish handbook is not found by full-text search")
	}

	// A new entry named by a dangling wikilink: One resolves it.
	writeRel(t, root, "data/later.md", "---\ntitle: Later\nsummary: arrives late\n---\n\n## Body\n\nlate\n")
	e, err := entry.Resolve(root, dataSources, "later.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := One(ctx, o, e); err != nil {
		t.Fatal(err)
	}
	if got := edgeLines(t, s); !slices.Contains(got, "data/b.md -links/wikilink-> later => data/later.md") {
		t.Errorf("edges after One(later.md) = %q; want b's wikilink resolved", got)
	}

	// b.md goes: All prunes it, and the edges to it dangle.
	if err := os.Remove(filepath.Join(root, "data", "b.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := All(ctx, o); err != nil {
		t.Fatal(err)
	}
	got := edgeLines(t, s)
	for _, line := range []string{"data/a.md -related/path-> data/b.md => ", "data/a.md -links/wikilink-> b => "} {
		if !slices.Contains(got, line) {
			t.Errorf("edges after b.md was removed = %q; want %q", got, line)
		}
	}
}

// TestAllDiscoversTwiceBeforePruning is the Q22 race through the Discover seam: an entry a
// concurrent `kb add` writes after All's first discovery is in Entries but not in that discovery;
// the second discovery sees it, so it is kept, while a row whose file is really gone is pruned.
func TestAllDiscoversTwiceBeforePruning(t *testing.T) {
	root := newTestRepo(t)
	base := fake.New("fake-embed", testDim)
	s := newTestStore(t, base)
	ctx := context.Background()
	if _, err := s.ReplaceEntry(ctx, store.EntryInput{Path: "data/gone.md", Kind: entry.KindFile, Title: "Gone", Summary: "s"}, nil); err != nil {
		t.Fatal(err)
	}
	calls := 0
	// After the first discovery, a concurrent `kb add` writes data/late.md: the file, then the
	// row, through the same database, as the other process would.
	firstThenAdd := func(r string, src []config.Source) ([]entry.Entry, []entry.Skipped, error) {
		es, sk, err := entry.Discover(r, src)
		calls++
		if calls == 1 {
			writeRel(t, root, "data/late.md", lateEntry)
			e, rerr := entry.Resolve(root, dataSources, "late.md")
			if rerr != nil {
				t.Fatal(rerr)
			}
			if _, rerr := One(ctx, Options{Root: root, Sources: dataSources, Store: s, Embedder: base}, e); rerr != nil {
				t.Fatal(rerr)
			}
		}
		return es, sk, err
	}
	var stdout bytes.Buffer
	sum, err := All(ctx, Options{Root: root, Sources: dataSources, Store: s, Embedder: base, Stdout: &stdout, Discover: firstThenAdd})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("Discover ran %d times, want 2 (once more before pruning)", calls)
	}
	if row, err := s.Entry(ctx, "data/late.md"); err != nil || row == nil {
		t.Fatalf("data/late.md = %v, %v; the concurrently added entry was pruned", row, err)
	}
	if row, _ := s.Entry(ctx, "data/gone.md"); row != nil || sum.Removed != 1 {
		t.Errorf("data/gone.md = %v, Removed = %d; want it pruned, alone", row, sum.Removed)
	}
}

// TestPruneSet: a stored row is pruned only when neither discovery found it.
func TestPruneSet(t *testing.T) {
	es := func(paths ...string) []entry.Entry {
		var out []entry.Entry
		for _, p := range paths {
			out = append(out, entry.Entry{Path: p})
		}
		return out
	}
	for _, tc := range []struct {
		name       string
		stored     []string
		first, now []entry.Entry
		wantPruned []string
	}{
		{"nothing stored", nil, es("a"), nil, nil},
		{"all found first", []string{"a", "b"}, es("a", "b"), nil, nil},
		{"gone from both", []string{"a", "b", "c"}, es("a"), es("a"), []string{"b", "c"}},
		{"found only in the second", []string{"a", "late"}, es("a"), es("a", "late"), nil},
		{"found only in the first (removed since): kept this run", []string{"a"}, es("a"), nil, nil},
		{"no second discovery", []string{"a", "b"}, es("a"), nil, []string{"b"}},
		{"stored order kept", []string{"z", "a", "m"}, nil, nil, []string{"z", "a", "m"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pruneSet(tc.stored, tc.first, tc.now); !slices.Equal(got, tc.wantPruned) {
				t.Errorf("pruneSet = %q, want %q", got, tc.wantPruned)
			}
		})
	}
}

// failingResolve is a store whose ResolveEdges fails with a Conflict.
type failingResolve struct{ store.Index }

func (failingResolve) ResolveEdges(context.Context) error {
	return errs.New(errs.Conflict, "test.ResolveEdges", "resolve refused")
}

// TestResolveFailureEndsRun: a ResolveEdges failure is the run's error, kind kept, for All and One;
// the entries are already written.
func TestResolveFailureEndsRun(t *testing.T) {
	root := newTestRepo(t)
	base := fake.New("fake-embed", testDim)
	s := newTestStore(t, base)
	ctx := context.Background()
	o := Options{Root: root, Sources: dataSources, Store: failingResolve{s}, Embedder: base}
	sum, err := All(ctx, o)
	if errs.KindOf(err) != errs.Conflict || sum.Reindexed == 0 {
		t.Errorf("All = %+v, %v (kind %v); want the entries written, then the Conflict", sum, err, errs.KindOf(err))
	}
	e, err := entry.Resolve(root, dataSources, "tmux-daemons-die-at-logout.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := One(ctx, o, e); errs.KindOf(err) != errs.Conflict {
		t.Errorf("One = %v (kind %v), want the Conflict", err, errs.KindOf(err))
	}
}

// TestAllSecondDiscovery is the decision around All's second Discover: it runs only when some
// stored row was not found by the first; a single candidate that the second finds is kept; its
// failure is the run's error and nothing is pruned.
func TestAllSecondDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name       string
		late       bool // a concurrent add after the first discovery
		secondErr  bool
		wantCalls  int
		wantErr    bool
		wantLate   bool
		wantRemove int
	}{
		{"nothing to prune: one discovery", false, false, 1, false, false, 0},
		{"one candidate, found the second time: kept", true, false, 2, false, true, 0},
		{"second discovery fails: error, nothing pruned", true, true, 2, true, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newTestRepo(t)
			base := fake.New("fake-embed", testDim)
			s := newTestStore(t, base)
			ctx := context.Background()
			calls := 0
			discover := func(r string, src []config.Source) ([]entry.Entry, []entry.Skipped, error) {
				calls++
				if calls == 2 && tc.secondErr {
					return nil, nil, errs.New(errs.Usage, "test.Discover", "source unreadable")
				}
				es, sk, err := entry.Discover(r, src)
				if calls == 1 && tc.late {
					writeRel(t, root, "data/late.md", lateEntry)
					e, rerr := entry.Resolve(root, dataSources, "late.md")
					if rerr != nil {
						t.Fatal(rerr)
					}
					if _, rerr := One(ctx, Options{Root: root, Sources: dataSources, Store: s, Embedder: base}, e); rerr != nil {
						t.Fatal(rerr)
					}
				}
				return es, sk, err
			}
			sum, err := All(ctx, Options{Root: root, Sources: dataSources, Store: s, Embedder: base, Discover: discover})
			if (err != nil) != tc.wantErr || calls != tc.wantCalls || sum.Removed != tc.wantRemove {
				t.Errorf("All = %+v, %v after %d discoveries; want err=%v, %d discoveries, %d removed", sum, err, calls, tc.wantErr, tc.wantCalls, tc.wantRemove)
			}
			if row, _ := s.Entry(ctx, "data/late.md"); (row != nil) != tc.wantLate {
				t.Errorf("data/late.md present = %v, want %v", row != nil, tc.wantLate)
			}
		})
	}
}
