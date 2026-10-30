package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/fake"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/eval"
)

// TestEvalWiring: a judged fts search is replayed and scored; --mode, -k, --caller and --since
// reach the service; the human form has the PLAN headline; eval never writes the log.
func TestEvalWiring(t *testing.T) {
	root := useRoot(t, newExplainRepo(t))
	id := searchID(t, "tmux logout", "--caller", "claude")
	if _, stderr, exit := runKB(t, []string{"feedback", id, "1", "--useful"}); exit != 0 {
		t.Fatalf("feedback: exit %d %s", exit, stderr)
	}
	before := feedbackRows(t, root)

	evalJSON := func(args ...string) eval.Report {
		t.Helper()
		stdout, stderr, exit := runKB(t, append([]string{"eval", "--mode", "fts", "--json"}, args...))
		if exit != 0 || stderr != "" {
			t.Fatalf("eval %v: exit %d stderr %q", args, exit, stderr)
		}
		var rep eval.Report
		if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
			t.Fatalf("eval --json: %v\n%s", err, stdout)
		}
		return rep
	}
	rep := evalJSON()
	if rep.Mode != "fts" || rep.K != 8 || rep.Searches.Total != 1 || rep.Chunk.Queries != 1 || rep.Chunk.MRR == nil ||
		rep.Chunk.MRR.Mean != 1 || !rep.SmallSample || rep.MinJudged != 30 || rep.SettingsHash == "" {
		t.Errorf("report %+v", rep)
	}
	if rep := evalJSON("-k", "3"); rep.K != 3 {
		t.Errorf("-k 3 reached the report as k=%d", rep.K)
	}
	if rep := evalJSON("--caller", "codex"); rep.Searches.Total != 0 {
		t.Errorf("--caller codex kept %d searches", rep.Searches.Total)
	}
	if rep := evalJSON("--since", "2999-01-01"); rep.Searches.Total != 0 {
		t.Errorf("--since in the future kept %d searches", rep.Searches.Total)
	}

	stdout, stderr, exit := runKB(t, []string{"eval", "--mode", "fts", "--per-query"})
	if exit != 0 || stderr != "" {
		t.Fatalf("eval: exit %d stderr %q", exit, stderr)
	}
	for _, want := range []string{"queries=1 hit@8=1.000 [1.000, 1.000] mrr=1.000", "small sample:", "\"tmux logout\""} {
		if !strings.Contains(stdout, want) {
			t.Errorf("human output lacks %q:\n%s", want, stdout)
		}
	}
	if after := feedbackRows(t, root); after != before {
		t.Errorf("eval changed the feedback rows: %d -> %d", before, after)
	}
	if again := searchID(t, "tmux logout"); again != "2" {
		t.Errorf("eval logged its replays: the next search id is %s, want 2", again)
	}
}

// TestEvalErrors: bad flags are usage errors before anything is opened; a missing index is
// NotFound. All exit 1.
func TestEvalErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		repo bool
		want string
	}{
		{"bad mode", []string{"eval", "--mode", "fts-fallback"}, true, `invalid --mode "fts-fallback"`},
		{"bad since", []string{"eval", "--since", "soon"}, true, `invalid --since "soon"`},
		{"baseline name without a baseline flag", []string{"eval", "q"}, true, "a baseline NAME needs --save-baseline or --compare"},
		{"two names", []string{"eval", "a", "b", "--compare"}, true, "accepts at most 1 arg"},
		{"no index", []string{"eval"}, false, "no index at"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.repo {
				useRoot(t, newExplainRepo(t))
			} else {
				useRoot(t, t.TempDir())
			}
			_, stderr, exit := runKB(t, tc.args)
			if exit != ExitUsage || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d stderr %q, want exit %d and %q", exit, stderr, ExitUsage, tc.want)
			}
		})
	}
}

// TestEvalCountsOtherStoreGold (Q77): a verdict on another store's hit of a cross-store search is
// counted as other stores' gold and never scored against this index; kb eval replays this store
// only.
func TestEvalCountsOtherStoreGold(t *testing.T) {
	newFanOutWorld(t)
	stdout, stderr, code := run(t, "search", "zebracorn", "--stores", "notes", "--json")
	if code != 0 {
		t.Fatalf("search: %d %s", code, stderr)
	}
	var doc struct {
		SearchID int64 `json:"search_id"`
		Hits     []struct {
			Rank  int    `json:"rank"`
			Store string `json:"store"`
		} `json:"hits"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatal(err)
	}
	rank := 0
	for _, h := range doc.Hits {
		if h.Store == "notes" {
			rank = h.Rank
			break
		}
	}
	if rank == 0 {
		t.Fatalf("no notes hit: %s", stdout)
	}
	if _, stderr, code := run(t, "feedback", fmt.Sprint(doc.SearchID), fmt.Sprint(rank), "--useful"); code != 0 {
		t.Fatalf("feedback: %d %s", code, stderr)
	}
	out, stderr, code := run(t, "eval", "--mode", "fts", "--json")
	if code != 0 {
		t.Fatalf("eval: %d %s", code, stderr)
	}
	var rep eval.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.OtherStore != (eval.OtherStoreGold{Chunks: 1, Queries: 1}) || rep.Chunk.Queries != 0 || rep.Chunk.GoldNotInIndex != 0 {
		t.Errorf("other_store %+v chunk %+v; want the notes verdict counted apart, nothing scored", rep.OtherStore, rep.Chunk)
	}
}

// TestRenderEvalLines: which lines the human report has for which report shape.
func TestRenderEvalLines(t *testing.T) {
	est := &eval.Estimate{Mean: 0.5, Lo: 0.25, Hi: 0.75}
	scored := eval.Variant{Queries: 2, HitAtK: est, MRR: est, NDCG: est}
	one := eval.Variant{Queries: 1, HitAtK: est, MRR: est, NDCG: est}
	for _, tc := range []struct {
		name        string
		rep         eval.Report
		want, never []string
	}{
		{"nothing scored", eval.Report{K: 8},
			[]string{"queries=0: no useful verdicts", "gold not in index: 0 chunks"},
			[]string{"entry:", "opens:", "other stores", "small sample"}},
		{"all variants, small sample", eval.Report{K: 5, Chunk: scored, Entry: scored, Opens: scored, SmallSample: true, MinJudged: 30},
			[]string{"queries=2 hit@5=0.500 [0.250, 0.750]", "entry: queries=2", "opens: queries=2", "(2 queries < eval.min_judged 30)"},
			[]string{"queries=0", "other stores"}},
		{"other stores' gold", eval.Report{K: 8, OtherStore: eval.OtherStoreGold{Chunks: 2, Opens: 1, Queries: 1}},
			[]string{"gold from other stores (not scored; kb eval replays this store only): 2 chunks · 1 opens · 1 queries with no gold here"},
			nil},
		{"one query per variant", eval.Report{K: 8, Chunk: one, Entry: one, Opens: one, Confidence: 0.95, Resamples: 1000, Seed: 1},
			[]string{"queries=1 hit@8", "entry: queries=1", "opens: queries=1", "intervals: 95% bootstrap, 1000 resamples, seed 1"},
			[]string{"queries=0"}},
		{"other stores' opens only", eval.Report{K: 8, OtherStore: eval.OtherStoreGold{Opens: 1}},
			[]string{"gold from other stores (not scored; kb eval replays this store only): 0 chunks · 1 opens"},
			nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := renderEval(&tc.rep, false)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("lacks %q:\n%s", w, got)
				}
			}
			for _, n := range tc.never {
				if strings.Contains(got, n) {
					t.Errorf("has %q:\n%s", n, got)
				}
			}
		})
	}
}

// TestRenderEvalPerQuery: the per-query columns are the first gold rank, "-" for gold missed in
// the top k and "." for a variant with no gold.
func TestRenderEvalPerQuery(t *testing.T) {
	rep := eval.Report{K: 8, PerQuery: []eval.Result{{
		Text: "tmux logout", SearchIDs: []int64{1, 2},
		Chunk: &eval.Score{FirstRank: 3}, Entry: &eval.Score{}, Opens: nil,
	}}}
	got := renderEval(&rep, true)
	if want := `3   -   .   "tmux logout"  (2 searches, 0 gold, 0 opened, 0 negatives in top 8)`; !strings.Contains(got, want) {
		t.Errorf("per-query line lacks %q:\n%s", want, got)
	}
	if strings.Contains(renderEval(&rep, false), "tmux logout") {
		t.Error("per-query lines without --per-query")
	}
}

// countingEmbedders makes testDeps.embedder build fake embedders and counts the builds; down makes
// every Embed call fail as Unavailable (a model that cannot load).
func countingEmbedders(t *testing.T, down bool) *int {
	t.Helper()
	n := 0
	orig := testDeps.embedder
	t.Cleanup(func() { testDeps.embedder = orig })
	testDeps.embedder = func(e config.Embed) (embed.Embedder, error) {
		n++
		if down {
			return unavailableEmbedder{fake.New(e.Model, 768)}, nil
		}
		return fake.New(e.Model, 768), nil
	}
	return &n
}

type unavailableEmbedder struct{ embed.Embedder }

func (unavailableEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errs.New(errs.Unavailable, "test.Embed", "the model cannot be loaded")
}

// TestEvalLoadsModelOnlyToReplay: the embedder is built only when some query has gold of this
// store to replay; an empty or unjudged log needs none (the slow part of kb eval).
func TestEvalLoadsModelOnlyToReplay(t *testing.T) {
	root := newSearchTestRepo(t, testModel768, testDim768)
	useRoot(t, root)
	builds := countingEmbedders(t, false)
	id := searchID(t, "widget")
	*builds = 0
	if _, stderr, exit := runKB(t, []string{"eval"}); exit != 0 || *builds != 0 {
		t.Errorf("unjudged log: exit %d, %d embedder builds, stderr %q; want 0 builds", exit, *builds, stderr)
	}
	mustKB(t, "feedback", id, "1", "--useful")
	if _, stderr, exit := runKB(t, []string{"eval"}); exit != 0 || *builds != 1 {
		t.Errorf("judged log: exit %d, %d embedder builds, stderr %q; want 1", exit, *builds, stderr)
	}
}

// TestEvalFallbackExits2: with the model unavailable a hybrid replay falls back to FTS; the
// report is still printed, a warning says how many replays fell back, and the exit is 2.
func TestEvalFallbackExits2(t *testing.T) {
	root := newSearchTestRepo(t, testModel768, testDim768)
	useRoot(t, root)
	countingEmbedders(t, false)
	id := searchID(t, "widget")
	mustKB(t, "feedback", id, "1", "--useful")
	countingEmbedders(t, true)
	stdout, stderr, exit := runKB(t, []string{"eval"})
	if exit != ExitEmbedder || !strings.Contains(stdout, "queries=1") || !strings.Contains(stderr, "1 of the replays ran --mode fts instead") {
		t.Errorf("exit %d\nstdout %q\nstderr %q; want 2, the report, one fallback warning", exit, stdout, stderr)
	}
}

// TestEvalOpenFailures: each failure before the replay is the command's error, never a report.
// An older or newer index is in TestCommandsOnIndexStates.
func TestEvalOpenFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, root string)
		code  int
		want  string
	}{
		{"broken config", func(t *testing.T, root string) { writeConfig(t, root, "[search\n") }, ExitUsage, "config"},
		{"embed_meta mismatch on replay", func(t *testing.T, root string) {
			id := searchID(t, "widget")
			mustKB(t, "feedback", id, "1", "--useful")
			writeConfig(t, root, "[embed]\nmodel = \"BAAI/bge-small-en-v1.5\"\n")
		}, ExitUsage, "kb reindex --all"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newSearchTestRepo(t, testModel768, testDim768)
			useRoot(t, root)
			countingEmbedders(t, false)
			tc.setup(t, root)
			stdout, stderr, exit := runKB(t, []string{"eval"})
			if exit != tc.code || stdout != "" || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d stdout %q stderr %q; want %d, no report, %q", exit, stdout, stderr, tc.code, tc.want)
			}
		})
	}
}

// TestEvalLoadsSynonyms: a store with synonyms.txt is evaluated with them, as search does: the
// settings hash (and so a baseline) differs from the same run without the file.
func TestEvalLoadsSynonyms(t *testing.T) {
	root := useRoot(t, newExplainRepo(t))
	searchID(t, "tmux logout")
	hash := func() string {
		t.Helper()
		stdout, stderr, exit := runKB(t, []string{"eval", "--mode", "fts", "--json"})
		if exit != 0 {
			t.Fatalf("eval: exit %d %s", exit, stderr)
		}
		var rep eval.Report
		if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
			t.Fatal(err)
		}
		return rep.SettingsHash
	}
	without := hash()
	writeFile(t, filepath.Join(testWS(root).StoreDir, "synonyms.txt"), "logout, signout\n")
	if with := hash(); with == without {
		t.Errorf("settings hash %s ignores synonyms.txt", with)
	}
	writeFile(t, filepath.Join(testWS(root).StoreDir, "synonyms.txt"), strings.Repeat("#", 1<<20+1))
	if _, stderr, exit := runKB(t, []string{"eval", "--mode", "fts"}); exit != ExitUsage || !strings.Contains(stderr, "synonyms.txt") {
		t.Errorf("unreadable synonyms: exit %d stderr %q, want a usage error naming the file", exit, stderr)
	}
}
