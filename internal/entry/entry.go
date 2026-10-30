// Package entry discovers knowledge entries in a store's sources and parses their YAML front
// matter.
//
// A store lists its sources in config ([[sources]], config.Source); paths are relative to the
// location root. Each source has a layout:
//
//   - entries (kb owns the directory, e.g. data/): a file entry is every *.md directly in it
//     except AGENTS.md, CLAUDE.md, index.md, plan.md and README.md; a directory entry is every
//     directory directly in it that contains a README.md, except skill/, .kb/ and .git/, and its
//     indexed files are README.md and docs/*.md. Front matter with a title and a summary is
//     required.
//   - files (in place, e.g. docs/**/*.md): every *.md file the glob matches, minus exclude, is a
//     file entry. Front matter is optional: a missing title or summary falls back to the first
//     "# " heading and the first paragraph (see fallbackMeta). .kb, .git and node_modules are
//     never walked.
//
// The Path of an entry is always the root-relative path of its front door: "data/foo.md",
// "data/droplet/README.md", "docs/install.md".
//
// Discover never fails for a bad individual entry: a file it cannot read, front matter it cannot
// parse, or front matter missing a title/summary all land in that Entry's Err field instead. A
// symlink that leads outside the location root is never read: it is reported in Skipped. Only a
// failure to read a source directory, or two sources claiming one file, is fatal. This lets
// `kb list` show a broken entry instead of hiding it, and lets `kb reindex` warn on one bad
// entry, skip it, and keep going.
//
// Errors returned by Discover, Resolve and Load are *errs.Error: Usage (with the path) for a
// source that cannot be read or an argument outside the root, NotFound for "no such entry".
package entry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/errs"
)

// Kind values stored in the entries table.
const (
	KindFile = "file"
	KindDir  = "dir"
	// DataDir is the default store's entries directory, which kb init writes as its source.
	DataDir = "data"
)

// ValidationError marks front matter that parses structurally but fails a documented content
// rule: no title, no summary, or a summary that spans more than one line. `kb add` refuses an
// entry that fails this way before it is ever written; `kb reindex` logs a warning and skips it,
// continuing with the rest of the repository. Use errors.As to detect it across a wrapped error
// chain — ParseFrontMatter, Entry.Err and chunk.Split can all carry one.
type ValidationError struct {
	// Path is the repo-relative path of the offending file. It is empty when ParseFrontMatter is
	// called directly on bytes with no path in scope; callers that have a path (loadMeta, and
	// chunk.Split for the front-door check) fill it in.
	Path   string
	Reason string
}

func (e *ValidationError) Error() string {
	if e.Path == "" {
		return e.Reason
	}
	return fmt.Sprintf("%s: %s", e.Path, e.Reason)
}

// FrontMatter is the YAML header every entry carries. Updated and Verified are kept verbatim as
// strings because the store writes them through unchanged; Verified may be empty.
type FrontMatter struct {
	Title    string
	Summary  string
	Tags     []string
	Updated  string
	Verified string

	// Related, Supersedes and DependsOn list other entries by path (relative to this file) or as
	// [[wikilinks]]; internal/links turns them into edges. Aliases are alternative names the entry
	// answers to. All four take the same two list syntaxes as Tags. Unlike Tags, empty items are
	// dropped. Aliases are validated (see MaxAliases, MaxAliasRunes); the link lists are not.
	Related    []string
	Supersedes []string
	DependsOn  []string
	Aliases    []string
}

// listField returns the FrontMatter slice that front-matter key names, or nil when key is not a
// list key.
func (fm *FrontMatter) listField(key string) *[]string {
	switch key {
	case "tags":
		return &fm.Tags
	case "related":
		return &fm.Related
	case "supersedes":
		return &fm.Supersedes
	case "depends_on":
		return &fm.DependsOn
	case "aliases":
		return &fm.Aliases
	}
	return nil
}

// Entry is one knowledge entry. Files lists every indexed file, root-relative, in index order
// (README.md first, then docs/*.md sorted by name). Dir is the directory name for a directory
// entry and "" for a file entry. Layout is the layout of the source it came from
// (config.LayoutEntries or config.LayoutFiles) and Source that source's index in the store's
// [[sources]] list.
//
// Err is non-nil when this entry's front door could not be read or its front matter could not be
// parsed or validated; Meta is the zero value in that case. Discover still returns such an
// entry — it never drops one silently — so every caller that trusts Meta must check Err first.
// A *ValidationError in Err (test with errors.As) means the file itself is fine but its content
// is incomplete (no title/summary); any other error means the file could not be read at all or
// its front matter is structurally broken (e.g. an unterminated "---" fence).
type Entry struct {
	Path   string
	Kind   string
	Dir    string
	Files  []string
	Layout string
	Source int
	Meta   FrontMatter
	Err    error
}

// InPlace reports whether the entry comes from a files source: kb indexes it but does not own
// the file (kb rm refuses it, kb add never writes there).
func (e Entry) InPlace() bool { return e.Layout == config.LayoutFiles }

// HasTag reports whether the entry carries tag, case-insensitively.
func (e Entry) HasTag(tag string) bool {
	for _, t := range e.Meta.Tags {
		if strings.EqualFold(t, tag) {
			return true
		}
	}
	return false
}

// Skipped is a file or directory Discover left out on purpose, with the reason, for kb reindex
// to warn about and kb doctor to report.
type Skipped struct {
	Path   string
	Reason string
}

func (s Skipped) String() string { return s.Path + ": " + s.Reason }

// Discover reads every source of a store under root and returns its entries sorted by Path, plus
// what it skipped (a symlink leading outside root). A file or directory entry whose front matter
// cannot be read, parsed or validated is still returned, with Err set (see the Entry.Err doc).
//
// A source directory that does not exist (an entries dir not made yet, a glob whose base is
// missing) is an empty source, not an error: callers that care, such as kb doctor, check it
// themselves. No sources means no entries. The errors Discover returns are Usage: a source
// directory that exists but cannot be read, or two sources that both match one file.
func Discover(root string, sources []config.Source) ([]Entry, []Skipped, error) {
	return discover(root, sources, false)
}

// discover is Discover; with lazy set it leaves every Entry's Meta and Err unread (no file is
// opened), for Resolve, which reads only the entry it returns.
func discover(root string, sources []config.Source, lazy bool) ([]Entry, []Skipped, error) {
	return discoverWalk(root, sources, lazy, nil)
}

// discoverWalk is discover with onDir, when set, called with the root-relative path of every
// directory a files source descends into (a test counts them to prove an exclude prunes the walk).
func discoverWalk(root string, sources []config.Source, lazy bool, onDir func(rel string)) ([]Entry, []Skipped, error) {
	d := discovery{root: root, lazy: lazy, onDir: onDir, owner: map[string]int{}, folded: map[string]string{}}
	if real, err := filepath.EvalSymlinks(root); err == nil {
		d.realRoot = real
	} else {
		d.realRoot = filepath.Clean(root)
	}
	for i, src := range sources {
		var err error
		switch src.Layout {
		case config.LayoutEntries:
			err = d.entriesSource(i, src)
		case config.LayoutFiles:
			err = d.filesSource(i, src)
		default:
			// config.Validate refuses any other layout before a source gets here.
			err = errs.New(errs.Usage, "entry.Discover", "source %d: unknown layout %q", i+1, src.Layout)
		}
		if err != nil {
			return nil, nil, err
		}
	}
	sort.Slice(d.entries, func(i, j int) bool { return d.entries[i].Path < d.entries[j].Path })
	return d.entries, d.skipped, nil
}

// discovery is the state of one Discover run.
type discovery struct {
	root, realRoot string
	lazy           bool             // skip loadMeta (see discover)
	onDir          func(rel string) // see discoverWalk
	entries        []Entry
	skipped        []Skipped
	owner          map[string]int    // root-relative file -> index of the source that claimed it
	folded         map[string]string // lower-cased root-relative file -> the first spelling claimed
}

// meta fills e.Meta (or e.Err) unless the run is lazy.
func (d *discovery) meta(e *Entry) {
	if !d.lazy {
		loadMeta(d.root, e)
	}
}

// add records e, after checking that no earlier source already claimed one of its files. Two
// spellings that differ only in case ("docs/a.md", "Docs/a.md") are the same file on a
// case-insensitive volume (macOS APFS by default): when they are, that is an overlap too.
func (d *discovery) add(e Entry, src config.Source) error {
	for _, f := range e.Files {
		prev, ok := d.owner[f]
		claimed := f
		if !ok {
			if first, seen := d.folded[strings.ToLower(f)]; seen && d.sameFile(first, f) {
				prev, ok, claimed = d.owner[first], true, first
			}
		}
		if ok && prev != e.Source {
			return errs.New(errs.Usage, "entry.Discover",
				"sources[%d] and sources[%d] (%s) both match %s; a file may belong to one source of a store: narrow one with exclude",
				prev+1, e.Source+1, src, overlapName(claimed, f)).With("path", f)
		}
		d.owner[f] = e.Source
		if _, seen := d.folded[strings.ToLower(f)]; !seen {
			d.folded[strings.ToLower(f)] = f
		}
	}
	d.entries = append(d.entries, e)
	return nil
}

// sameFile reports whether two root-relative spellings name one file on disk.
func (d *discovery) sameFile(a, b string) bool {
	fa, err := os.Stat(filepath.Join(d.root, filepath.FromSlash(a)))
	if err != nil {
		return false
	}
	fb, err := os.Stat(filepath.Join(d.root, filepath.FromSlash(b)))
	return err == nil && os.SameFile(fa, fb)
}

// overlapName is the file an overlap error names: one path, or both spellings of one file.
func overlapName(first, f string) string {
	if first == f {
		return f
	}
	return f + " (the same file as " + first + ")"
}

// entriesSource reads one entries directory one level deep, with the rules of the package doc.
func (d *discovery) entriesSource(idx int, src config.Source) error {
	dir := src.Path
	if out, err := d.outside(dir); out {
		d.skip(dir, err)
		return nil
	}
	items, err := os.ReadDir(filepath.Join(d.root, filepath.FromSlash(dir)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errs.Wrap(err, errs.Usage, "entry.Discover", "cannot read the entries directory %s: %v", dir, err).With("path", dir)
	}
	for _, item := range items {
		name := item.Name()
		rel := dir + "/" + name
		switch {
		case item.IsDir():
			if config.ReservedEntryDir(name) {
				continue
			}
			readme := filepath.Join(d.root, filepath.FromSlash(rel), "README.md")
			if info, err := os.Stat(readme); err != nil || info.IsDir() {
				continue
			}
			if out, why := d.outside(rel + "/README.md"); out {
				d.skip(rel+"/README.md", why)
				continue
			}
			e := Entry{Path: rel + "/README.md", Kind: KindDir, Dir: rel, Layout: config.LayoutEntries, Source: idx}
			files, err := d.dirFiles(rel)
			if err != nil {
				e.Err = err
				e.Files = []string{e.Path}
			} else {
				e.Files = files
				d.meta(&e)
			}
			if err := d.add(e, src); err != nil {
				return err
			}
		case strings.HasSuffix(name, ".md") && !config.ReservedEntryFile(name):
			if item.Type()&fs.ModeSymlink != 0 {
				if out, why := d.outside(rel); out {
					d.skip(rel, why)
					continue
				}
			}
			e := Entry{Path: rel, Kind: KindFile, Files: []string{rel}, Layout: config.LayoutEntries, Source: idx}
			d.meta(&e)
			if err := d.add(e, src); err != nil {
				return err
			}
		}
	}
	return nil
}

// dirFiles returns the indexed files of a directory entry: README.md, then docs/*.md by name. A
// docs file that is a symlink out of the root is skipped.
func (d *discovery) dirFiles(dir string) ([]string, error) {
	files := []string{dir + "/README.md"}
	docs, err := os.ReadDir(filepath.Join(d.root, filepath.FromSlash(dir), "docs"))
	if err != nil {
		if os.IsNotExist(err) {
			return files, nil
		}
		return nil, fmt.Errorf("read %s/docs: %w", dir, err)
	}
	var names []string
	for _, f := range docs {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".md") {
			continue
		}
		rel := dir + "/docs/" + f.Name()
		if f.Type()&fs.ModeSymlink != 0 {
			if out, why := d.outside(rel); out {
				d.skip(rel, why)
				continue
			}
		}
		names = append(names, f.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		files = append(files, dir+"/docs/"+n)
	}
	return files, nil
}

// skip records a path left out on purpose.
func (d *discovery) skip(rel, reason string) {
	d.skipped = append(d.skipped, Skipped{Path: rel, Reason: reason})
}

// outside reports whether the root-relative path rel resolves, through symlinks, to somewhere
// outside the location root, with the reason for Skipped. A path that does not resolve (missing,
// or a dangling link) is not outside: reading it fails later and is reported as that entry's Err.
func (d *discovery) outside(rel string) (bool, string) {
	real, err := filepath.EvalSymlinks(filepath.Join(d.root, filepath.FromSlash(rel)))
	if err != nil {
		return false, ""
	}
	r, err := filepath.Rel(d.realRoot, real)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return true, "symlink to " + real + ", outside the location root; not indexed"
	}
	return false, ""
}

// loadMeta reads the entry's front-door file and fills e.Meta. On any failure — the file cannot
// be read, its front matter is structurally broken, or it validates as incomplete — it sets e.Err
// instead and leaves e.Meta at the zero value. It never returns an error: Discover must keep
// going past one bad entry (see the package and Entry.Err docs). An in-place entry (files layout)
// gets the fallback title and summary instead of a "missing" error (fallbackMeta).
func loadMeta(root string, e *Entry) {
	src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(e.Path)))
	if err != nil {
		e.Err = fmt.Errorf("read %s: %w", e.Path, err)
		return
	}
	var meta FrontMatter
	if e.InPlace() {
		var body []byte
		meta, body, err = parseOptional(src)
		if err == nil {
			meta = fallbackMeta(meta, body, e.Path)
		}
	} else {
		meta, _, err = ParseFrontMatter(src)
	}
	if err != nil {
		var ve *ValidationError
		if errors.As(err, &ve) {
			e.Err = &ValidationError{Path: e.Path, Reason: ve.Reason}
			return
		}
		e.Err = fmt.Errorf("%s: %w", e.Path, err)
		return
	}
	e.Meta = meta
}

// Resolve turns one <entry> command-line argument into the Entry it names. It tries, in order:
// the argument as a root-relative path ("docs/install.md", "data/foo.md"), then the argument
// inside each entries source in config order ("foo.md", "droplet", "droplet/" and
// "droplet/README.md" for data/). An absolute path is made root-relative first and must be
// inside root. A file entry matches its Path; a directory entry its Path or its Dir.
//
// It is a NotFound error if the argument names no discovered entry, and a Usage error if it is
// empty or outside root. An entry whose own front matter is broken is still returned (Err is not
// considered here; the caller decides what to do with it).
func Resolve(root string, sources []config.Source, arg string) (Entry, error) {
	trimmedArg := strings.TrimSpace(arg)
	if trimmedArg == "" {
		return Entry{}, errs.New(errs.Usage, "entry.Resolve", "resolve: empty entry argument")
	}

	rel := filepath.ToSlash(trimmedArg)
	if filepath.IsAbs(trimmedArg) {
		absRoot, err := filepath.Abs(root)
		if err != nil {
			return Entry{}, errs.Wrap(err, errs.Usage, "entry.Resolve", "resolve %q: %v", arg, err)
		}
		r, err := filepath.Rel(absRoot, filepath.Clean(trimmedArg))
		if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			return Entry{}, errs.New(errs.Usage, "entry.Resolve", "resolve %q: not inside %s", arg, root)
		}
		rel = filepath.ToSlash(r)
	}
	rel = strings.TrimPrefix(rel, "./")
	rel = strings.TrimSuffix(rel, "/")
	notFound := errs.New(errs.NotFound, "entry.Resolve", "resolve %q: not a knowledge entry", arg).With("path", arg)
	if rel == "" || rel == "." {
		return Entry{}, notFound
	}

	candidates := []string{rel}
	if !filepath.IsAbs(trimmedArg) {
		for _, src := range sources {
			if src.Layout == config.LayoutEntries && !strings.HasPrefix(rel, src.Path+"/") {
				candidates = append(candidates, src.Path+"/"+rel)
			}
		}
	}

	// A lazy walk: the overlap and symlink rules still apply, but only the entry returned has its
	// front matter read (a full Discover reads every file: ~0.5 s per 10k, on every show/edit/rm).
	entries, _, err := discover(root, sources, true)
	if err != nil {
		return Entry{}, err
	}
	for _, c := range candidates {
		for _, e := range entries {
			if e.Path == c || (e.Kind == KindDir && e.Dir == c) {
				if e.Err == nil {
					loadMeta(root, &e)
				}
				return e, nil
			}
		}
	}
	return Entry{}, notFound
}

// Load reads every file in e.Files and returns them keyed by their repo-relative path, exactly as
// chunk.Split and BodyHash expect. A file that is gone is NotFound; any other read failure is
// Usage; both name the file.
func Load(root string, e Entry) (map[string][]byte, error) {
	files := make(map[string][]byte, len(e.Files))
	for _, f := range e.Files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil {
			kind := errs.Usage
			if errors.Is(err, fs.ErrNotExist) {
				kind = errs.NotFound
			}
			return nil, errs.Wrap(err, kind, "entry.Load", "load %s: %v", f, err).With("path", f)
		}
		files[f] = data
	}
	return files, nil
}

// BodyHash is the entries.body_hash value: sha256 of every file in order, concatenated. order is
// normally e.Files; it is a separate parameter so callers that already have it don't need to
// carry the whole Entry around.
func BodyHash(files map[string][]byte, order []string) string {
	h := sha256.New()
	for _, p := range order {
		h.Write(files[p])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ParseFrontMatter splits a Markdown source into its YAML front matter and the body that follows
// it. A file with no leading "---" fence has no front matter: an empty FrontMatter and the whole
// source are returned, without an error — this is the common case for a file with nothing to
// validate (a docs/*.md file need not carry front matter at all).
//
// When a "---" fenced block is present, ParseFrontMatter reads the keys title, summary, updated,
// verified and the lists tags, related, supersedes, depends_on, aliases, tolerating: CRLF line
// endings and a leading BOM; a quoted or unquoted scalar value; a value containing further colons or em/en dashes after the "key:" delimiter
// (only the first colon on the line separates key from value); an inline list
// ("tags: [a, b]") or a block list ("tags:" followed by "  - a" lines); and blank or comment
// ("#...") lines inside the block. Everything else in the block is ignored.
//
// If the fence never closes, that is a structural error (plain, not *ValidationError) and body is
// the whole original source. If the fence closes but title or summary is missing, or summary
// spans more than one line, ParseFrontMatter returns a *ValidationError — but still returns the
// correctly parsed fm and the correctly stripped body, so a caller that only wants the body
// stripped (chunk.Split, for a docs/*.md file's own front matter) can use errors.As to ignore
// this specific error and keep going.
func ParseFrontMatter(src []byte) (FrontMatter, []byte, error) {
	fm, body, fenced, err := scanFrontMatter(src)
	if err != nil || !fenced {
		return fm, body, err
	}
	if strings.TrimSpace(fm.Title) == "" || strings.TrimSpace(fm.Summary) == "" {
		return fm, body, &ValidationError{Reason: "front matter must set both title and summary"}
	}
	if reason := checkFields(fm); reason != "" {
		return fm, body, &ValidationError{Reason: reason}
	}
	return fm, body, nil
}

// parseOptional is ParseFrontMatter for an in-place file (files layout), whose front matter is
// optional: a missing title or summary is not an error (fallbackMeta fills it in), but a value
// that is present must still pass the same rules (one line, caps, no control characters), and a
// structurally broken fence is still an error.
func parseOptional(src []byte) (FrontMatter, []byte, error) {
	fm, body, _, err := scanFrontMatter(src)
	if err != nil {
		return fm, body, err
	}
	if reason := checkFields(fm); reason != "" {
		return fm, body, &ValidationError{Reason: reason}
	}
	return fm, body, nil
}

// checkFields applies the content rules to whatever title, summary and aliases are set.
func checkFields(fm FrontMatter) string {
	if fm.Title != "" {
		if reason := checkText("title", fm.Title, MaxTitleRunes); reason != "" {
			return reason
		}
	}
	if fm.Summary != "" {
		if reason := checkText("summary", fm.Summary, MaxSummaryRunes); reason != "" {
			return reason
		}
	}
	return checkAliases(fm.Aliases)
}

// scanFrontMatter is the structural half of ParseFrontMatter: it finds the fence, reads the keys
// and refuses multi-line title/summary shapes. fenced is false when there is no front matter.
func scanFrontMatter(src []byte) (fm FrontMatter, body []byte, fenced bool, err error) {
	rest := bytes.TrimPrefix(src, []byte("\ufeff"))
	if !bytes.HasPrefix(rest, []byte("---\n")) && !bytes.HasPrefix(rest, []byte("---\r\n")) {
		return fm, rest, false, nil
	}
	lines := strings.Split(strings.ReplaceAll(string(rest), "\r\n", "\n"), "\n")
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t") == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return fm, src, true, fmt.Errorf("front matter is not closed by a '---' line")
	}
	body = []byte(strings.Join(lines[end+1:], "\n"))
	// This parser has no support for YAML multi-line scalars at all \u2014 block (title: |, summary: >,
	// with chomping/indentation modifiers) or plain (an unindented continuation line with no key).
	// Both shapes read as a single-line value if left unrecognized: a block-scalar indicator would
	// parse as the literal string "|" or ">", and a plain continuation line would just be ignored
	// (silently truncating the value to its first line). Since "summary must be one line" is a
	// documented rule, both shapes are rejected outright as a *ValidationError instead.
	key := ""
	for _, line := range lines[1:end] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "- ") { // continuation of a block list
			if p := fm.listField(key); p != nil {
				item := unquote(strings.TrimSpace(trimmed[2:]))
				if item != "" || key == "tags" { // tags keep empty items, as they always did
					*p = append(*p, item)
				}
			}
			continue
		}
		name, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			if key == "title" || key == "summary" {
				return fm, body, true, &ValidationError{Reason: key + " must be one line"}
			}
			continue
		}
		key = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		switch key {
		case "title":
			if value == "" || isBlockScalarIndicator(value) {
				return fm, body, true, &ValidationError{Reason: "title must be one line"}
			}
			fm.Title = unquote(value)
		case "summary":
			if value == "" || isBlockScalarIndicator(value) {
				return fm, body, true, &ValidationError{Reason: "summary must be one line"}
			}
			fm.Summary = unquote(value)
		case "updated":
			fm.Updated = unquote(value)
		case "verified":
			fm.Verified = unquote(value)
		default:
			if p := fm.listField(key); p != nil {
				*p = parseList(value)
			}
		}
	}

	return fm, body, true, nil
}

// MaxAliases and MaxAliasRunes cap the aliases list. Aliases join the FTS title column, so like a
// title they must stay short, one-line display text; MaxAliasRunes is half of MaxTitleRunes
// because an alias is a name, not a headline.
const (
	MaxAliases    = 20
	MaxAliasRunes = 100
)

// checkAliases returns why an aliases list is unacceptable, or "".
func checkAliases(aliases []string) string {
	if len(aliases) > MaxAliases {
		return fmt.Sprintf("aliases has %d items; the limit is %d", len(aliases), MaxAliases)
	}
	for _, a := range aliases {
		if reason := checkText("alias", a, MaxAliasRunes); reason != "" {
			return reason
		}
	}
	return ""
}

// MaxTitleRunes and MaxSummaryRunes cap the front-matter title and summary. Both are shown on one
// line in search results and indexed (the title in every chunk's FTS row, the summary as chunk
// 0), so a runaway value bloats every hit. The caps are generous: a title is a headline, a summary
// one sentence or two.
const (
	MaxTitleRunes   = 200
	MaxSummaryRunes = 1000
)

// checkText returns why a title or summary value is unacceptable, or "": it must be valid UTF-8,
// hold no control characters (NUL, ESC, DEL, C1 and the like; a tab too, since it is one line of
// display text) and be at most max runes.
func checkText(field, v string, max int) string {
	if !utf8.ValidString(v) {
		return field + " is not valid UTF-8"
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return fmt.Sprintf("%s contains a control character (%U)", field, r)
		}
	}
	if n := utf8.RuneCountInString(v); n > max {
		return fmt.Sprintf("%s is %d characters; the limit is %d", field, n, max)
	}
	return ""
}

// parseList reads an inline list ("[a, b]") or returns nil for a block list, whose items arrive on
// the following lines. Items are split on every comma, so an inline item cannot contain one; use
// the block form with a quoted item for that.
func parseList(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	value = strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
	var items []string
	for _, part := range strings.Split(value, ",") {
		if t := unquote(strings.TrimSpace(part)); t != "" {
			items = append(items, t)
		}
	}
	return items
}

// isBlockScalarIndicator reports whether value is a bare YAML block-scalar indicator: "|" or ">",
// optionally followed by a single chomping ("-", "+") or explicit-indentation (a digit) modifier —
// "|", ">", "|-", ">+", "|2", and so on. This parser does not implement YAML block scalars, so a
// "title:"/"summary:" line shaped like one is rejected as a validation error by the caller rather
// than silently read as the literal indicator characters.
func isBlockScalarIndicator(value string) bool {
	if len(value) == 0 || (value[0] != '|' && value[0] != '>') {
		return false
	}
	if len(value) == 1 {
		return true
	}
	if len(value) != 2 {
		return false
	}
	c := value[1]
	return c == '-' || c == '+' || (c >= '0' && c <= '9')
}

// unquote strips one layer of matching single or double quotes and any trailing comment-free space.
func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
