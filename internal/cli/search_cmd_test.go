package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/fake"
	"github.com/nickstrad/kb/internal/embed/ollama"
	"github.com/nickstrad/kb/internal/store"
)

// newSearchTestRepo builds a temp repo root with a populated database: one entry, two chunks,
// both containing "widget" so `kb search widget` matches. embed_meta is recorded with the given
// model/dim so the caller controls whether the CLI's ollama-backed embedder (which always reports
// ollama.DefaultModel/DefaultDim unless KB_EMBED_MODEL/dim override it) is seen as matching.
func newSearchTestRepo(t *testing.T, embedModel string, embedDim int) string {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, ".kb", "kb.sqlite")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	if err := st.EnsureEmbedMeta(embedModel, embedDim); err != nil {
		t.Fatalf("EnsureEmbedMeta: %v", err)
	}

	emb := fake.New("fake-embed-for-fixture", embedDim)
	ctx := context.Background()
	texts := []string{
		"Widget Doc: an entry about the widget subsystem",
		"widget section one: details about configuring the widget",
	}
	docTexts := make([]string, len(texts))
	for i, text := range texts {
		docTexts[i] = embed.DocPrefix + text
	}
	vecs, err := emb.Embed(ctx, docTexts)
	if err != nil {
		t.Fatalf("embed fixture: %v", err)
	}

	chunks := make([]store.ChunkInput, len(texts))
	for i, text := range texts {
		heading := ""
		if i > 0 {
			heading = "Configuration"
		}
		sum := sha256.Sum256([]byte(text))
		chunks[i] = store.ChunkInput{
			Ord:        i,
			SourceFile: "widget.md",
			Heading:    heading,
			Text:       text,
			TextHash:   hex.EncodeToString(sum[:]),
			Embedding:  vecs[i],
		}
	}

	if _, err := st.ReplaceEntryChunks(ctx, store.EntryInput{
		Path:     "widget.md",
		Kind:     "file",
		Title:    "Widget Doc",
		Summary:  "an entry about the widget subsystem",
		Tags:     []string{"test"},
		BodyHash: "hash-widget",
	}, chunks); err != nil {
		t.Fatalf("index fixture: %v", err)
	}

	return root
}

// runKB is a small Execute wrapper that captures stdout/stderr.
func runKB(t *testing.T, args []string) (stdout, stderr string, exit int) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	exit = Execute(context.Background(), args, &outBuf, &errBuf)
	return outBuf.String(), errBuf.String(), exit
}

func TestSearchFTSMode(t *testing.T) {
	root := newSearchTestRepo(t, ollama.DefaultModel, ollama.DefaultDim)
	t.Setenv("KB_ROOT", root)

	stdout, stderr, exit := runKB(t, []string{"search", "widget", "--mode", "fts"})
	if exit != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", exit, stderr)
	}
	// A two-hit block is deliberately checked as one shape: callers who do not use --json can
	// still rely on the documented human format: the id-bearing first line (so `| head` keeps it),
	// the blank line between hits, the search_id=N footer, and the feedback hint.
	human := regexp.MustCompile(`(?s)\Asearch ([1-9][0-9]*) · 2 hits\n#1  [0-9]+\.[0-9]{4}  widget\.md(?: › Configuration)?\n[^\n]+\n\n#2  [0-9]+\.[0-9]{4}  widget\.md(?: › Configuration)?\n[^\n]+\nsearch_id=([1-9][0-9]*)\n(→ judge: [^\n]+)\n\z`)
	m := human.FindStringSubmatch(stdout)
	if m == nil {
		t.Fatalf("stdout does not match the two-hit human format:\n%s", stdout)
	}
	// Go's regexp has no backreferences, so the three places the id appears are compared here.
	id := m[1]
	if m[2] != id {
		t.Errorf("header id %s and footer search_id=%s differ", id, m[2])
	}
	wantHint := "→ judge: kb feedback " + id + " RANK --useful (or --not-useful) · none helped: kb feedback " +
		id + ` --none --note "why"`
	if m[3] != wantHint {
		t.Errorf("hint = %q, want %q", m[3], wantHint)
	}
}

func TestSearchJSONMode(t *testing.T) {
	root := newSearchTestRepo(t, ollama.DefaultModel, ollama.DefaultDim)
	t.Setenv("KB_ROOT", root)

	stdout, stderr, exit := runKB(t, []string{"search", "widget", "--mode", "fts", "--json"})
	if exit != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", exit, stderr)
	}

	var parsed struct {
		SearchID int64 `json:"search_id"`
		Feedback struct {
			Useful    string `json:"useful"`
			NotUseful string `json:"not_useful"`
			None      string `json:"none"`
		} `json:"feedback"`
		Query string
		Mode  string
		Hits  []map[string]any `json:"hits"`
	}
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		t.Fatalf("json.Unmarshal: %v\nstdout:\n%s", err, stdout)
	}
	if parsed.Mode != "fts" {
		t.Errorf("mode = %q, want fts", parsed.Mode)
	}
	if len(parsed.Hits) == 0 {
		t.Error("hits is empty, want at least one hit")
	}
	if parsed.SearchID == 0 {
		t.Error("search_id is 0")
	}
	// The feedback commands name this search's own id. The placeholder is RANK, not <rank>: a
	// pasted <rank> is a shell redirection, so the line would fail or create a stray file.
	id := strconv.FormatInt(parsed.SearchID, 10)
	if want := "kb feedback " + id + " RANK --useful"; parsed.Feedback.Useful != want {
		t.Errorf("feedback.useful = %q, want %q", parsed.Feedback.Useful, want)
	}
	if want := "kb feedback " + id + ` RANK --not-useful --note "why"`; parsed.Feedback.NotUseful != want {
		t.Errorf("feedback.not_useful = %q, want %q", parsed.Feedback.NotUseful, want)
	}
	if want := "kb feedback " + id + ` --none --note "why"`; parsed.Feedback.None != want {
		t.Errorf("feedback.none = %q, want %q", parsed.Feedback.None, want)
	}
	if want := `"kb feedback ` + id + ` RANK --useful"`; !strings.Contains(stdout, want) {
		t.Errorf("raw JSON lacks the copyable command %s:\n%s", want, stdout)
	}

	// Confirm the JSON's top-level shape is an object with a "hits" array, not e.g. null.
	var raw map[string]any
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		t.Fatalf("re-unmarshal into map: %v", err)
	}
	if _, ok := raw["hits"].([]any); !ok {
		t.Errorf("hits is not a JSON array: %T", raw["hits"])
	}
}

func TestSearchHybridEmbedderUnavailable(t *testing.T) {
	// embed_meta matches what the ollama client will report for itself (DefaultModel/DefaultDim,
	// since KB_EMBED_MODEL is unset), so the mismatch check passes and the failure comes from the
	// network call instead.
	root := newSearchTestRepo(t, ollama.DefaultModel, ollama.DefaultDim)
	t.Setenv("KB_ROOT", root)
	t.Setenv("KB_OLLAMA_URL", "http://127.0.0.1:1") // closed port: connection refused

	stdout, stderr, exit := runKB(t, []string{"search", "widget", "--mode", "hybrid"})
	if exit != 2 {
		t.Fatalf("exit = %d, want 2; stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stderr, "falling back to --mode fts") {
		t.Errorf("stderr missing fallback warning: %s", stderr)
	}
	if !strings.Contains(stdout, "search_id=") {
		t.Errorf("stdout missing results/footer despite fallback:\n%s", stdout)
	}
}

func TestSearchVecEmbedderUnavailable(t *testing.T) {
	root := newSearchTestRepo(t, ollama.DefaultModel, ollama.DefaultDim)
	t.Setenv("KB_ROOT", root)
	t.Setenv("KB_OLLAMA_URL", "http://127.0.0.1:1")

	stdout, stderr, exit := runKB(t, []string{"search", "widget", "--mode", "vec"})
	if exit != 2 {
		t.Fatalf("exit = %d, want 2; stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout should be empty on a hard vec-mode failure, got:\n%s", stdout)
	}
}

func TestSearchNoDatabase(t *testing.T) {
	root := t.TempDir() // no .kb/kb.sqlite
	t.Setenv("KB_ROOT", root)

	_, stderr, exit := runKB(t, []string{"search", "widget"})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%s", exit, stderr)
	}
	if !strings.Contains(stderr, "kb reindex --all") {
		t.Errorf("stderr missing reindex hint: %s", stderr)
	}
	if _, err := os.Stat(filepath.Join(root, ".kb")); !os.IsNotExist(err) {
		t.Errorf("search without a database created .kb: stat error = %v", err)
	}
}

func TestSearchTagFilterAndZeroHits(t *testing.T) {
	root := newSearchTestRepo(t, ollama.DefaultModel, ollama.DefaultDim)
	t.Setenv("KB_ROOT", root)

	stdout, stderr, exit := runKB(t, []string{"search", "widget", "--mode", "fts", "--tag", "test"})
	if exit != 0 {
		t.Fatalf("tagged search exit = %d, want 0; stderr=%s", exit, stderr)
	}
	if !strings.Contains(stdout, "widget.md") {
		t.Errorf("tagged search omitted matching entry:\n%s", stdout)
	}

	stdout, stderr, exit = runKB(t, []string{"search", "not-in-the-fixture", "--mode", "fts"})
	if exit != 0 {
		t.Fatalf("zero-hit search exit = %d, want 0; stderr=%s", exit, stderr)
	}
	// A zero-result search still leads with its id and ends with a hint, and the hint offers only
	// --none: there is no rank to mark.
	m := regexp.MustCompile(`\Asearch ([1-9][0-9]*) · 0 hits\nno results\nsearch_id=([1-9][0-9]*)\n(→ judge: [^\n]+)\n\z`).FindStringSubmatch(stdout)
	if m == nil {
		t.Fatalf("zero-hit output = %q, want no-results format with a search id and hint", stdout)
	}
	if m[1] != m[2] {
		t.Errorf("header id %s and footer search_id=%s differ", m[1], m[2])
	}
	if want := "→ judge: kb feedback " + m[1] + ` --none --note "what you were looking for"`; m[3] != want {
		t.Errorf("zero-hit hint = %q, want %q", m[3], want)
	}

	stdout, stderr, exit = runKB(t, []string{"search", "not-in-the-fixture", "--mode", "fts", "--json"})
	if exit != 0 {
		t.Fatalf("zero-hit JSON search exit = %d, want 0; stderr=%s", exit, stderr)
	}
	var parsed struct {
		SearchID int64             `json:"search_id"`
		Feedback map[string]string `json:"feedback"`
		Hits     []map[string]any  `json:"hits"`
	}
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		t.Fatalf("unmarshal zero-hit JSON: %v\n%s", err, stdout)
	}
	want := map[string]string{"none": "kb feedback " + strconv.FormatInt(parsed.SearchID, 10) + ` --none --note "why"`}
	if !reflect.DeepEqual(parsed.Feedback, want) {
		t.Errorf("zero-hit JSON feedback = %v, want only %v", parsed.Feedback, want)
	}
}

func TestSearchEmbedMetaMismatch(t *testing.T) {
	root := newSearchTestRepo(t, "a-different-model", ollama.DefaultDim)
	t.Setenv("KB_ROOT", root)

	_, stderr, exit := runKB(t, []string{"search", "widget", "--mode", "hybrid"})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%s", exit, stderr)
	}
	if !strings.Contains(stderr, "embedding model mismatch") {
		t.Errorf("stderr missing embed_meta mismatch: %s", stderr)
	}
}

func TestSearchCallerPrecedence(t *testing.T) {
	tests := []struct {
		name   string
		env    string
		args   []string
		caller string
	}{
		{name: "flag", env: "from-environment", args: []string{"--caller", "from-flag"}, caller: "from-flag"},
		{name: "environment", env: "from-environment", caller: "from-environment"},
		{name: "unknown", env: "", caller: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := newSearchTestRepo(t, ollama.DefaultModel, ollama.DefaultDim)
			t.Setenv("KB_ROOT", root)
			t.Setenv("KB_CALLER", tt.env)

			args := append([]string{"search", "widget", "--mode", "fts"}, tt.args...)
			_, stderr, exit := runKB(t, args)
			if exit != 0 {
				t.Fatalf("exit = %d, want 0; stderr=%s", exit, stderr)
			}
			if got := loggedSearchCaller(t, root); got != tt.caller {
				t.Errorf("logged caller = %q, want %q", got, tt.caller)
			}
		})
	}
}

func TestSearchLogFailure(t *testing.T) {
	root := newSearchTestRepo(t, ollama.DefaultModel, ollama.DefaultDim)
	t.Setenv("KB_ROOT", root)
	disableSearchLog(t, root)

	stdout, stderr, exit := runKB(t, []string{"search", "widget", "--mode", "fts"})
	if exit != 0 {
		t.Fatalf("human log-failure exit = %d, want 0; stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if !strings.HasPrefix(stdout, "search not logged · 2 hits\n") || !strings.HasSuffix(stdout, "search_id=none (not logged)\n") {
		t.Errorf("human log-failure header/footer = %q", stdout)
	}
	if strings.Contains(stdout, "kb feedback") {
		t.Errorf("an unlogged search printed a feedback hint it cannot honour:\n%s", stdout)
	}
	if !strings.Contains(stderr, "warning: search not logged:") {
		t.Errorf("stderr missing log-failure warning: %s", stderr)
	}

	stdout, stderr, exit = runKB(t, []string{"search", "widget", "--mode", "fts", "--json"})
	if exit != 0 {
		t.Fatalf("JSON log-failure exit = %d, want 0; stdout=%s stderr=%s", exit, stdout, stderr)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		t.Fatalf("unmarshal JSON log-failure output: %v\n%s", err, stdout)
	}
	if id, ok := raw["search_id"]; !ok || id != nil {
		t.Errorf("JSON search_id = %#v (present %t), want present null", id, ok)
	}
	if fb, ok := raw["feedback"]; !ok || fb != nil {
		t.Errorf("JSON feedback = %#v (present %t), want present null", fb, ok)
	}
}

func TestSearchFallbackLogFailurePrintsBothWarnings(t *testing.T) {
	root := newSearchTestRepo(t, ollama.DefaultModel, ollama.DefaultDim)
	t.Setenv("KB_ROOT", root)
	t.Setenv("KB_OLLAMA_URL", "http://127.0.0.1:1")
	disableSearchLog(t, root)

	stdout, stderr, exit := runKB(t, []string{"search", "widget", "--mode", "hybrid"})
	if exit != 2 {
		t.Fatalf("exit = %d, want 2; stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if !strings.HasSuffix(stdout, "search_id=none (not logged)\n") {
		t.Errorf("fallback log-failure footer = %q", stdout)
	}
	notLogged := strings.Index(stderr, "warning: search not logged:")
	fallback := strings.Index(stderr, "falling back to --mode fts")
	if notLogged < 0 || fallback < 0 || notLogged > fallback {
		t.Errorf("stderr should contain the log warning before the fallback warning:\n%s", stderr)
	}
}

func loggedSearchCaller(t *testing.T, root string) string {
	t.Helper()
	st, err := store.Open(filepath.Join(root, ".kb", "kb.sqlite"))
	if err != nil {
		t.Fatalf("open store for caller assertion: %v", err)
	}
	defer st.Close()
	var caller string
	if err := st.DB().QueryRow("SELECT caller FROM searches ORDER BY id DESC LIMIT 1").Scan(&caller); err != nil {
		t.Fatalf("read logged caller: %v", err)
	}
	return caller
}

func disableSearchLog(t *testing.T, root string) {
	t.Helper()
	st, err := store.Open(filepath.Join(root, ".kb", "kb.sqlite"))
	if err != nil {
		t.Fatalf("open store to disable search logging: %v", err)
	}
	defer st.Close()
	if _, err := st.DB().Exec("ALTER TABLE searches RENAME TO searches_disabled"); err != nil {
		t.Fatalf("rename searches table: %v", err)
	}
}

func TestSearchBadMode(t *testing.T) {
	root := t.TempDir()
	t.Setenv("KB_ROOT", root)

	_, stderr, exit := runKB(t, []string{"search", "widget", "--mode", "bogus"})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%s", exit, stderr)
	}
	if !strings.Contains(stderr, "--mode") {
		t.Errorf("stderr missing mode complaint: %s", stderr)
	}
}

func TestSearchErrorHasOneSearchPrefix(t *testing.T) {
	root := newSearchTestRepo(t, ollama.DefaultModel, ollama.DefaultDim)
	t.Setenv("KB_ROOT", root)

	_, stderr, exit := runKB(t, []string{"search", " ", "--mode", "fts"})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%s", exit, stderr)
	}
	if !strings.Contains(stderr, "kb search: empty query") || strings.Contains(stderr, "kb search: search:") {
		t.Errorf("empty-query error has the wrong prefix: %s", stderr)
	}
}

func TestSearchHelpRecommendsJSON(t *testing.T) {
	stdout, stderr, exit := runKB(t, []string{"search", "--help"})
	if exit != 0 {
		t.Fatalf("exit = %d, want 0; stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stdout, "Agents and scripts should parse --json.") {
		t.Errorf("search help does not recommend --json parsing:\n%s", stdout)
	}
}

func TestSearchKClampsToOne(t *testing.T) {
	root := newSearchTestRepo(t, ollama.DefaultModel, ollama.DefaultDim)
	t.Setenv("KB_ROOT", root)

	stdout, stderr, exit := runKB(t, []string{"search", "widget", "--mode", "fts", "-k", "0"})
	if exit != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", exit, stderr)
	}
	n := strings.Count(stdout, "\n#")
	if strings.HasPrefix(stdout, "#") {
		n++
	}
	if n != 1 {
		t.Errorf("got %d result blocks, want 1 (k=0 should clamp to 1):\n%s", n, stdout)
	}
}

func TestSearchMultiWordQueryJoinsArgs(t *testing.T) {
	root := newSearchTestRepo(t, ollama.DefaultModel, ollama.DefaultDim)
	t.Setenv("KB_ROOT", root)

	stdout, stderr, exit := runKB(t, []string{"search", "widget", "subsystem", "--mode", "fts"})
	if exit != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", exit, stderr)
	}
	if !strings.Contains(stdout, "search_id=") {
		t.Errorf("stdout missing footer:\n%s", stdout)
	}
}

func TestTrimText(t *testing.T) {
	t.Run("under limit is unchanged", func(t *testing.T) {
		s := strings.Repeat("a", 600)
		if got := trimText(s, 600); got != s {
			t.Errorf("trimText changed a string at exactly the limit")
		}
	})

	t.Run("cuts at the last space before the limit", func(t *testing.T) {
		s := strings.Repeat("a", 595) + " " + strings.Repeat("b", 10) // len 606
		want := strings.Repeat("a", 595) + " …"
		if got := trimText(s, 600); got != want {
			t.Errorf("trimText(...) = %q, want %q", got, want)
		}
	})

	t.Run("hard cut when there is no space", func(t *testing.T) {
		s := strings.Repeat("a", 700)
		got := trimText(s, 600)
		want := strings.Repeat("a", 600) + " …"
		if got != want {
			t.Errorf("trimText(...) = %q, want %q", got, want)
		}
	})

	t.Run("does not backtrack more than eighty runes", func(t *testing.T) {
		s := "See " + strings.Repeat("x", 700)
		want := "See " + strings.Repeat("x", 596) + " …"
		if got := trimText(s, 600); got != want {
			t.Errorf("trimText(...) = %q, want a hard cut %q", got, want)
		}
	})

	t.Run("multibyte text respects rune boundaries", func(t *testing.T) {
		s := strings.Repeat("é", 700) // each é is one rune, two UTF-8 bytes
		got := trimText(s, 600)
		if !strings.HasSuffix(got, " …") {
			t.Fatalf("trimText did not mark the cut: %q", got)
		}
		body := strings.TrimSuffix(got, " …")
		if n := len([]rune(body)); n != 600 {
			t.Errorf("trimmed body has %d runes, want 600", n)
		}
		// Every rune must still be a valid é, i.e. no byte was sliced out of the middle of one.
		for _, r := range body {
			if r != 'é' {
				t.Fatalf("trimText corrupted a multibyte rune: got %q in body", r)
			}
		}
	})

	t.Run("empty text", func(t *testing.T) {
		if got := trimText("", 600); got != "" {
			t.Errorf("trimText(\"\", 600) = %q, want \"\"", got)
		}
	})
}
