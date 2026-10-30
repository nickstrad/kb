package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/fake"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/render"
	"github.com/nickstrad/kb/internal/search"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/store/sqlite"
)

// newLinkedRepo is the explain repo plus a hub entry that links to some of it:
//
//	hub.md -supersedes-> tmux.md, hub.md -related-> ssh.md, hub.md -links-> gone.md (dangling),
//	ssh.md -depends_on-> git.md
//
// The hub's text matches "logout" too, so a search for it returns hub.md, tmux.md and ssh.md.
func newLinkedRepo(t *testing.T) string {
	t.Helper()
	root := newExplainRepo(t)
	ctx := context.Background()
	st, err := sqlite.Open(ctx, testDBPath(root), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	emb := fake.New(testModel768, testDim768)
	put := func(path, text string, edges ...store.EdgeInput) {
		vecs, err := emb.Embed(ctx, []string{embed.DocText(emb, text)})
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(text))
		in := store.EntryInput{Path: path, Kind: "file", Title: path, Summary: "fixture", BodyHash: "hash-" + path, Edges: edges}
		if _, err := st.ReplaceEntry(ctx, in, []store.ChunkInput{{Ord: 0, SourceFile: path, Text: text, TextHash: hex.EncodeToString(sum[:]), Embedding: vecs[0]}}); err != nil {
			t.Fatal(err)
		}
	}
	e := func(dst, rel string) store.EdgeInput {
		return store.EdgeInput{DstPath: dst, Rel: rel, Kind: store.EdgePath}
	}
	put("hub.md", "Hub: where a logout is handled, see the tmux and ssh pages", e("tmux.md", "supersedes"), e("ssh.md", "related"), e("gone.md", "links"))
	put("ssh.md", "SSH Config: keepalive settings stop a logout from dropping the link", e("git.md", "depends_on"))
	if err := st.ResolveEdges(ctx); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestRelatedCommand(t *testing.T) {
	useRoot(t, newLinkedRepo(t))

	t.Run("human, depth 1", func(t *testing.T) {
		stdout, _ := mustKB(t, "related", "ssh.md")
		want := "ssh.md · 2 links\n" +
			"→  depends_on  git.md\n" +
			"←  related     hub.md\n"
		if stdout != want {
			t.Errorf("stdout =\n%q\nwant\n%q", stdout, want)
		}
	})
	t.Run("human, depth 2 names the entry it came through, and a dangling link", func(t *testing.T) {
		stdout, _ := mustKB(t, "related", "ssh.md", "--depth", "2")
		for _, want := range []string{"→  supersedes  tmux.md  via hub.md", "→  links       gone.md  (unresolved) via hub.md"} {
			if !strings.Contains(stdout, want) {
				t.Errorf("stdout lacks %q:\n%s", want, stdout)
			}
		}
	})
	t.Run("json", func(t *testing.T) {
		stdout, _ := mustKB(t, "related", "hub.md", "--json")
		var got struct {
			Path  string
			Depth int
			Edges []jsonRelated
		}
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("json: %v\n%s", err, stdout)
		}
		if got.Path != "hub.md" || got.Depth != 1 || len(got.Edges) != 3 {
			t.Fatalf("got %+v, want hub.md depth 1 with 3 edges", got)
		}
		dangling := got.Edges[0] // an unresolved link sorts first: its path is empty
		if dangling.Resolved || dangling.Target != "gone.md" || dangling.Path != "" || dangling.Direction != "out" || dangling.Rel != "links" {
			t.Errorf("first edge = %+v, want the dangling link to gone.md", dangling)
		}
	})
	t.Run("no links", func(t *testing.T) {
		stdout, _ := mustKB(t, "related", "docker.md")
		if stdout != "docker.md · 0 links\nno links\n" {
			t.Errorf("stdout = %q", stdout)
		}
	})
}

func TestRelatedErrors(t *testing.T) {
	useRoot(t, newLinkedRepo(t))
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"depth 0", []string{"related", "hub.md", "--depth", "0"}, "invalid --depth 0"},
		{"depth 4", []string{"related", "hub.md", "--depth", "4"}, "invalid --depth 4"},
		{"unknown entry", []string{"related", "nope.md"}, `no entry "nope.md"`},
		{"no argument", []string{"related"}, "accepts 1 arg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, exit := runKB(t, tc.args)
			if exit == 0 || stdout != "" || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d stdout %q stderr %q; want a failure mentioning %q", exit, stdout, stderr, tc.want)
			}
		})
	}
	t.Run("no index", func(t *testing.T) {
		useRoot(t, t.TempDir())
		_, stderr, exit := runKB(t, []string{"related", "x.md"})
		if exit == 0 || !strings.Contains(stderr, "no index") {
			t.Errorf("exit %d stderr %q, want a no-index failure", exit, stderr)
		}
	})
	// Each step before the walk fails well-formed: the store config, the REF store, the index.
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, root string)
		arg   string
		want  string
	}{
		{"bad store config", func(t *testing.T, root string) { writeConfig(t, root, "[search]\nnope = 1\n") }, "hub.md", "nope"},
		{"unknown REF store", func(t *testing.T, root string) {}, "nope:hub.md", `no store "nope"`},
		{"garbage index", func(t *testing.T, root string) {
			writeFile(t, testDBPath(root), strings.Repeat("not a database ", 512))
		}, "hub.md", "kb.sqlite"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := useRoot(t, newLinkedRepo(t))
			tc.setup(t, root)
			stdout, stderr, exit := runKB(t, []string{"related", tc.arg})
			if exit != ExitUsage || stdout != "" || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d stdout %q stderr %q; want exit %d mentioning %q", exit, stdout, stderr, ExitUsage, tc.want)
			}
		})
	}
}

// TestSearchSupersedeAndNeighbors: the superseded entry is marked and ranked down, kb explain
// shows the penalty, --neighbors appends the Related: section without the hits and without
// logging anything, and search.supersede_penalty=1 restores the plain order.
func TestSearchSupersedeAndNeighbors(t *testing.T) {
	root := useRoot(t, newLinkedRepo(t))
	withFixtureEmbedder(t)

	stdout, _ := mustKB(t, "search", "logout", "--mode", "fts", "--json")
	var res struct {
		Hits []struct {
			Path       string
			Superseded bool
		}
		Neighbors *[]jsonNeighbor
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("json: %v\n%s", err, stdout)
	}
	if res.Neighbors != nil {
		t.Errorf("neighbors present without --neighbors: %s", stdout)
	}
	last := res.Hits[len(res.Hits)-1]
	if last.Path != "tmux.md" || !last.Superseded {
		t.Errorf("last hit = %+v, want the superseded tmux.md (hits %+v)", last, res.Hits)
	}
	for _, h := range res.Hits {
		if h.Superseded != (h.Path == "tmux.md") {
			t.Errorf("hit %s superseded = %v", h.Path, h.Superseded)
		}
	}

	human, _ := mustKB(t, "search", "logout", "--mode", "fts")
	if !strings.Contains(human, " · superseded") {
		t.Errorf("human output lacks the superseded marker:\n%s", human)
	}
	if exp, _ := mustKB(t, "explain", "logout", "--mode", "fts", "--json"); !strings.Contains(exp, `"supersede_penalty": 0.5`) || !strings.Contains(exp, `"superseded": true`) {
		t.Errorf("explain --json lacks the penalty:\n%s", exp)
	}

	// Penalty 1 is the plain order: nothing is marked and tmux.md (two chunks) is not last.
	writeConfig(t, root, "[embed]\nmodel = \""+testModel768+"\"\n[search]\nsupersede_penalty = 1\n")
	if human, _ = mustKB(t, "search", "logout", "--mode", "fts"); strings.Contains(human, "superseded") {
		t.Errorf("penalty 1 still marks:\n%s", human)
	}
}

func TestSearchNeighbors(t *testing.T) {
	root := useRoot(t, newLinkedRepo(t))
	withFixtureEmbedder(t)

	// "keepalive" returns only ssh.md; its neighbours are hub.md and git.md (hub is not a hit).
	stdout, _ := mustKB(t, "search", "keepalive", "--mode", "fts", "-k", "1", "--neighbors")
	want := "\nRelated:\n  [1] → git.md (depends_on), ← hub.md (related)\n"
	if !strings.Contains(stdout, want) || strings.Index(stdout, "Related:") > strings.Index(stdout, "search_id=") {
		t.Errorf("stdout lacks the section before the footer:\n%s", stdout)
	}
	// "logout" returns hub, tmux and ssh: their links to each other are not neighbours; git is.
	stdout, _ = mustKB(t, "search", "logout", "--mode", "fts", "--neighbors")
	if !strings.Contains(stdout, "Related:\n  [") || strings.Contains(stdout, "hub.md (") || strings.Contains(stdout, "ssh.md (related)") || !strings.Contains(stdout, "git.md (depends_on)") {
		t.Errorf("neighbours of hits are listed as neighbours:\n%s", stdout)
	}
	// No neighbours at all: no section.
	stdout, _ = mustKB(t, "search", "docker", "--mode", "fts", "--neighbors")
	if strings.Contains(stdout, "Related:") {
		t.Errorf("an empty neighbour list printed a section:\n%s", stdout)
	}
	js, _ := mustKB(t, "search", "keepalive", "--mode", "fts", "--neighbors", "--json")
	var res struct{ Neighbors *[]jsonNeighbor }
	if err := json.Unmarshal([]byte(js), &res); err != nil || res.Neighbors == nil || len(*res.Neighbors) != 2 {
		t.Fatalf("neighbors = %+v, %v\n%s", res.Neighbors, err, js)
	}
	if n := (*res.Neighbors)[0]; n.Rank != 1 || n.Path != "git.md" || n.Rel != "depends_on" || n.Direction != "out" {
		t.Errorf("first neighbour = %+v", n)
	}
	js, _ = mustKB(t, "search", "docker", "--mode", "fts", "--neighbors", "--json")
	if !strings.Contains(js, `"neighbors": []`) {
		t.Errorf("no neighbours should print an empty list:\n%s", js)
	}

	// Neighbours are not logged: every search_results row is a returned hit.
	st, err := sqlite.Open(context.Background(), testDBPath(root), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tbl, err := st.QueryLog(context.Background(), "SELECT count(*) FROM search_results r JOIN searches s ON s.id = r.search_id WHERE r.entry_path = 'git.md'")
	if err != nil {
		t.Fatal(err)
	}
	if tbl.Rows[0][0].(int64) != 0 {
		t.Errorf("git.md was logged as a result %v times, want 0", tbl.Rows[0][0])
	}
}

func TestRelatedDepthBoundsAndStore(t *testing.T) {
	useRoot(t, newLinkedRepo(t))
	if stdout, _ := mustKB(t, "related", "ssh.md", "--depth", "3"); !strings.Contains(stdout, "ssh.md · 4 links") {
		t.Errorf("depth 3 stdout:\n%s", stdout)
	}
	if _, stderr, exit := runKB(t, []string{"related", "ssh.md", "--store", "nosuchstore"}); exit == 0 || stderr == "" {
		t.Errorf("an unknown --store: exit %d stderr %q, want a failure", exit, stderr)
	}
}

// relaterStub answers Related from a table and records the paths it was asked about.
type relaterStub struct {
	rows  map[string][]store.RelatedRow
	err   map[string]error
	asked []string
}

func (r *relaterStub) Related(_ context.Context, path string, _ int) ([]store.RelatedRow, error) {
	r.asked = append(r.asked, path)
	return r.rows[path], r.err[path]
}

func (r *relaterStub) Superseded(context.Context, []string) (map[string]bool, error) {
	return nil, nil
}

// TestNeighborLines: the decisions around Related: an entry is asked once however many chunks hit
// it, another store's hit is not asked, a removed entry is skipped, other errors surface, and a
// dangling or self link is no neighbour.
func TestNeighborLines(t *testing.T) {
	boom := errs.New(errs.Internal, "test", "boom")
	hits := []search.Hit{
		{Rank: 1, Path: "a.md"}, {Rank: 2, Path: "a.md"}, {Rank: 3, Store: "x", Path: "x.md"}, {Rank: 4, Path: "gone.md"},
	}
	stub := &relaterStub{
		rows: map[string][]store.RelatedRow{"a.md": {
			{Rel: "links", Direction: store.DirOut, Via: "a.md", Path: "b.md"},
			{Rel: "links", Direction: store.DirOut, Via: "a.md", Path: "a.md"}, // a self-link
			{Rel: "links", Direction: store.DirOut, Via: "a.md", Target: "nowhere.md"},
		}},
		err: map[string]error{"gone.md": errs.New(errs.NotFound, "test", "no entry")},
	}
	lines, err := neighborLines(context.Background(), stub, hits)
	if err != nil {
		t.Fatal(err)
	}
	if got := render.FormatNeighbors(lines); got != "Related:\n  [1] → b.md (links)" {
		t.Errorf("section = %q", got)
	}
	if want := []string{"a.md", "gone.md"}; !slices.Equal(stub.asked, want) {
		t.Errorf("asked %v, want %v", stub.asked, want)
	}
	stub.err["a.md"] = boom
	if _, err := neighborLines(context.Background(), stub, hits); !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}
	if lines, err := neighborLines(context.Background(), stub, nil); err != nil || lines == nil || len(lines) != 0 {
		t.Errorf("no hits: %v, %v; want an empty, non-nil list", lines, err)
	}
}
