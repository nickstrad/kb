package search

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
)

// TestParseSynonyms is the file-format table: comments, blanks, duplicates, overlap, case,
// whitespace, unicode and groups too small to mean anything.
func TestParseSynonyms(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want map[string][]string
	}{
		{"empty", "", map[string][]string{}},
		{"only comments and blanks", "# x\n\n   \n  # y, z\n", map[string][]string{}},
		{"one group", "k8s, kubernetes\n", map[string][]string{"k8s": {"kubernetes"}, "kubernetes": {"k8s"}}},
		{"three members", "a,b,c", map[string][]string{"a": {"b", "c"}, "b": {"a", "c"}, "c": {"a", "b"}}},
		{"case and spacing", "  K8S ,  Kubernetes  \r\n", map[string][]string{"k8s": {"kubernetes"}, "kubernetes": {"k8s"}}},
		{"inline hash is part of a member", "c#, csharp # not a comment", map[string][]string{"c#": {"csharp # not a comment"}, "csharp # not a comment": {"c#"}}},
		{"multi-word member collapses spaces", "k8s,  kube \t cluster", map[string][]string{"k8s": {"kube cluster"}, "kube cluster": {"k8s"}}},
		{"empty members dropped", "a,,b,", map[string][]string{"a": {"b"}, "b": {"a"}}},
		{"single member ignored", "alone\n,\nx,x\n", map[string][]string{}},
		{"duplicate group", "a,b\nb,a\na,b", map[string][]string{"a": {"b"}, "b": {"a"}}},
		{"overlapping groups do not merge", "a,b\nb,c", map[string][]string{"a": {"b"}, "b": {"a", "c"}, "c": {"b"}}},
		{"unicode folds case", "Straße, STRASSE, Ünï", map[string][]string{
			"straße": {"strasse", "ünï"}, "strasse": {"straße", "ünï"}, "ünï": {"straße", "strasse"}}},
		{"no trailing newline, comment last", "a,b\n# end", map[string][]string{"a": {"b"}, "b": {"a"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseSynonyms([]byte(tc.in))
			if len(got.byTerm) != len(tc.want) {
				t.Fatalf("terms = %v, want %v", got.byTerm, tc.want)
			}
			for term, alts := range tc.want {
				if !slices.Equal(got.byTerm[term], alts) {
					t.Errorf("%q -> %v, want %v", term, got.byTerm[term], alts)
				}
			}
			if got.Len() != len(tc.want) {
				t.Errorf("Len = %d, want %d", got.Len(), len(tc.want))
			}
		})
	}
}

// TestSynonymsHash: equal groups hash equal whatever the comments, case, spacing and order; other
// groups, and no groups, differ.
func TestSynonymsHash(t *testing.T) {
	base := ParseSynonyms([]byte("k8s, kubernetes\nvm, virtual machine\n")).Hash()
	if base == "" {
		t.Fatal("hash of real groups is empty")
	}
	for _, same := range []string{
		"# note\nvm,virtual   machine\n\nK8S,KUBERNETES",
		"vm, virtual machine\nk8s, kubernetes\nk8s, kubernetes",
		"kubernetes, k8s\nvirtual machine, vm", // member order: the alternatives are ORed
	} {
		if got := ParseSynonyms([]byte(same)).Hash(); got != base {
			t.Errorf("hash of %q = %s, want %s", same, got, base)
		}
	}
	for _, other := range []string{"k8s, kubernetes", "k8s, kubernetes\nvm, virtual machine, box", ""} {
		if got := ParseSynonyms([]byte(other)).Hash(); got == base {
			t.Errorf("hash of %q equals the base hash", other)
		}
	}
	// Overlapping groups in either line order expand the same, so they hash the same.
	if a, b := ParseSynonyms([]byte("a, b, c\nc, d")).Hash(), ParseSynonyms([]byte("c, d\nb, a, c")).Hash(); a != b {
		t.Errorf("overlapping groups hash by line order: %s != %s", a, b)
	}
	var nilSyn *Synonyms
	if nilSyn.Hash() != "" || nilSyn.Len() != 0 || nilSyn.Lookup("x") != nil || nilSyn.Expand([]string{"x"}) != nil {
		t.Error("nil Synonyms is not empty")
	}
}

// TestSynonymsLookup is the decision table for one query field.
func TestSynonymsLookup(t *testing.T) {
	syn := ParseSynonyms([]byte("k8s, kubernetes\ndb, database, datastore\nkube cluster, k8s"))
	for _, tc := range []struct {
		field string
		want  []string
	}{
		{"k8s", []string{"kubernetes", "kube cluster"}}, // two groups: both contribute
		{"K8S", []string{"kubernetes", "kube cluster"}}, // case folds
		{"Kubernetes,", []string{"k8s"}},                // surrounding punctuation
		{"(db)", []string{"database", "datastore"}},
		{"datastore.", []string{"db", "database"}},
		{"kube", nil},                     // a member of a multi-word phrase is not the phrase
		{"cluster", nil},                  // ditto
		{"kube cluster", []string{"k8s"}}, // a phrase key exists, though no query field has a space
		{"nothing", nil},
		{"", nil},
		{"...", nil},
	} {
		if got := syn.Lookup(tc.field); !slices.Equal(got, tc.want) {
			t.Errorf("Lookup(%q) = %v, want %v", tc.field, got, tc.want)
		}
	}
}

// TestSynonymsLookupCap: a huge group expands to at most MaxAlternatives, the first ones.
func TestSynonymsLookupCap(t *testing.T) {
	members := make([]string, MaxAlternatives+10)
	for i := range members {
		members[i] = fmt.Sprintf("m%d", i)
	}
	syn := ParseSynonyms([]byte(strings.Join(members, ",")))
	got := syn.Lookup("m0")
	if len(got) != MaxAlternatives || got[0] != "m1" || got[MaxAlternatives-1] != fmt.Sprintf("m%d", MaxAlternatives) {
		t.Errorf("Lookup = %v, want m1..m%d", got, MaxAlternatives)
	}
}

// TestExpand: the map is keyed by the field as typed, only for fields with synonyms.
func TestExpand(t *testing.T) {
	syn := ParseSynonyms([]byte("k8s, kubernetes"))
	got := syn.Expand([]string{"Run", "K8S", "pods"})
	if len(got) != 1 || !slices.Equal(got["K8S"], []string{"kubernetes"}) {
		t.Errorf("Expand = %v, want only K8S -> kubernetes", got)
	}
	if syn.Expand([]string{"run"}) != nil {
		t.Error("Expand with no synonym is not nil")
	}
}

// TestLoadSynonyms: a good file loads; a missing one is NotFound; a directory, a FIFO, a device and
// an oversized file are refused unread with a Usage error naming the file.
func TestLoadSynonyms(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "synonyms.txt")
	if err := os.WriteFile(good, []byte("a, b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	syn, err := LoadSynonyms(good)
	if err != nil || syn.Len() != 2 {
		t.Fatalf("LoadSynonyms(good) = %v, %v; want 2 terms", syn, err)
	}

	if _, err := LoadSynonyms(filepath.Join(dir, "missing.txt")); errs.KindOf(err) != errs.NotFound {
		t.Errorf("missing file: kind = %v (%v), want NotFound", errs.KindOf(err), err)
	}

	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	zero := filepath.Join(dir, "zero")
	if err := os.Symlink("/dev/zero", zero); err != nil {
		t.Fatal(err)
	}
	huge := filepath.Join(dir, "huge.txt")
	if err := os.WriteFile(huge, []byte(strings.Repeat("a, b\n", config.MaxFileBytes/5+10)), 0o644); err != nil {
		t.Fatal(err)
	}
	exact := filepath.Join(dir, "exact.txt") // exactly at the cap is fine
	if err := os.WriteFile(exact, []byte(strings.Repeat("#", config.MaxFileBytes)), 0o644); err != nil {
		t.Fatal(err)
	}
	if syn, err := LoadSynonyms(exact); err != nil || syn.Len() != 0 {
		t.Errorf("file of exactly MaxFileBytes: %v, %v; want empty and no error", syn, err)
	}
	for name, path := range map[string]string{"directory": dir, "fifo": fifo, "device symlink": zero, "oversized": huge} {
		_, err := LoadSynonyms(path)
		if errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), path) {
			t.Errorf("%s: err = %v (kind %v), want a Usage error naming %s", name, err, errs.KindOf(err), path)
		}
	}
}

// synSearcher is testSearcher with synonyms.
func synSearcher(t testing.TB, text string) *Searcher {
	s, _ := testSearcher(t)
	s.Synonyms = ParseSynonyms([]byte(text))
	return s
}

// TestSearchSynonymExpansion is the decision table of expansion on the main FTS list: the matches,
// the OR retry, and no effect where there is no group.
func TestSearchSynonymExpansion(t *testing.T) {
	for _, tc := range []struct {
		name      string
		syn       string
		query     string
		wantMatch string // the FTSMatch
		wantOr    bool
		wantHits  bool
		wantAlts  map[string][]string
	}{
		{"no synonyms: nothing found", "", "wombat", `"wombat"`, false, false, nil},
		{"synonym finds the quokka chunk", "wombat, quokka", "wombat", `("wombat" OR "quokka")`, false, true, map[string][]string{"wombat": {"quokka"}}},
		{"group of the term in the middle, AND kept", "wombat, quokka", "wombat enclosure", `("wombat" OR "quokka") AND "enclosure"`, false, true, map[string][]string{"wombat": {"quokka"}}},
		{"multi-word synonym is a phrase", "wombat, quokka widget", "wombat", `("wombat" OR "quokka widget")`, false, true, map[string][]string{"wombat": {"quokka widget"}}},
		{"OR retry is expanded too", "wombat, quokka", "wombat fluffernutter", `("wombat" OR "quokka") OR "fluffernutter"`, true, true, map[string][]string{"wombat": {"quokka"}}},
		{"unrelated group changes nothing", "alpha, beta", "quokka", `"quokka"`, false, true, nil},
		{"operator-only alternative is dropped", "wombat, or", "wombat", `"wombat"`, false, false, map[string][]string{"wombat": {"or"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := synSearcher(t, tc.syn)
			out, err := s.Search(context.Background(), Request{Query: tc.query, Mode: ModeFTS, K: 8})
			if err != nil {
				t.Fatal(err)
			}
			if out.FTSMatch != tc.wantMatch || out.FTSFallbackToOr != tc.wantOr {
				t.Errorf("match = %q or=%v, want %q or=%v", out.FTSMatch, out.FTSFallbackToOr, tc.wantMatch, tc.wantOr)
			}
			if got := len(out.Result.Hits) > 0; got != tc.wantHits {
				t.Errorf("hits = %d, want hits=%v", len(out.Result.Hits), tc.wantHits)
			}
			if fmt.Sprint(out.FTSSynonyms) != fmt.Sprint(tc.wantAlts) {
				t.Errorf("FTSSynonyms = %v, want %v", out.FTSSynonyms, tc.wantAlts)
			}
		})
	}
}

// TestSynonymsNeverReachTheExactList: an identifier in a group expands in the main list only
// (the W9 rule); the exact list still searches the identifier alone.
func TestSynonymsNeverReachTheExactList(t *testing.T) {
	s := synSearcher(t, "foo_bar, quokka")
	out, err := s.Search(context.Background(), Request{Query: "foo_bar", Mode: ModeFTS, K: 8})
	if err != nil {
		t.Fatal(err)
	}
	if want := `("foo_bar" OR "quokka")`; out.FTSMatch != want {
		t.Errorf("main match = %q, want %q", out.FTSMatch, want)
	}
	if out.ExactMatch != `"foo_bar"` {
		t.Errorf("exact match = %q, want only \"foo_bar\"", out.ExactMatch)
	}
}

// TestSynonymsVectorQueryUnchanged: the text that is embedded is the raw query.
func TestSynonymsVectorQueryUnchanged(t *testing.T) {
	plain, _ := testSearcher(t)
	withSyn := synSearcher(t, "wombat, quokka")
	a, err := plain.Search(context.Background(), Request{Query: "wombat", Mode: ModeVec, K: 8})
	if err != nil {
		t.Fatal(err)
	}
	b, err := withSyn.Search(context.Background(), Request{Query: "wombat", Mode: ModeVec, K: 8})
	if err != nil {
		t.Fatal(err)
	}
	ja, _ := json.Marshal(a.Result.Hits)
	jb, _ := json.Marshal(b.Result.Hits)
	if b.FTSMatch != "" || b.FTSSynonyms != nil || string(ja) != string(jb) {
		t.Errorf("vec search changed by synonyms: match=%q alts=%v", b.FTSMatch, b.FTSSynonyms)
	}
}

// TestExplainShowsSynonyms: the expanded expression and the term map reach the explanation, and
// the per-column re-scoring (which reruns the expression) finds the expanded hit.
func TestExplainShowsSynonyms(t *testing.T) {
	s := synSearcher(t, "wombat, quokka")
	ex, err := s.Explain(context.Background(), Request{Query: "wombat", Mode: ModeFTS, K: 8})
	if err != nil {
		t.Fatal(err)
	}
	if ex.FTS == nil || ex.FTS.Match != `("wombat" OR "quokka")` || !slices.Equal(ex.FTS.Synonyms["wombat"], []string{"quokka"}) {
		t.Fatalf("explain fts = %+v, want the expanded match and wombat -> quokka", ex.FTS)
	}
	if len(ex.Candidates) == 0 || ex.Candidates[0].Columns == nil || ex.Candidates[0].Columns.Text == nil {
		t.Errorf("candidate has no per-column scores: %+v", ex.Candidates)
	}
}

func TestSettingsWithSynonyms(t *testing.T) {
	base, err := DefaultSettings()
	if err != nil {
		t.Fatal(err)
	}
	if got := base.WithSynonyms(""); got.Hash != base.Hash {
		t.Error("no synonyms changed the hash")
	}
	a, b := base.WithSynonyms("h1"), base.WithSynonyms("h2")
	if a.Hash == base.Hash || a.Hash == b.Hash || a.Hash != base.WithSynonyms("h1").Hash {
		t.Errorf("hashes: base=%s h1=%s h2=%s", base.Hash, a.Hash, b.Hash)
	}
}

// ftsChunkSet is the set of chunk ids an FTS query returns.
func ftsChunkSet(t testing.TB, s *Searcher, terms []string, alts map[string][]string, op store.FTSOp) map[int64]bool {
	t.Helper()
	res, err := s.Store.FTS(context.Background(), store.FTSQuery{Terms: terms, Op: op, Limit: 4096, Alts: alts})
	if err != nil {
		t.Fatalf("fts %v %v: %v", terms, alts, err)
	}
	set := map[int64]bool{}
	for _, r := range res.Rows {
		set[r.ChunkID] = true
	}
	return set
}

var synonymVocab = []string{"widget", "zebracorn", "quokka", "enclosure", "gate", "latch", "docker", "bridge", "networks", "dns", "alpha", "beta", "nothing", "k8s", "wombat"}

// TestSynonymExpansionIsASuperset is the property: for random queries and random groups,
// expanding never drops a chunk the unexpanded query found (AND and OR alike).
func TestSynonymExpansionIsASuperset(t *testing.T) {
	s, _ := testSearcher(t)
	r := rand.New(rand.NewSource(55))
	pick := func() string { return synonymVocab[r.Intn(len(synonymVocab))] }
	for i := 0; i < 200; i++ {
		var terms []string
		for n := 1 + r.Intn(3); n > 0; n-- {
			terms = append(terms, pick())
		}
		var lines []string
		for n := r.Intn(4); n > 0; n-- {
			lines = append(lines, pick()+", "+pick()+", "+pick()+" "+pick())
		}
		alts := ParseSynonyms([]byte(strings.Join(lines, "\n"))).Expand(terms)
		for _, op := range []store.FTSOp{store.OpAnd, store.OpOr} {
			plain := ftsChunkSet(t, s, terms, nil, op)
			expanded := ftsChunkSet(t, s, terms, alts, op)
			for id := range plain {
				if !expanded[id] {
					t.Fatalf("terms %v alts %v op %v: chunk %d found without expansion, lost with it", terms, alts, op, id)
				}
			}
		}
	}
}

// FuzzSynonyms: any file bytes parse without panic, every lookup stays within the cap and never
// names the term itself, and the expanded query is valid FTS5 (the store does not error) and a
// superset of the plain one.
func FuzzSynonyms(f *testing.F) {
	f.Add([]byte("k8s, kubernetes\n# c\nwidget,quokka"), "k8s widget")
	f.Add([]byte("\"a\", NEAR, OR, *, ((\n"), `"a" OR (NEAR* ) `)
	f.Add([]byte("\x00,\xff\xfe,é"), "é \x00")
	f.Add([]byte(""), "")
	s, _ := testSearcher(f)
	f.Fuzz(func(t *testing.T, file []byte, query string) {
		syn := ParseSynonyms(file)
		fields := strings.Fields(query)
		for _, fld := range fields {
			alts := syn.Lookup(fld)
			if len(alts) > MaxAlternatives {
				t.Fatalf("Lookup(%q) has %d alternatives", fld, len(alts))
			}
		}
		alts := syn.Expand(fields)
		plain := ftsChunkSet(t, s, fields, nil, store.OpAnd)
		expanded := ftsChunkSet(t, s, fields, alts, store.OpAnd)
		for id := range plain {
			if !expanded[id] {
				t.Fatalf("chunk %d lost by expanding %q with %q", id, query, file)
			}
		}
		ftsChunkSet(t, s, fields, alts, store.OpOr)
	})
}

// TestParseSynonymsLargeGroup: a huge group or a file at config.MaxFileBytes parses in linear time
// and memory (each term keeps its first MaxAlternatives alternatives, all Lookup returns). Before
// the W10 pass one 10,000-member line took minutes (a cubic "already added" check).
func TestParseSynonymsLargeGroup(t *testing.T) {
	var big strings.Builder
	for i := range 10000 {
		fmt.Fprintf(&big, "w%d,", i)
	}
	var pairs strings.Builder
	for i := 0; pairs.Len() < config.MaxFileBytes-16; i++ {
		fmt.Fprintf(&pairs, "a%d,b%d\n", i, i)
	}
	for _, tc := range []struct {
		name, text, field string
		want              int
	}{
		{"one 10k-member line", big.String(), "w5000", MaxAlternatives},
		{"one 10k-member line, first member", big.String(), "w0", MaxAlternatives},
		{"a MaxFileBytes file of pairs", pairs.String(), "a7", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan *Synonyms, 1)
			go func() { done <- ParseSynonyms([]byte(tc.text)) }()
			select {
			case syn := <-done:
				if got := syn.Lookup(tc.field); len(got) != tc.want {
					t.Errorf("Lookup(%s) = %d alternatives, want %d", tc.field, len(got), tc.want)
				}
			case <-time.After(10 * time.Second): // liveness bound, not a timing assertion
				t.Fatal("ParseSynonyms did not finish in 10s: not linear")
			}
		})
	}
	if got := ParseSynonyms([]byte(big.String())).Lookup("w0"); got[0] != "w1" || got[MaxAlternatives-1] != fmt.Sprint("w", MaxAlternatives) {
		t.Errorf("Lookup(w0) = %q, want the first %d other members in file order", got, MaxAlternatives)
	}
}
