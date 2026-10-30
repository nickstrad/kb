package eval

import (
	"reflect"
	"testing"

	"github.com/nickstrad/kb/internal/store"
)

func useful(rank int, path, heading string) store.Verdict {
	return store.Verdict{Rank: rank, Useful: true, EntryPath: path, Heading: heading}
}

func notUseful(rank int, path, heading string) store.Verdict {
	return store.Verdict{Rank: rank, EntryPath: path, Heading: heading}
}

// TestExtractDecisions is the gold decision table for one search: which count it lands in and
// what gold it yields. Renamed headings and deleted entries are decided against the index, in
// TestEvaluateGoldInIndex.
func TestExtractDecisions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		search    store.JudgedSearch
		counts    SearchCounts
		useful    []Chunk
		negatives []Chunk
		opens     []string
		noQuery   bool
	}{
		{name: "useful verdict: chunk gold",
			search: store.JudgedSearch{Verdicts: []store.Verdict{useful(2, "a.md", "H")}},
			counts: SearchCounts{Total: 1, Useful: 1}, useful: []Chunk{{"a.md", "H"}}},
		{name: "not-useful only: a negative, no gold, excluded",
			search: store.JudgedSearch{Verdicts: []store.Verdict{notUseful(1, "a.md", "H")}},
			counts: SearchCounts{Total: 1, NotUsefulOnly: 1}, noQuery: true},
		{name: "useful and not-useful: gold plus a negative",
			search: store.JudgedSearch{Verdicts: []store.Verdict{notUseful(1, "b.md", ""), useful(2, "a.md", "H")}},
			counts: SearchCounts{Total: 1, Useful: 1}, useful: []Chunk{{"a.md", "H"}}, negatives: []Chunk{{"b.md", ""}}},
		{name: "--none only: excluded",
			search: store.JudgedSearch{None: true},
			counts: SearchCounts{Total: 1, NoneOnly: 1}, noQuery: true},
		{name: "--none with not-useful verdicts: counted as none",
			search: store.JudgedSearch{None: true, Verdicts: []store.Verdict{notUseful(1, "b.md", "")}},
			counts: SearchCounts{Total: 1, NoneOnly: 1}, noQuery: true},
		{name: "useful beats a stray --none",
			search: store.JudgedSearch{None: true, Verdicts: []store.Verdict{useful(1, "a.md", "")}},
			counts: SearchCounts{Total: 1, Useful: 1}, useful: []Chunk{{"a.md", ""}}},
		{name: "unjudged: excluded",
			search: store.JudgedSearch{},
			counts: SearchCounts{Total: 1, Unjudged: 1}, noQuery: true},
		{name: "opens only: opens gold, never useful gold",
			search: store.JudgedSearch{Opens: []string{"c.md"}},
			counts: SearchCounts{Total: 1, Unjudged: 1, WithOpens: 1}, opens: []string{"c.md"}},
		{name: "--none and an open: opens gold only",
			search: store.JudgedSearch{None: true, Opens: []string{"c.md"}},
			counts: SearchCounts{Total: 1, NoneOnly: 1, WithOpens: 1}, opens: []string{"c.md"}},
		{name: "blank query: counted, never a query",
			search: store.JudgedSearch{Query: " \t", Verdicts: []store.Verdict{useful(1, "a.md", "")}},
			counts: SearchCounts{Total: 1, Useful: 1}, noQuery: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.search
			s.ID = 1
			if s.Query == "" {
				s.Query = "q"
			}
			qs, counts := Extract([]store.JudgedSearch{s})
			if counts != tc.counts {
				t.Errorf("counts %+v, want %+v", counts, tc.counts)
			}
			if tc.noQuery {
				if len(qs) != 0 {
					t.Errorf("queries %+v, want none", qs)
				}
				return
			}
			if len(qs) != 1 {
				t.Fatalf("queries %+v, want one", qs)
			}
			q := qs[0]
			if !reflect.DeepEqual(q.Useful, nonNil(tc.useful)) || !reflect.DeepEqual(q.Negatives, nonNil(tc.negatives)) ||
				!reflect.DeepEqual(q.Opens, nonNilS(tc.opens)) {
				t.Errorf("gold %+v / %+v / %v, want %+v / %+v / %v", q.Useful, q.Negatives, q.Opens, tc.useful, tc.negatives, tc.opens)
			}
		})
	}
}

func nonNil(c []Chunk) []Chunk {
	if c == nil {
		return []Chunk{}
	}
	return c
}

func nonNilS(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// TestExtractGroups: searches of one normalised query merge into one query with the union of
// their gold, the newest text, sorted ids; a chunk useful anywhere in the group is not a negative;
// queries come back sorted by key whatever the log order.
func TestExtractGroups(t *testing.T) {
	qs, counts := Extract([]store.JudgedSearch{
		{ID: 5, Query: "Tmux   Logout", Verdicts: []store.Verdict{useful(1, "b.md", "H2"), notUseful(2, "a.md", "H1")}},
		{ID: 2, Query: "zsh", Verdicts: []store.Verdict{useful(1, "z.md", "")}},
		{ID: 3, Query: "tmux logout ", Verdicts: []store.Verdict{useful(1, "a.md", "H1"), notUseful(3, "c.md", "")}, Opens: []string{"a.md"}},
		{ID: 4, Query: "TMUX logout", Verdicts: []store.Verdict{useful(1, "b.md", "H2")}},
	})
	if counts != (SearchCounts{Total: 4, Useful: 4, WithOpens: 1}) {
		t.Errorf("counts %+v", counts)
	}
	want := []Query{
		{Key: "tmux logout", Text: "Tmux   Logout", SearchIDs: []int64{3, 4, 5},
			Useful:    []Chunk{{"a.md", "H1"}, {"b.md", "H2"}},
			Negatives: []Chunk{{"c.md", ""}},
			Opens:     []string{"a.md"}},
		{Key: "zsh", Text: "zsh", SearchIDs: []int64{2}, Useful: []Chunk{{"z.md", ""}}, Negatives: []Chunk{}, Opens: []string{}},
	}
	if !reflect.DeepEqual(qs, want) {
		t.Errorf("Extract:\n got %+v\nwant %+v", qs, want)
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"":                "",
		"  ":              "",
		"Tmux":            "tmux",
		" tmux\t\nLogout": "tmux logout",
		"ÉCOLE  été":      "école été",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}
