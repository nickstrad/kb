package search

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/nickstrad/kb/internal/store"
)

// t0 is the fixed clock of the suggest fixtures.
var t0 = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// js is one logged search at t0+at by caller; how it went is "fail" (nothing), "useful", "open",
// "other-open", "not-useful" or "none" (a whole-search --none).
func js(id int64, at time.Duration, caller, query, how string) store.JudgedSearch {
	s := store.JudgedSearch{ID: id, TS: t0.Add(at).Format(time.RFC3339), Query: query, Caller: caller, Mode: ModeHybrid, K: 8}
	switch how {
	case "useful":
		s.Verdicts = []store.Verdict{{Rank: 1, Useful: true, EntryPath: "a.md"}}
	case "not-useful":
		s.Verdicts = []store.Verdict{{Rank: 1, Useful: false, EntryPath: "a.md"}}
	case "none":
		s.None = true
	case "open":
		s.Opens = []string{"a.md"}
	case "other-open":
		s.OtherOpens = []store.StorePath{{Store: "x@/y", Path: "b.md"}}
	}
	return s
}

// TestSuggestPairs is the mining decision table: which consecutive searches make a reworded pair.
func TestSuggestPairs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		searches []store.JudgedSearch
		want     string // "a, b" of the one group, "" = none
	}{
		{"fail then useful", []store.JudgedSearch{js(1, 0, "c", "k8s deploy", "fail"), js(2, time.Minute, "c", "kubernetes deploy", "useful")}, "k8s, kubernetes"},
		{"fail then open", []store.JudgedSearch{js(1, 0, "c", "k8s", "fail"), js(2, time.Minute, "c", "kubernetes", "open")}, "k8s, kubernetes"},
		{"fail then other store's open", []store.JudgedSearch{js(1, 0, "c", "k8s", "fail"), js(2, time.Minute, "c", "kubernetes", "other-open")}, "k8s, kubernetes"},
		{"not-useful then useful", []store.JudgedSearch{js(1, 0, "c", "k8s", "not-useful"), js(2, time.Minute, "c", "kubernetes", "useful")}, "k8s, kubernetes"},
		{"--none then useful", []store.JudgedSearch{js(1, 0, "c", "k8s", "none"), js(2, time.Minute, "c", "kubernetes", "useful")}, "k8s, kubernetes"},
		{"exactly the window", []store.JudgedSearch{js(1, 0, "c", "k8s", "fail"), js(2, 5*time.Minute, "c", "kubernetes", "useful")}, "k8s, kubernetes"},
		{"one second past the window", []store.JudgedSearch{js(1, 0, "c", "k8s", "fail"), js(2, 5*time.Minute+time.Second, "c", "kubernetes", "useful")}, ""},
		{"second earlier than first", []store.JudgedSearch{js(1, time.Minute, "c", "k8s", "fail"), js(2, 0, "c", "kubernetes", "useful")}, ""},
		{"both useful", []store.JudgedSearch{js(1, 0, "c", "k8s", "useful"), js(2, time.Minute, "c", "kubernetes", "useful")}, ""},
		{"second failed too", []store.JudgedSearch{js(1, 0, "c", "k8s", "fail"), js(2, time.Minute, "c", "kubernetes", "fail")}, ""},
		{"second only not-useful", []store.JudgedSearch{js(1, 0, "c", "k8s", "fail"), js(2, time.Minute, "c", "kubernetes", "not-useful")}, ""},
		{"different callers", []store.JudgedSearch{js(1, 0, "a", "k8s", "fail"), js(2, time.Minute, "b", "kubernetes", "useful")}, ""},
		{"other caller in between does not break it", []store.JudgedSearch{js(1, 0, "a", "k8s", "fail"), js(2, 30*time.Second, "b", "zzz", "fail"), js(3, time.Minute, "a", "kubernetes", "useful")}, "k8s, kubernetes"},
		{"only the immediate successor counts", []store.JudgedSearch{js(1, 0, "a", "k8s", "fail"), js(2, 30*time.Second, "a", "kube", "fail"), js(3, time.Minute, "a", "kubernetes", "useful")}, "kube, kubernetes"},
		{"word added only", []store.JudgedSearch{js(1, 0, "c", "deploy", "fail"), js(2, time.Minute, "c", "deploy kubernetes", "useful")}, ""},
		{"two words changed", []store.JudgedSearch{js(1, 0, "c", "k8s rollout", "fail"), js(2, time.Minute, "c", "kubernetes deploy", "useful")}, ""},
		{"same query", []store.JudgedSearch{js(1, 0, "c", "k8s", "fail"), js(2, time.Minute, "c", "K8s,", "useful")}, ""},
		{"case and punctuation normalised", []store.JudgedSearch{js(1, 0, "c", "K8S deploy", "fail"), js(2, time.Minute, "c", "Kubernetes, deploy", "useful")}, "k8s, kubernetes"},
		{"bad timestamp", []store.JudgedSearch{{ID: 1, TS: "garbage", Query: "k8s", Caller: "c"}, js(2, time.Minute, "c", "kubernetes", "useful")}, ""},
		{"bad second timestamp", []store.JudgedSearch{js(1, 0, "c", "k8s", "fail"), {ID: 2, TS: "garbage", Query: "kubernetes", Caller: "c", Opens: []string{"a.md"}}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Suggest(tc.searches, nil, SuggestOptions{})
			line := ""
			if len(got.Suggestions) > 0 {
				line = got.Suggestions[0].Line
			}
			if line != tc.want || len(got.Suggestions) > 1 {
				t.Errorf("suggestions = %+v, want %q", got.Suggestions, tc.want)
			}
			if wantPairs := min(len(got.Suggestions), 1); got.Pairs != wantPairs {
				t.Errorf("pairs = %d, want %d", got.Pairs, wantPairs)
			}
		})
	}
}

// TestSuggestRanking: counts, callers, direction, document counts, typo marks, MinPairs, groups
// already in synonyms, and the order.
func TestSuggestRanking(t *testing.T) {
	var log []store.JudgedSearch
	id := int64(0)
	pair := func(caller, from, to string, at time.Duration) {
		id++
		log = append(log, js(id, at, caller, from, "fail"))
		id++
		log = append(log, js(id, at+time.Minute, caller, to, "useful"))
	}
	pair("a", "k8s", "kubernetes", 0)
	pair("b", "k8s", "kubernetes", time.Hour)
	pair("a", "kubernetes", "k8s", 2*time.Hour) // the other direction counts for the same group
	pair("a", "rollout", "deploy", 3*time.Hour)
	pair("b", "rollout", "deploy", 4*time.Hour)
	pair("a", "kuberntes", "kubernetes", 5*time.Hour) // a typo
	pair("a", "pod", "container", 6*time.Hour)        // already a synonym group
	df := map[string]int{"kubernetes": 12, "deploy": 4}
	syn := ParseSynonyms([]byte("pod, container"))

	got := Suggest(log, df, SuggestOptions{Synonyms: syn})
	if got.Searches != len(log) || got.Pairs != 6 {
		t.Errorf("searches = %d pairs = %d, want %d and 6", got.Searches, got.Pairs, len(log))
	}
	var lines []string
	for _, s := range got.Suggestions {
		lines = append(lines, fmt.Sprintf("%s|%d|%d|%v|%v|%v", s.Line, s.Pairs, s.Callers, s.From, s.Doc, s.Typo))
	}
	want := []string{
		"k8s, kubernetes|3|2|map[k8s:2 kubernetes:1]|map[k8s:0 kubernetes:12]|false",
		"deploy, rollout|2|2|map[rollout:2]|map[deploy:4 rollout:0]|false",
		"kubernetes, kuberntes|1|1|map[kuberntes:1]|map[kubernetes:12 kuberntes:0]|true",
	}
	if !slices.Equal(lines, want) {
		t.Errorf("suggestions:\n got %q\nwant %q", lines, want)
	}

	if got := Suggest(log, df, SuggestOptions{Synonyms: syn, MinPairs: 2}); len(got.Suggestions) != 2 || got.Pairs != 6 {
		t.Errorf("MinPairs 2: %d groups (pairs %d), want 2 (pairs still 6)", len(got.Suggestions), got.Pairs)
	}
	if got := Suggest(log, df, SuggestOptions{Synonyms: syn, Window: 30 * time.Second}); len(got.Suggestions) != 0 || got.Window != "30s" {
		t.Errorf("30s window: %+v", got)
	}
}

// TestSuggestGaps: words of failed searches with no document, counted per search, ordered and
// capped; a word of a search that worked is never a gap.
func TestSuggestGaps(t *testing.T) {
	log := []store.JudgedSearch{
		js(1, 0, "a", "wombat wombat k8s", "fail"),
		js(2, time.Hour, "b", "wombat kubernetes", "none"),
		js(3, 2*time.Hour, "a", "quokka", "useful"),
		js(4, 3*time.Hour, "a", "aardvark", "fail"),
	}
	df := map[string]int{"kubernetes": 3}
	got := Suggest(log, df, SuggestOptions{})
	want := []Gap{{"wombat", 2}, {"aardvark", 1}, {"k8s", 1}}
	if !slices.Equal(got.Gaps, want) {
		t.Errorf("gaps = %v, want %v", got.Gaps, want)
	}
	if got := Suggest(log, df, SuggestOptions{MaxGaps: 1}); !slices.Equal(got.Gaps, want[:1]) {
		t.Errorf("MaxGaps 1: %v", got.Gaps)
	}
	if got := Suggest(nil, nil, SuggestOptions{}); got.Suggestions == nil || got.Gaps == nil || got.Window != "5m0s" {
		t.Errorf("empty log: %+v (want non-nil empty lists and the default window)", got)
	}
}

func TestSuggestTerms(t *testing.T) {
	got := SuggestTerms([]store.JudgedSearch{{Query: "K8s, deploy"}, {Query: "deploy (pods)"}})
	if want := []string{"deploy", "k8s", "pods"}; !slices.Equal(got, want) {
		t.Errorf("SuggestTerms = %v, want %v", got, want)
	}
}

// TestSuggestTies: equal pair counts order by more callers, then by line; gaps tie on the term; a group
// is a typo when either direction is (pod -> pods is not, pods -> pod is); two searches logged in
// the same second still pair.
func TestSuggestTies(t *testing.T) {
	log := []store.JudgedSearch{
		js(1, 0, "a", "zeta", "fail"), js(2, time.Minute, "a", "omega", "useful"),
		js(3, time.Hour, "a", "beta", "fail"), js(4, time.Hour+time.Minute, "a", "alpha", "useful"),
		js(5, 2*time.Hour, "b", "pod", "fail"), js(6, 2*time.Hour, "b", "pods", "useful"),
		js(7, 3*time.Hour, "c", "pods", "fail"), js(8, 3*time.Hour+time.Minute, "c", "pod", "useful"),
		js(9, 4*time.Hour, "a", "beta", "fail"), js(10, 4*time.Hour+time.Minute, "a", "alpha", "useful"),
		js(11, 5*time.Hour, "a", "gamma", "fail"), js(12, 5*time.Hour+time.Minute, "a", "delta", "useful"),
	}
	got := Suggest(log, nil, SuggestOptions{})
	var lines []string
	for _, s := range got.Suggestions {
		lines = append(lines, fmt.Sprintf("%s|%d|%d|%v", s.Line, s.Pairs, s.Callers, s.Typo))
	}
	if want := []string{"pod, pods|2|2|true", "alpha, beta|2|1|false", "delta, gamma|1|1|false", "omega, zeta|1|1|false"}; !slices.Equal(lines, want) {
		t.Errorf("order: got %q, want %q", lines, want)
	}
	if want := []Gap{{"beta", 2}, {"gamma", 1}, {"pod", 1}, {"pods", 1}, {"zeta", 1}}; !slices.Equal(got.Gaps, want) {
		t.Errorf("gaps = %v, want %v", got.Gaps, want)
	}
}

// TestIsTypo is the typo rule: letters only on both sides, within spell's distance, not equal.
func TestIsTypo(t *testing.T) {
	for _, tc := range []struct {
		from, to string
		want     bool
	}{
		{"kuberntes", "kubernetes", true},
		{"pods", "pod", true},
		{"pod", "pods", false},      // 3 letters: spell never corrects it
		{"pods", "pods", false},     // equal
		{"podz", "pod1", false},     // a digit on one side
		{"pod1", "podz", false},     // a digit on the other
		{"widget", "wigdte", false}, // 2 edits for 6 letters
	} {
		if got := isTypo(tc.from, tc.to); got != tc.want {
			t.Errorf("isTypo(%q, %q) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}
