package entry

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/errs"
)

// mkTree writes files (root-relative path -> content) under a new temp root and returns the root
// with symlinks resolved.
func mkTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func paths(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Path
	}
	return out
}

func files(glob string, exclude ...string) config.Source {
	return config.Source{Glob: glob, Layout: config.LayoutFiles, Exclude: exclude}
}

const validFM = "---\ntitle: T\nsummary: S\n---\n## A\n\nbody\n"

// TestDiscoverFilesLayout: glob, exclude, the never-walked directories, non-.md files and the
// files-layout fields of each entry.
func TestDiscoverFilesLayout(t *testing.T) {
	root := mkTree(t, map[string]string{
		"README.md":                  "# kb\n\nInstall it with go install.\n",
		"docs/install.md":            "# Install\n\nSteps.\n",
		"docs/guide/usage.md":        "plain text, no heading\n",
		"docs/archive/old.md":        "# Old\n",
		"docs/notes.txt":             "not markdown\n",
		"docs/node_modules/x/y.md":   "# dependency\n",
		"docs/.git/HEAD.md":          "# git\n",
		"docs/.kb/stores/a/z.md":     "# store\n",
		"docs/with-fm.md":            validFM,
		"other/elsewhere.md":         "# elsewhere\n",
		"docs/guide/deeper/again.md": "again\n",
	})
	got, skipped, err := Discover(root, []config.Source{files("docs/**/*.md", "docs/archive/**", "docs/guide/deeper/*")})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"docs/guide/usage.md", "docs/install.md", "docs/with-fm.md"}
	if !reflect.DeepEqual(paths(got), want) || len(skipped) != 0 {
		t.Fatalf("Discover = %v (skipped %v), want %v", paths(got), skipped, want)
	}
	for _, e := range got {
		if e.Kind != KindFile || e.Layout != config.LayoutFiles || !e.InPlace() || e.Source != 0 || !reflect.DeepEqual(e.Files, []string{e.Path}) || e.Err != nil {
			t.Errorf("%s: %+v, want a valid in-place file entry of source 0", e.Path, e)
		}
	}
	if got[1].Meta.Title != "Install" || got[1].Meta.Summary != "Steps." {
		t.Errorf("install.md meta = %+v, want the H1 and first paragraph", got[1].Meta)
	}
	if got[0].Meta.Title != "Usage" || got[0].Meta.Summary != "plain text, no heading" {
		t.Errorf("usage.md meta = %+v, want the humanised name and the paragraph", got[0].Meta)
	}
	if got[2].Meta.Title != "T" || got[2].Meta.Summary != "S" {
		t.Errorf("with-fm.md meta = %+v, want the front matter", got[2].Meta)
	}

	// A literal glob at the root, the acceptance shape (kb init --source README.md).
	got, _, err = Discover(root, []config.Source{files("README.md")})
	if err != nil || !reflect.DeepEqual(paths(got), []string{"README.md"}) {
		t.Fatalf("README.md glob = %v, %v", paths(got), err)
	}
	if got[0].Meta.Title != "kb" || got[0].Meta.Summary != "Install it with go install." {
		t.Errorf("README meta = %+v", got[0].Meta)
	}
}

// TestDiscoverSourcesTable is the decision table of Discover over sources: one row per outcome.
func TestDiscoverSourcesTable(t *testing.T) {
	entriesSrc := func(p string) config.Source { return config.Source{Path: p, Layout: config.LayoutEntries} }
	tests := []struct {
		name    string
		tree    map[string]string
		sources []config.Source
		want    []string
		errKind errs.Kind // checked when errMsg != ""
		errMsg  string
	}{
		{"no sources index nothing", map[string]string{"data/a.md": validFM}, nil, []string{}, 0, ""},
		{"missing entries dir is empty", map[string]string{}, []config.Source{entriesSrc("data")}, []string{}, 0, ""},
		{"missing glob base is empty", map[string]string{}, []config.Source{files("docs/**/*.md")}, []string{}, 0, ""},
		{"named store entries dir", map[string]string{"data-adr/x.md": validFM, "data/y.md": validFM}, []config.Source{entriesSrc("data-adr")}, []string{"data-adr/x.md"}, 0, ""},
		{"entries and files side by side", map[string]string{"data/a.md": validFM, "docs/b.md": "b\n"},
			[]config.Source{entriesSrc("data"), files("docs/*.md")}, []string{"data/a.md", "docs/b.md"}, 0, ""},
		{"overlap between an entries and a files source", map[string]string{"data/a.md": validFM},
			[]config.Source{entriesSrc("data"), files("**/*.md")}, nil, errs.Usage, "sources[1] and sources[2]"},
		{"overlap through a directory entry's docs", map[string]string{"data/d/README.md": validFM, "data/d/docs/x.md": "x\n"},
			[]config.Source{entriesSrc("data"), files("data/d/docs/*.md")}, nil, errs.Usage, "both match data/d/docs/x.md"},
		{"overlap avoided with exclude", map[string]string{"data/a.md": validFM, "b.md": "b\n"},
			[]config.Source{entriesSrc("data"), files("**/*.md", "data/**")}, []string{"b.md", "data/a.md"}, 0, ""},
		{"two globs matching one file", map[string]string{"docs/a.md": "a\n"},
			[]config.Source{files("docs/*.md"), files("**/a.md")}, nil, errs.Usage, "both match docs/a.md"},
		{"entries dir is a file", map[string]string{"data": "x"}, []config.Source{entriesSrc("data")}, nil, errs.Usage, "cannot read the entries directory data"},
		{"a .md the glob rejects is not indexed", map[string]string{"docs/a.md": "a\n", "docs/b.md": "b\n"},
			[]config.Source{files("docs/a*.md")}, []string{"docs/a.md"}, 0, ""},
		{"overlap found by the entries source (files listed first)", map[string]string{"data/a.md": validFM},
			[]config.Source{files("data/a.md"), entriesSrc("data")}, nil, errs.Usage, "both match data/a.md"},
		{"overlap found by a directory entry (files listed first)", map[string]string{"data/d/README.md": validFM},
			[]config.Source{files("data/d/README.md"), entriesSrc("data")}, nil, errs.Usage, "both match data/d/README.md"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := mkTree(t, tt.tree)
			got, _, err := Discover(root, tt.sources)
			if tt.errMsg != "" {
				if err == nil || errs.KindOf(err) != tt.errKind || !strings.Contains(err.Error(), tt.errMsg) {
					t.Fatalf("Discover err = %v (kind %v), want %v containing %q", err, errs.KindOf(err), tt.errKind, tt.errMsg)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p := paths(got); !reflect.DeepEqual(p, tt.want) {
				t.Fatalf("Discover = %v, want %v", p, tt.want)
			}
		})
	}
}

// TestDiscoverSymlinks: a symlink that leads outside the location root is skipped with a reason,
// wherever it sits (DEFAULTS #65); one that stays inside is read.
func TestDiscoverSymlinks(t *testing.T) {
	outside := mkTree(t, map[string]string{"secret.md": validFM, "dir/README.md": validFM, "docs/x.md": "x\n"})
	root := mkTree(t, map[string]string{
		"data/real.md":            validFM,
		"data/d/README.md":        validFM,
		"docs/inside-target.md":   "# Target\n",
		"data/linkdir/README.md":  validFM,
		"site/docs/page.md":       "# Page\n",
		"data/indocs/README.md":   validFM,
		"data/indocs/docs/own.md": "own\n",
	})
	symlink(t, filepath.Join(outside, "secret.md"), filepath.Join(root, "data", "leak.md"))
	symlink(t, filepath.Join(outside, "docs", "x.md"), filepath.Join(root, "data", "indocs", "docs", "leak.md"))
	symlink(t, filepath.Join(root, "docs", "inside-target.md"), filepath.Join(root, "data", "inner.md"))
	symlink(t, filepath.Join(outside, "dir", "README.md"), filepath.Join(root, "data", "d2", "README.md"))
	symlink(t, filepath.Join(outside, "secret.md"), filepath.Join(root, "docs", "leak.md"))
	symlink(t, filepath.Join(root, "site", "docs"), filepath.Join(root, "linked"))
	symlink(t, outside, filepath.Join(root, "ext"))
	symlink(t, filepath.Join(root, "no-such.md"), filepath.Join(root, "data", "dangling.md"))

	srcs := []config.Source{{Path: "data", Layout: config.LayoutEntries}, files("docs/*.md"), files("linked/*.md"), files("ext/**/*.md")}
	got, skipped, err := Discover(root, srcs)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"data/d/README.md", "data/dangling.md", "data/indocs/README.md", "data/inner.md", "data/linkdir/README.md", "data/real.md", "docs/inside-target.md", "linked/page.md"}
	if !reflect.DeepEqual(paths(got), want) {
		t.Fatalf("entries = %v, want %v", paths(got), want)
	}
	for _, e := range got {
		switch e.Path {
		case "data/dangling.md":
			if e.Err == nil {
				t.Errorf("a dangling link is an entry that cannot be read, got Err nil")
			}
		case "data/indocs/README.md":
			if !reflect.DeepEqual(e.Files, []string{"data/indocs/README.md", "data/indocs/docs/own.md"}) {
				t.Errorf("indocs files = %v, want the leaking docs file dropped", e.Files)
			}
		}
	}
	var skippedPaths []string
	for _, s := range skipped {
		skippedPaths = append(skippedPaths, s.Path)
		if !strings.Contains(s.Reason, "outside the location root") || !strings.Contains(s.String(), s.Path+": ") {
			t.Errorf("skip %v: reason does not say outside the root", s)
		}
	}
	wantSkipped := map[string]bool{"data/leak.md": true, "data/indocs/docs/leak.md": true, "data/d2/README.md": true, "docs/leak.md": true, "ext": true}
	if len(skippedPaths) != len(wantSkipped) {
		t.Fatalf("skipped = %v, want %v", skippedPaths, wantSkipped)
	}
	for _, p := range skippedPaths {
		if !wantSkipped[p] {
			t.Errorf("unexpected skip %s", p)
		}
	}

	// The entries directory itself linked out of the root: the whole source is skipped.
	root2 := mkTree(t, nil)
	symlink(t, outside, filepath.Join(root2, "data"))
	got, skipped, err = Discover(root2, srcs[:1])
	if err != nil || len(got) != 0 || len(skipped) != 1 || skipped[0].Path != "data" {
		t.Fatalf("linked data/: %v %v %v, want one skip of data", paths(got), skipped, err)
	}
}

// TestDiscoverSymlinkToRootParent: an entries directory linked to the directory that holds the
// root resolves to exactly "..", not below it; it is still outside and the source is skipped.
func TestDiscoverSymlinkToRootParent(t *testing.T) {
	root := mkTree(t, map[string]string{"docs/own.md": "# Own\n"})
	symlink(t, filepath.Dir(root), filepath.Join(root, "data"))
	got, skipped, err := Discover(root, []config.Source{{Path: "data", Layout: config.LayoutEntries}})
	if err != nil || len(got) != 0 || len(skipped) != 1 || skipped[0].Path != "data" || !strings.Contains(skipped[0].Reason, "outside the location root") {
		t.Fatalf("data -> root's parent: %v %v %v, want one skip of data", paths(got), skipped, err)
	}
}

// TestResolveSources: the root-relative path wins, then each entries source in order; a files
// entry resolves by its path; a miss is NotFound, an outside path Usage.
func TestResolveSources(t *testing.T) {
	root := mkTree(t, map[string]string{
		"data/a.md":            validFM,
		"data-adr/a.md":        validFM,
		"data-adr/b.md":        validFM,
		"data-adr/d/README.md": validFM,
		"docs/a.md":            "# A\n",
		"a.md":                 "# root a\n",
	})
	srcs := []config.Source{
		{Path: "data", Layout: config.LayoutEntries},
		{Path: "data-adr", Layout: config.LayoutEntries},
		files("docs/*.md"),
		files("a.md"),
	}
	for _, tt := range []struct{ arg, want string }{
		{"a.md", "a.md"}, // root-relative first
		{"b.md", "data-adr/b.md"},
		{"data/a.md", "data/a.md"},
		{"data-adr/a.md", "data-adr/a.md"},
		{"docs/a.md", "docs/a.md"},
		{"d", "data-adr/d/README.md"},
		{"d/", "data-adr/d/README.md"},
		{"./docs/a.md", "docs/a.md"},
		{filepath.Join(root, "docs", "a.md"), "docs/a.md"},
	} {
		e, err := Resolve(root, srcs, tt.arg)
		if err != nil || e.Path != tt.want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", tt.arg, e.Path, err, tt.want)
		}
	}
	for _, tt := range []struct {
		arg  string
		kind errs.Kind
	}{
		{"", errs.Usage},
		{"nope.md", errs.NotFound},
		{".", errs.NotFound},
		{"/elsewhere/x.md", errs.Usage},
	} {
		if _, err := Resolve(root, srcs, tt.arg); errs.KindOf(err) != tt.kind {
			t.Errorf("Resolve(%q) = %v (kind %v), want kind %v", tt.arg, err, errs.KindOf(err), tt.kind)
		}
	}
	// A Discover failure reaches the caller as is.
	if _, err := Resolve(root, []config.Source{files("**/a.md"), files("docs/*.md")}, "docs/a.md"); errs.KindOf(err) != errs.Usage {
		t.Errorf("overlapping sources: %v, want Usage", err)
	}
}

// TestResolveMatchesDiscover: Resolve walks lazily (no front matter read) but returns, for every
// entry, exactly what Discover returns for it: Meta for good entries, Err for broken ones (bad
// front matter, a missing required field).
func TestResolveMatchesDiscover(t *testing.T) {
	root := mkTree(t, map[string]string{
		"data/good.md":     validFM,
		"data/bad.md":      "---\ntitle: T\n---\nno summary\n",
		"data/d/README.md": validFM,
		"data/d/docs/x.md": "# X\n",
		"docs/plain.md":    "# Plain\n\nbody\n",
		"docs/broken.md":   "---\ntitle: x\n",
	})
	srcs := []config.Source{{Path: "data", Layout: config.LayoutEntries}, files("docs/*.md")}
	all, _, err := Discover(root, srcs)
	if err != nil || len(all) != 5 {
		t.Fatalf("Discover = %v, %v", paths(all), err)
	}
	if broken := len(all) - countOK(all); broken != 2 {
		t.Fatalf("fixture has %d broken entries, want 2 (data/bad.md, docs/broken.md)", broken)
	}
	lazy, _, err := discover(root, srcs, true)
	if err != nil || !reflect.DeepEqual(paths(lazy), paths(all)) {
		t.Fatalf("lazy discover = %v, %v; want %v", paths(lazy), err, paths(all))
	}
	for i, want := range all {
		if !reflect.DeepEqual(lazy[i].Meta, FrontMatter{}) || lazy[i].Err != nil {
			t.Errorf("lazy %s read its front matter: %+v %v", want.Path, lazy[i].Meta, lazy[i].Err)
		}
		got, err := Resolve(root, srcs, want.Path)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", want.Path, err)
		}
		if (got.Err == nil) != (want.Err == nil) || (got.Err != nil && got.Err.Error() != want.Err.Error()) {
			t.Errorf("Resolve(%s).Err = %v, Discover has %v", want.Path, got.Err, want.Err)
		}
		got.Err, want.Err = nil, nil
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Resolve(%s) = %+v\nDiscover has %+v", want.Path, got, want)
		}
	}
}

func countOK(entries []Entry) int {
	n := 0
	for _, e := range entries {
		if e.Err == nil {
			n++
		}
	}
	return n
}

// TestDiscoverCaseFoldOverlap: on a case-insensitive volume (APFS default) "docs/*.md" and
// "Docs/*.md" reach the same files; that is an overlap (Usage, naming both spellings), not each
// file indexed twice. On a case-sensitive volume "Docs" does not exist and only docs/ is indexed.
func TestDiscoverCaseFoldOverlap(t *testing.T) {
	root := mkTree(t, map[string]string{"docs/a.md": "# A\n"})
	_, statErr := os.Stat(filepath.Join(root, "Docs", "a.md"))
	entries, _, err := Discover(root, []config.Source{files("docs/*.md"), files("Docs/*.md")})
	if statErr != nil { // case-sensitive volume
		if err != nil || !reflect.DeepEqual(paths(entries), []string{"docs/a.md"}) {
			t.Fatalf("case-sensitive: %v, %v", paths(entries), err)
		}
		return
	}
	if errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), "Docs/a.md (the same file as docs/a.md)") {
		t.Fatalf("Discover = %v, %v; want a Usage overlap naming both spellings", paths(entries), err)
	}
	// One source alone, spelled either way, is fine.
	if entries, _, err := Discover(root, []config.Source{files("Docs/*.md")}); err != nil || len(entries) != 1 {
		t.Errorf("one source: %v, %v", paths(entries), err)
	}
}

func TestLoadErrorKinds(t *testing.T) {
	root := mkTree(t, map[string]string{"data/a.md": validFM})
	_, err := Load(root, Entry{Path: "data/gone.md", Files: []string{"data/gone.md"}})
	if errs.KindOf(err) != errs.NotFound || !strings.Contains(err.Error(), "data/gone.md") {
		t.Errorf("missing file: %v (kind %v), want NotFound naming it", err, errs.KindOf(err))
	}
	_, err = Load(root, Entry{Path: "data", Files: []string{"data"}})
	if errs.KindOf(err) != errs.Usage {
		t.Errorf("a directory: %v (kind %v), want Usage", err, errs.KindOf(err))
	}
}

// TestFallbackMeta is the fallback precedence table (PLAN 1.5 step 3): one row per source of the
// title and of the summary.
func TestFallbackMeta(t *testing.T) {
	long := strings.Repeat("word ", 100)
	tests := []struct {
		name               string
		fm                 FrontMatter
		body, path         string
		wantTitle, wantSum string
	}{
		{"front matter wins", FrontMatter{Title: "FM", Summary: "FS"}, "# H1\n\npara\n", "x.md", "FM", "FS"},
		{"H1 then paragraph", FrontMatter{}, "# H1\n\nThe first\nparagraph.\n\nSecond.\n", "x.md", "H1", "The first paragraph."},
		{"H1 not on the first line", FrontMatter{}, "Intro line.\n\n# Later\n", "x.md", "Later", "Intro line."},
		{"H1 inside a fence is ignored", FrontMatter{}, "```\n# not a title\nnot a para\n```\n", "my-notes.md", "My notes", ""},
		{"only deeper headings", FrontMatter{}, "## Section\n\nText.\n", "dir/install_guide.md", "Install guide", "Text."},
		{"hashes-only H1 is not a title", FrontMatter{}, "# ##\n", "a.md", "A", ""},
		{"title from fm, summary fallback", FrontMatter{Title: "FM"}, "# H1\n\npara\n", "x.md", "FM", "para"},
		{"summary from fm, title fallback", FrontMatter{Summary: "FS"}, "# H1\n", "x.md", "H1", "FS"},
		{"whitespace-only fm title falls back", FrontMatter{Title: "  "}, "# H1\n", "x.md", "H1", ""},
		{"badges and html skipped", FrontMatter{}, "# T\n\n[![CI](x)](y)\n\n<p align=center>\n\nReal text.\n", "x.md", "T", "Real text."},
		{"table and rule skipped", FrontMatter{}, "| a | b |\n\n---\n\nProse.\n", "x.md", "X", "Prose."},
		{"empty file", FrontMatter{}, "", "empty.md", "Empty", ""},
		{"summary cut to 200 runes", FrontMatter{}, long, "x.md", "X", strings.TrimSpace(long[:199*1]) + ""},
		{"control characters stripped", FrontMatter{}, "# a\x1bb\tc\n\nx\x00y\n", "x.md", "ab c", "xy"},
		{"name made of separators", FrontMatter{}, "", "dir/-_-.md", "dir/-_-.md", ""},
		{"two backticks are not a fence", FrontMatter{}, "``\n# Real\n``\n", "x.md", "Real", "``"},
		{"tilde fence hides its heading", FrontMatter{}, "~~~\n# no\n~~~\n", "x.md", "X", ""},
		{"a fence closes only on its own marker", FrontMatter{}, "```\n~~~\n# no\n```\n# yes\n", "x.md", "yes", ""},
		{"empty in-place summary falls back", FrontMatter{Summary: "  "}, "# H\n\npara\n", "x.md", "H", "para"},
		{"empty summary and title both fall back", FrontMatter{Title: "", Summary: ""}, "# H\n\npara\n", "x.md", "H", "para"},
		{"invalid UTF-8 in the name is dropped, not upper-cased", FrontMatter{}, "", "dir/\xffab.md", "ab", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fallbackMeta(tt.fm, []byte(tt.body), tt.path)
			if tt.name == "summary cut to 200 runes" {
				if n := utf8.RuneCountInString(got.Summary); n != FallbackSummaryRunes || !strings.HasSuffix(got.Summary, "…") {
					t.Errorf("summary is %d runes (%q), want %d ending in …", n, got.Summary, FallbackSummaryRunes)
				}
				return
			}
			if got.Title != tt.wantTitle || got.Summary != tt.wantSum {
				t.Errorf("fallbackMeta = (%q, %q), want (%q, %q)", got.Title, got.Summary, tt.wantTitle, tt.wantSum)
			}
		})
	}
}

// TestDiscoverInPlaceFrontMatterRules: in place, front matter is optional but what it sets must
// still be valid; a broken fence is an entry error, as in the entries layout.
func TestDiscoverInPlaceFrontMatterRules(t *testing.T) {
	root := mkTree(t, map[string]string{
		"docs/partial.md":  "---\ntitle: Only a title\n---\nFirst paragraph.\n",
		"docs/bad-cap.md":  "---\ntitle: " + strings.Repeat("x", MaxTitleRunes+1) + "\n---\n",
		"docs/unclosed.md": "---\ntitle: x\n",
		"docs/multi.md":    "---\nsummary: >\n  folded\n---\n",
	})
	got, _, err := Discover(root, []config.Source{files("docs/*.md")})
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]Entry{}
	for _, e := range got {
		byPath[e.Path] = e
	}
	if e := byPath["docs/partial.md"]; e.Err != nil || e.Meta.Title != "Only a title" || e.Meta.Summary != "First paragraph." {
		t.Errorf("partial: %+v", e)
	}
	for _, p := range []string{"docs/bad-cap.md", "docs/unclosed.md", "docs/multi.md"} {
		if byPath[p].Err == nil || !strings.Contains(byPath[p].Err.Error(), p) {
			t.Errorf("%s: Err = %v, want an error naming the file", p, byPath[p].Err)
		}
	}
}

// FuzzFallbackMeta: whatever the body and file name, the derived title is non-empty and both
// values pass the front-matter content rules (checkText), so a fallback never fails validation.
func FuzzFallbackMeta(f *testing.F) {
	for _, s := range [][2]string{
		{"# Title\n\nPara.\n", "a.md"}, {"", "-.md"}, {"\x00\x1b", "\x7f.md"}, {"```\n# x\n", "é.md"},
		{"# " + strings.Repeat("ü", 300), "x.md"}, {"\xff\xfe", "\xff.md"},
	} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, body, name string) {
		got := fallbackMeta(FrontMatter{}, []byte(body), "docs/"+name)
		if got.Title == "" {
			t.Fatalf("empty title for %q / %q", body, name)
		}
		if r := checkText("title", got.Title, MaxTitleRunes); r != "" {
			t.Fatalf("title %q: %s", got.Title, r)
		}
		if r := checkText("summary", got.Summary, FallbackSummaryRunes); r != "" {
			t.Fatalf("summary %q: %s", got.Summary, r)
		}
	})
}

// TestDiscoverFilesNonRegular: a *.md name that is a directory, a link to a directory or a FIFO
// is never an entry; an unreadable directory under a glob is a Usage error naming the source.
func TestDiscoverFilesNonRegular(t *testing.T) {
	root := mkTree(t, map[string]string{"docs/real.md": "# Real\n", "docs/dir.md/inner.md": "# Inner\n"})
	symlink(t, filepath.Join(root, "docs", "dir.md"), filepath.Join(root, "docs", "linkdir.md"))
	if err := syscall.Mkfifo(filepath.Join(root, "docs", "fifo.md"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, skipped, err := Discover(root, []config.Source{files("docs/*.md")})
	if err != nil || !reflect.DeepEqual(paths(got), []string{"docs/real.md"}) || len(skipped) != 0 {
		t.Fatalf("Discover = %v %v %v, want docs/real.md only", paths(got), skipped, err)
	}
	locked := filepath.Join(root, "docs", "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) }) // so t.TempDir can remove it
	_, _, err = Discover(root, []config.Source{files("docs/**/*.md")})
	if errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), `cannot read source "docs/**/*.md" (files)`) {
		t.Errorf("unreadable dir: %v (kind %v), want Usage naming the source", err, errs.KindOf(err))
	}
}

// BenchmarkResolve: one Resolve over a 2,000-file in-place source (leverage
// filesystem-boundary: show/edit/rm/add resolve their argument on every call). The lazy walk reads
// only the resolved file's front matter.
func BenchmarkResolve(b *testing.B) {
	root, err := filepath.EvalSymlinks(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 2000; i++ {
		p := filepath.Join(root, "docs", fmt.Sprintf("d%02d", i%20), fmt.Sprintf("f%04d.md", i))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(validFM+strings.Repeat("text ", 400)), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	srcs := []config.Source{files("docs/**/*.md")}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Resolve(root, srcs, "docs/d07/f1007.md"); err != nil {
			b.Fatal(err)
		}
	}
}

// unreadable makes dir unreadable for the rest of the test (skipped as root, which ignores modes).
func unreadable(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
}

// TestDiscoverUnreadableGlobBase: a glob base that exists but cannot be read is a Usage error
// naming the source, not an empty source (only a missing base is empty).
func TestDiscoverUnreadableGlobBase(t *testing.T) {
	root := mkTree(t, map[string]string{"docs/a.md": "a\n"})
	unreadable(t, filepath.Join(root, "docs"))
	_, _, err := Discover(root, []config.Source{files("docs/*.md")})
	if errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), "cannot read source") {
		t.Fatalf("Discover = %v (kind %v), want Usage \"cannot read source\"", err, errs.KindOf(err))
	}
}

// TestDiscoverDirEntryDocs: the files of a directory entry, one row per docs/ outcome.
func TestDiscoverDirEntryDocs(t *testing.T) {
	src := []config.Source{{Path: "data", Layout: config.LayoutEntries}}
	for _, tt := range []struct {
		name    string
		tree    map[string]string
		lock    bool // make data/d/docs unreadable
		wantErr bool
		files   []string
	}{
		{"no docs dir", map[string]string{"data/d/README.md": validFM}, false, false, []string{"data/d/README.md"}},
		{"docs dir unreadable", map[string]string{"data/d/README.md": validFM, "data/d/docs/x.md": "x\n"}, true, true, []string{"data/d/README.md"}},
		{"subdirs and non-md files left out", map[string]string{"data/d/README.md": validFM, "data/d/docs/b.md": "b\n",
			"data/d/docs/a.md": "a\n", "data/d/docs/x.txt": "x", "data/d/docs/sub/c.md": "c\n", "data/d/docs/dir.md/e.md": "e\n"},
			false, false, []string{"data/d/README.md", "data/d/docs/a.md", "data/d/docs/b.md"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := mkTree(t, tt.tree)
			if tt.lock {
				unreadable(t, filepath.Join(root, "data", "d", "docs"))
			}
			got, _, err := Discover(root, src)
			if err != nil || len(got) != 1 {
				t.Fatalf("Discover = %v, %v", paths(got), err)
			}
			if (got[0].Err != nil) != tt.wantErr || !reflect.DeepEqual(got[0].Files, tt.files) {
				t.Errorf("entry Err = %v, Files = %v; want err %v, files %v", got[0].Err, got[0].Files, tt.wantErr, tt.files)
			}
		})
	}
}

// TestFallbackSummaryBoundary: a paragraph of exactly FallbackSummaryRunes is kept whole; one
// rune more is cut to FallbackSummaryRunes-1 runes plus "…".
func TestFallbackSummaryBoundary(t *testing.T) {
	exact := strings.Repeat("é", FallbackSummaryRunes)
	if got := fallbackMeta(FrontMatter{}, []byte(exact+"\n"), "x.md").Summary; got != exact {
		t.Errorf("%d runes: summary %q, want it unchanged", FallbackSummaryRunes, got)
	}
	over := exact + "z"
	want := strings.Repeat("é", FallbackSummaryRunes-1) + "…"
	if got := fallbackMeta(FrontMatter{}, []byte(over+"\n"), "x.md").Summary; got != want {
		t.Errorf("%d runes: summary %q, want %q", FallbackSummaryRunes+1, got, want)
	}
}

// TestDiscoverFilesDanglingSymlink: a dangling link a files source matches is an entry whose
// file cannot be read (Err set), not skipped and not a Discover error.
func TestDiscoverFilesDanglingSymlink(t *testing.T) {
	root := mkTree(t, map[string]string{"docs/ok.md": "ok\n"})
	symlink(t, filepath.Join(root, "no-such.md"), filepath.Join(root, "docs", "dangling.md"))
	got, skipped, err := Discover(root, []config.Source{files("docs/*.md")})
	if err != nil || len(skipped) != 0 || !reflect.DeepEqual(paths(got), []string{"docs/dangling.md", "docs/ok.md"}) {
		t.Fatalf("Discover = %v skipped %v, %v", paths(got), skipped, err)
	}
	if got[0].Err == nil || got[1].Err != nil {
		t.Errorf("Err: dangling %v, ok %v; want only the dangling link to fail", got[0].Err, got[1].Err)
	}
}

// TestExcludesWholeDir is the decision table for pruning: only an exclude ending in "/**" whose
// head matches the directory or an ancestor covers it; a file-level exclude never does.
func TestExcludesWholeDir(t *testing.T) {
	for _, tc := range []struct {
		name     string
		excludes []string
		dir      string
		want     bool
	}{
		{"no excludes", nil, "docs/vendor", false},
		{"**/vendor/** at depth 2", []string{"**/vendor/**"}, "docs/vendor", true},
		{"**/vendor/** at the top", []string{"**/vendor/**"}, "vendor", true},
		{"**/vendor/** below the match", []string{"**/vendor/**"}, "docs/vendor/a/b", true},
		{"**/vendor/** other dir", []string{"**/vendor/**"}, "docs/other", false},
		{"**/vendor/** name prefix only", []string{"**/vendor/**"}, "docs/vendors", false},
		{"literal head", []string{"drafts/**"}, "drafts", true},
		{"literal head, sibling", []string{"drafts/**"}, "docs/drafts", false},
		{"wildcard head", []string{"d*/old/**"}, "docs/old", true},
		{"file pattern", []string{"**/*.md"}, "docs", false},
		{"dir name without /**", []string{"docs/vendor"}, "docs/vendor", false},
		{"** alone", []string{"**"}, "docs", true},
		{"second pattern matches", []string{"x/**", "**/vendor/**"}, "a/vendor", true},
	} {
		if got := excludesWholeDir(tc.excludes, tc.dir); got != tc.want {
			t.Errorf("%s: excludesWholeDir(%v, %q) = %v, want %v", tc.name, tc.excludes, tc.dir, got, tc.want)
		}
	}
}

// TestExcludePrunesWalk: an exclude covering a directory stops the walk at it (the directory
// visits are counted, not the time), and the entries found are the same with or without pruning.
func TestExcludePrunesWalk(t *testing.T) {
	root := mkTree(t, map[string]string{
		"docs/a.md":                validFM,
		"docs/vendor/x/y/deep.md":  validFM,
		"docs/vendor/x/z/other.md": validFM,
		"docs/sub/b.md":            validFM,
	})
	var visited []string
	got, _, err := discoverWalk(root, []config.Source{files("docs/**/*.md", "**/vendor/**")}, false,
		func(rel string) { visited = append(visited, rel) })
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"docs/a.md", "docs/sub/b.md"}; !reflect.DeepEqual(paths(got), want) {
		t.Errorf("entries = %v, want %v", paths(got), want)
	}
	for _, v := range visited {
		if strings.Contains(v, "vendor") {
			t.Errorf("walk descended into %s, want the excluded directory pruned (visited %v)", v, visited)
		}
	}
	if want := []string{"docs", "docs/sub"}; !reflect.DeepEqual(visited, want) {
		t.Errorf("directories visited = %v, want %v", visited, want)
	}
	// Without the exclude the walk goes in: the count above is the pruning, not the layout.
	visited = nil
	if _, _, err := discoverWalk(root, []config.Source{files("docs/**/*.md")}, false, func(rel string) { visited = append(visited, rel) }); err != nil {
		t.Fatal(err)
	}
	if len(visited) != 6 {
		t.Errorf("unpruned visits = %v, want 6 directories", visited)
	}
}
