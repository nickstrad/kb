package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// This file holds the pure half of the journey audit (PLAN E5, docs/TESTING.md §11.7): the
// README cross-check and the helpers the command audit (audit_test.go, tag e2e) shares. It needs
// no kb binary, so it runs in a plain go test.
//
// README markers. An HTML comment `<!-- journey: a, b -->` names the journeys that back the
// block that follows it: the next fenced code block, or else the next paragraph or list (up to
// the next blank line). Every kb command in the block must appear in one of those journeys:
// a whole line that starts with `kb ` (or `$ kb `) inside a fence, minus a trailing `# comment`,
// and every inline `code span` that starts with `kb `. A README command matches a journey
// command when its tokens are an ordered subsequence of the journey's (the journey may add flags
// such as --json), where a README placeholder token (`<path>`, or REF:path) stands for any one
// token that is not a flag.

// tokenize splits a command line on spaces, keeping quoted text (single or double) as one
// token without its quotes. An empty quoted string is an empty token.
func tokenize(s string) []string {
	var toks []string
	var cur strings.Builder
	var quote rune
	has := false
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, has = r, true
		case r == ' ' || r == '\t':
			if has {
				toks = append(toks, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteRune(r)
			has = true
		}
	}
	if has {
		toks = append(toks, cur.String())
	}
	return toks
}

// valueFlags are the global flags that take a separate value ("--store adr").
var valueFlags = map[string]bool{"--store": true, "--root": true}

// cmdPath returns the command path of a tokenised "kb ..." line against the command tree
// (tree maps a path such as "model" to its child names): flags and the values of valueFlags are
// skipped, and tokens are followed down the tree until one is not a subcommand.
func cmdPath(toks []string, tree map[string][]string) string {
	if len(toks) == 0 || toks[0] != "kb" {
		return ""
	}
	path := ""
	for i := 1; i < len(toks); i++ {
		t := toks[i]
		if strings.HasPrefix(t, "-") {
			if valueFlags[t] {
				i++
			}
			continue
		}
		found := false
		for _, c := range tree[path] {
			if c == t {
				found = true
			}
		}
		if !found {
			break
		}
		if path != "" {
			path += " "
		}
		path += t
	}
	return path
}

// journeyCommands returns the tokenised kb command lines of a journey, in order.
func journeyCommands(t *testing.T, name string) [][]string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("testdata", "journeys", name+".txt"))
	if err != nil {
		t.Fatalf("journey %q: %v", name, err)
	}
	tr, err := Parse(string(src))
	if err != nil {
		t.Fatalf("journey %q: %v", name, err)
	}
	var out [][]string
	for _, b := range tr.Blocks {
		if toks := tokenize(b.Cmd); b.IsCmd && len(toks) > 0 && toks[0] == "kb" {
			out = append(out, toks)
		}
	}
	return out
}

var rePlaceholder = regexp.MustCompile(`^<[^<>]+>$`)

// isPlaceholder reports whether a README token stands for "any one non-flag token".
func isPlaceholder(tok string) bool { return rePlaceholder.MatchString(tok) || tok == "REF:path" }

// matchCommand reports whether the README command is an ordered subsequence of the journey
// command, with placeholders matching any one token that does not start with "-".
func matchCommand(readme, journey []string) bool {
	j := 0
	for _, r := range readme {
		for ; j < len(journey); j++ {
			jt := journey[j]
			if jt == r || (isPlaceholder(r) && !strings.HasPrefix(jt, "-")) {
				j++
				goto next
			}
		}
		return false
	next:
	}
	return true
}

// readmeSkip lists README commands in marked blocks that no journey can carry, each with the
// reason. A stale entry (the command is no longer in a marked block) fails the test.
var readmeSkip = map[string]string{
	"kb help":             "the output is the whole reference text; a journey would copy it and churn on every wording change",
	"kb <command> --help": "same as kb help: a placeholder for any command's reference text",
}

// markedBlock is a README block with its journeys and the kb commands found in it.
type markedBlock struct {
	line     int // 1-based line of the marker
	journeys []string
	cmds     []string // command text as written
}

var (
	reMarker = regexp.MustCompile(`^<!-- journey: ([a-z0-9, -]+) -->$`)
	reSpan   = regexp.MustCompile("`([^`]+)`")
)

// markedBlocks reads README text and returns its marked blocks. A marker with no block after
// it, or naming no journey, is an error.
func markedBlocks(readme string) ([]markedBlock, error) {
	lines := strings.Split(readme, "\n")
	var out []markedBlock
	for i := 0; i < len(lines); i++ {
		trim := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trim, "<!-- journey:") {
			m := reMarker.FindStringSubmatch(trim)
			if m == nil {
				return nil, fmt.Errorf("README line %d: malformed journey marker %q", i+1, trim)
			}
			b := markedBlock{line: i + 1}
			for _, j := range strings.Split(m[1], ",") {
				b.journeys = append(b.journeys, strings.TrimSpace(j))
			}
			k := i + 1
			for k < len(lines) && strings.TrimSpace(lines[k]) == "" {
				k++
			}
			if k >= len(lines) {
				return nil, fmt.Errorf("README line %d: journey marker with no block after it", i+1)
			}
			fenced := strings.HasPrefix(lines[k], "```")
			var body []string
			if fenced {
				for k++; k < len(lines) && !strings.HasPrefix(lines[k], "```"); k++ {
					body = append(body, lines[k])
				}
			} else {
				for ; k < len(lines) && strings.TrimSpace(lines[k]) != ""; k++ {
					body = append(body, lines[k])
				}
			}
			for _, ln := range body {
				b.cmds = append(b.cmds, commandsIn(ln, fenced)...)
			}
			if len(b.cmds) == 0 {
				return nil, fmt.Errorf("README line %d: marked block holds no kb command", i+1)
			}
			out = append(out, b)
			i = k
		}
	}
	return out, nil
}

// commandsIn returns the kb commands on one README line: in a fence, the whole line when it
// starts with "kb " or "$ kb " (a trailing "  # comment" cut off); and always the inline code
// spans that start with "kb ".
func commandsIn(ln string, fenced bool) []string {
	if fenced {
		s := strings.TrimPrefix(strings.TrimSpace(ln), "$ ")
		if strings.HasPrefix(s, "kb ") {
			if k := strings.Index(s, " #"); k >= 0 {
				s = s[:k]
			}
			return []string{strings.TrimSpace(s)}
		}
	}
	var out []string
	for _, m := range reSpan.FindAllStringSubmatch(ln, -1) {
		if strings.HasPrefix(m[1], "kb ") {
			out = append(out, m[1])
		}
	}
	return out
}

// checkBlocks returns one message per README command that no named journey backs.
func checkBlocks(t *testing.T, blocks []markedBlock) []string {
	t.Helper()
	cache := map[string][][]string{}
	var bad []string
	for _, b := range blocks {
		for _, j := range b.journeys {
			if _, ok := cache[j]; !ok {
				cache[j] = journeyCommands(t, j)
			}
		}
		for _, c := range b.cmds {
			if _, skip := readmeSkip[c]; skip {
				continue
			}
			ok := false
			for _, j := range b.journeys {
				for _, jc := range cache[j] {
					if matchCommand(tokenize(c), jc) {
						ok = true
					}
				}
			}
			if !ok {
				bad = append(bad, fmt.Sprintf("README line %d: %q is in no command of journey(s) %s", b.line, c, strings.Join(b.journeys, ", ")))
			}
		}
	}
	return bad
}

func TestReadmeMatchesJourneys(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := markedBlocks(string(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) < 3 {
		t.Fatalf("README has %d journey-marked blocks; Searching, Upgrading and Using kb from an agent need one each", len(blocks))
	}
	for _, msg := range checkBlocks(t, blocks) {
		t.Error(msg)
	}
	used := map[string]bool{}
	for _, b := range blocks {
		for _, c := range b.cmds {
			used[c] = true
		}
	}
	for c := range readmeSkip {
		if !used[c] {
			t.Errorf("readmeSkip entry %q is in no marked block; delete it", c)
		}
	}
}

func TestMarkedBlocks(t *testing.T) {
	const doc = "intro `kb nope`\n\n<!-- journey: one, two -->\n```\n$ kb search \"x\" --json  # comment\nkb show <path>\nnot a command\n```\n\n<!-- journey: three -->\n- `kb add <f.md>` saves, or `kb rm <path>`\n- `plain` and `kb list`\n\nafter `kb ignored`\n"
	got, err := markedBlocks(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d blocks, want 2", len(got))
	}
	want := [][]string{{`kb search "x" --json`, "kb show <path>"}, {"kb add <f.md>", "kb rm <path>", "kb list"}}
	wantJ := [][]string{{"one", "two"}, {"three"}}
	for i := range want {
		if strings.Join(got[i].cmds, "|") != strings.Join(want[i], "|") || strings.Join(got[i].journeys, "|") != strings.Join(wantJ[i], "|") {
			t.Errorf("block %d = %+v, want cmds %q journeys %q", i, got[i], want[i], wantJ[i])
		}
	}
	for _, bad := range []string{"<!-- journey: -->\nx", "<!-- journey: a -->\n", "<!-- journey: a -->\n\n`plain`\n"} {
		if _, err := markedBlocks(bad); err == nil {
			t.Errorf("markedBlocks(%q) = nil error, want one", bad)
		}
	}
}

func TestMatchCommand(t *testing.T) {
	j := tokenize(`kb search "how do I" --caller claude --json`)
	for _, tc := range []struct {
		readme string
		want   bool
	}{
		{`kb search "<question>" --caller claude`, true},
		{`kb search "<question>" --json --caller claude`, false}, // order matters
		{`kb search "<question>" --callr claude`, false},         // a flag the journey lacks
		{`kb search <q> <extra> <more>`, false},                  // a placeholder never matches a flag
		{`kb search`, true},
		{`kb show <path>`, false},
		{`kb search "how do I" --caller <who>`, true},
	} {
		if got := matchCommand(tokenize(tc.readme), j); got != tc.want {
			t.Errorf("matchCommand(%q) = %v, want %v", tc.readme, got, tc.want)
		}
	}
}

func TestTokenizeAndCmdPath(t *testing.T) {
	if got := strings.Join(tokenize(`kb search '' "a b" -- --x`), "|"); got != "kb|search||a b|--|--x" {
		t.Errorf("tokenize = %q", got)
	}
	tree := map[string][]string{"": {"model", "search", "stats"}, "model": {"pull", "cache"}}
	for line, want := range map[string]string{
		`kb model pull`:                      "model pull",
		`kb --store adr model cache --clear`: "model cache",
		`kb --global search "model pull"`:    "search",
		`kb model`:                           "model",
		`kb --store model`:                   "",
		`kb stats overview --json`:           "stats",
		`ls model`:                           "",
	} {
		if got := cmdPath(tokenize(line), tree); got != want {
			t.Errorf("cmdPath(%q) = %q, want %q", line, got, want)
		}
	}
}
