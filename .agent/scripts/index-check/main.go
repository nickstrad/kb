// Command index-check reports entries missing from, or extra in, the .agent/scripts/index.md
// .agent/knowledge/index.md and .agent/leverage/index.md indexes compared with the files and folders next to them.
//
// Usage (from the repo root): go run ./.agent/scripts/index-check [-root .agent]
// Exit status: 0 in sync, 1 out of sync, 2 could not read.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// indexed lists the folders whose index.md must name every entry beside it.
var indexed = []string{"scripts", "knowledge", "leverage"}

// skip holds names in an indexed folder that are not entries.
var skip = map[string]bool{"index.md": true, "README.md": true}

// linkRE matches a Markdown link target: [text](target).
var linkRE = regexp.MustCompile(`\]\(([^)#\s]+)[^)]*\)`)

func main() {
	root := flag.String("root", ".agent", "the .agent directory")
	flag.Parse()
	bad := false
	for _, name := range indexed {
		dir := filepath.Join(*root, name)
		missing, extra, err := check(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "index-check: %v\n", err)
			os.Exit(2)
		}
		for _, m := range missing {
			fmt.Printf("%s/index.md: missing entry for %s\n", dir, m)
		}
		for _, x := range extra {
			fmt.Printf("%s/index.md: lists %s, which does not exist\n", dir, x)
		}
		bad = bad || len(missing) > 0 || len(extra) > 0
	}
	if bad {
		os.Exit(1)
	}
	fmt.Println("index-check: indexes in sync")
}

// check compares dir's entries with the local links in dir/index.md.
func check(dir string) (missing, extra []string, err error) {
	index, err := os.ReadFile(filepath.Join(dir, "index.md"))
	if err != nil {
		return nil, nil, fmt.Errorf("read index: %w", err)
	}
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("list %s: %w", dir, err)
	}
	var have []string
	for _, de := range des {
		if !skip[de.Name()] && !strings.HasPrefix(de.Name(), ".") {
			have = append(have, de.Name())
		}
	}
	missing, extra = diff(have, links(string(index)))
	return missing, extra, nil
}

// links returns the first path element of each relative link target in md, so that
// "foo/README.md", "foo/" and "foo" all name the entry "foo". URLs and absolute paths are ignored.
func links(md string) []string {
	var out []string
	for _, m := range linkRE.FindAllStringSubmatch(md, -1) {
		t := strings.TrimPrefix(m[1], "./")
		if t == "" || strings.Contains(t, "://") || strings.HasPrefix(t, "/") || strings.HasPrefix(t, "../") {
			continue
		}
		out = append(out, strings.SplitN(t, "/", 2)[0])
	}
	return out
}

// diff returns the sorted names in have but not listed, and listed but not in have.
func diff(have, listed []string) (missing, extra []string) {
	inHave, inListed := set(have), set(listed)
	for n := range inHave {
		if !inListed[n] {
			missing = append(missing, n)
		}
	}
	for n := range inListed {
		if !inHave[n] {
			extra = append(extra, n)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return missing, extra
}

func set(names []string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}
