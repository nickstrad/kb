package main

// Scenarios for in-place sources (PLAN 1.5): [[sources]] globs and excludes, the directories a
// glob never walks, missing source directories, symlinks out of the location root (DEFAULTS
// #65), two sources claiming one file, hostile globs, kb rm on a file kb does not own, and
// kb init --source.

import (
	"os"
	"path/filepath"
	"strings"
)

var sourcesScenarios = []scenario{
	{"in-place-sources", "[[sources]]: a files glob with exclude indexes Markdown in place (front matter optional) and never walks .git/.kb/node_modules; a missing source dir is empty; symlinks out of the root are skipped with a warning and reported by doctor; two sources claiming one file exit 1 and delete nothing; hostile globs exit 1 naming the key; kb rm refuses an in-place file; a deleted file is pruned; kb init --source writes the globs", scInPlaceSources},
}

func init() { scenarios = append(scenarios, sourcesScenarios...) }

const noneEmbed = "[embed]\nmodel = \"none\"\n"

// expectMsg is expect plus a substring of stderr ("" = any).
func (h *H) expectMsg(r res, code int, inStderr string) {
	if h.expect(r, code) && !strings.Contains(r.Stderr, inStderr) {
		h.fail("want %q on stderr:\n%s", inStderr, r)
	}
}

func scInPlaceSources(h *H) {
	inPlace := noneEmbed + dataSource +
		"[[sources]]\nglob = \"docs/**/*.md\"\nlayout = \"files\"\nexclude = [\"docs/archive/**\"]\n"
	h.setConfig(inPlace, false)
	h.write("data/owned.md", entryText("Owned", "## A\n\nzebraowned\n"))
	h.write("docs/guide.md", "# Guide\n\nzebraguide is the first paragraph.\n\n## Usage\n\nmore\n")
	h.write("docs/deep/x/plain.md", "no heading at all, zebraplain\n")
	h.write("docs/archive/old.md", "# Old\n\nzebraarchived\n")
	h.write("docs/node_modules/pkg/readme.md", "# Dep\n\nzebradependency\n")
	h.write("docs/.git/notes.md", "# Git\n\nzebragit\n")
	h.write("docs/.kb/stores/x/config.md", "# Kb\n\nzebrakb\n")
	h.write("docs/notes.txt", "zebratext\n")
	h.write("docs/crlf.md", "\uFEFF# Crlf\r\n\r\nzebracrlf\r\n")
	h.write("docs/control.md", "# a\x1bb\x00c\n\nzebracontrol \x07 bell\n")
	const secret = "ZEBRASECRETOUTSIDE"
	outsideFile := h.write(filepath.Join(h.outside, "secret.md"), "# Secret\n\n"+secret+"\n")
	h.must(os.Symlink(outsideFile, filepath.Join(h.root, "docs", "leak.md")) == nil, "symlink docs")
	h.must(os.Symlink(outsideFile, filepath.Join(h.root, "data", "leak.md")) == nil, "symlink data")
	h.must(os.Symlink(h.outside, filepath.Join(h.root, "docs", "outdir")) == nil, "symlink dir")

	r := h.kb(withNone("reindex", "--all")...)
	h.expectMsg(r, 0, "")
	if !strings.Contains(r.Stderr, "skipped docs/leak.md") || !strings.Contains(r.Stderr, "skipped data/leak.md") {
		h.fail("reindex does not warn about the symlinks out of the root:\n%s", r)
	}
	for token, want := range map[string]bool{
		"zebraowned": true, "zebraguide": true, "zebraplain": true, "zebracrlf": true, "zebracontrol": true,
		"zebraarchived": false, "zebradependency": false, "zebragit": false, "zebrakb": false, "zebratext": false, secret: false,
	} {
		s := h.kb(withNone("search", token, "--mode", "fts")...)
		if got := strings.Contains(s.Stdout, "› ") || strings.Contains(s.Stdout, "[1]"); got != want {
			h.fail("search %s: found=%v, want %v\n%s", token, got, want, s)
		}
	}
	d := h.kb(withNone("doctor")...)
	if !strings.Contains(d.Stdout, "warn sources: skipped docs/leak.md") || !strings.Contains(d.Stdout, "warn sources: skipped data/leak.md") {
		h.fail("doctor does not report the symlinks out of the root:\n%s", d)
	}
	l := h.kb(withNone("list")...)
	if !strings.Contains(l.Stdout, "docs/guide.md") || !strings.Contains(l.Stdout, "zebraguide is the first paragraph.") {
		h.fail("kb list lacks the in-place entry or its fallback summary:\n%s", l)
	}

	// kb rm never deletes a file kb does not own; a symlink's target is never touched.
	r = h.kb(withNone("rm", "docs/guide.md")...)
	h.expectMsg(r, 1, "not owned by kb")
	if _, err := os.Stat(filepath.Join(h.root, "docs", "guide.md")); err != nil {
		h.fail("kb rm deleted an in-place file")
	}
	h.kb(withNone("rm", "-y", "docs/leak.md")...)
	if _, err := os.Stat(outsideFile); err != nil {
		h.fail("kb rm removed a file outside the root through a symlink")
	}

	// Deleting an in-place file prunes its entry.
	h.must(os.Remove(filepath.Join(h.root, "docs", "deep", "x", "plain.md")) == nil, "remove plain.md")
	r = h.kb(withNone("reindex", "--all")...)
	if r.Code != 0 || !strings.Contains(r.Stdout, "removed docs/deep/x/plain.md") {
		h.fail("a deleted in-place file was not pruned:\n%s", r)
	}

	// Two sources claiming one file: exit 1 naming both, nothing pruned.
	h.setConfig(noneEmbed+dataSource+"[[sources]]\nglob = \"**/owned.md\"\nlayout = \"files\"\n", false)
	r = h.kb(withNone("reindex", "--all")...)
	h.expectMsg(r, 1, "both match data/owned.md")
	h.expectMsg(h.kb(withNone("doctor")...), 1, "")
	h.setConfig(inPlace, false)
	if s := h.kb(withNone("search", "zebraguide", "--mode", "fts")...); !strings.Contains(s.Stdout, "docs/guide.md") {
		h.fail("the overlap error deleted indexed rows:\n%s", s)
	}

	// Missing source directories are empty sources: exit 0, doctor warns, and the rows go.
	h.setConfig(noneEmbed+"[[sources]]\npath = \"nope\"\nlayout = \"entries\"\n[[sources]]\nglob = \"missing/**/*.md\"\nlayout = \"files\"\n", false)
	h.expectMsg(h.kb(withNone("reindex", "--all")...), 0, "")
	d = h.kb(withNone("doctor")...)
	if d.Code != 0 || !strings.Contains(d.Stdout, "no directory") || !strings.Contains(d.Stdout, "matches no *.md file") {
		h.fail("doctor on missing source dirs:\n%s", d)
	}

	// Hostile globs and paths: a Usage error naming the file and the key, never exit 3.
	for _, src := range []string{
		"glob = \"/etc/*.md\"\nlayout = \"files\"",
		"glob = \"../outside/*.md\"\nlayout = \"files\"",
		"glob = \"docs/../../*.md\"\nlayout = \"files\"",
		"glob = \"docs/[.md\"\nlayout = \"files\"",
		"path = \"../outside\"\nlayout = \"entries\"",
		"path = \".kb\"\nlayout = \"entries\"",
		"path = \"/tmp\"\nlayout = \"entries\"",
		"glob = \"*.md\"\nlayout = \"tree\"",
	} {
		h.setConfig(noneEmbed+"[[sources]]\n"+src+"\n", false)
		r := h.kb(withNone("reindex", "--all")...)
		if r.Code != 1 || !strings.Contains(r.Stderr, "config.toml") || !strings.Contains(r.Stderr, "sources[1]") {
			h.fail("hostile source %q: want exit 1 naming config.toml and sources[1]\n%s", src, r)
		}
	}
	if _, err := os.Stat(outsideFile); err != nil {
		h.fail("a file outside the root was removed")
	}

	// kb init --source in a fresh location writes the globs and no data/ directory.
	fresh := filepath.Join(h.outside, "fresh")
	h.write(filepath.Join(fresh, "README.md"), "# Fresh\n\nzebrainstall with go install.\n")
	h.expectMsg(h.kbAt(fresh, "init", "--model", "none", "--source", "README.md"), 0, "")
	if _, err := os.Stat(filepath.Join(fresh, "data")); err == nil {
		h.fail("kb init --source created data/")
	}
	h.expectMsg(h.kbAt(fresh, "reindex", "--all"), 0, "")
	if s := h.kbAt(fresh, "search", "zebrainstall", "--mode", "fts"); !strings.Contains(s.Stdout, "README.md") {
		h.fail("kb init --source README.md, reindex, search: README.md not found\n%s", s)
	}
	h.expectMsg(h.kbAt(fresh, "init", "--name", "bad", "--model", "none", "--source", "../*.md"), 1, "--source")
}
