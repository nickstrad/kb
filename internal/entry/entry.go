// Package entry discovers knowledge entries in the repository and parses their YAML front matter.
//
// An entry is one of two shapes (AGENTS.md, "Which files are entries"):
//
//   - a file entry: every *.md directly under the repo root except AGENTS.md, CLAUDE.md,
//     index.md, plan.md and README.md;
//   - a directory entry: every directory directly under the repo root that contains a README.md,
//     except skill/, .kb/ and .git/. Its indexed files are README.md and docs/*.md.
//
// The Path of an entry is always the repo-relative path of its front door: "foo.md" for a file
// entry, "droplet/README.md" for a directory entry.
//
// Discover never fails for a bad individual entry — a file it cannot read, front matter it
// cannot parse, or front matter missing a title/summary all land in that Entry's Err field
// instead. Only a failure to read the repo root itself is fatal. This lets `kb list` show a
// broken entry instead of disappearing entirely, and lets `kb reindex` warn on one bad entry,
// skip it, and keep going — see the Err field doc below.
package entry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Kind values stored in the entries table.
const (
	KindFile = "file"
	KindDir  = "dir"
)

// excludedRootFiles are the *.md files at the repo root that are repository machinery, not entries.
var excludedRootFiles = map[string]bool{
	"AGENTS.md": true,
	"CLAUDE.md": true,
	"index.md":  true,
	"plan.md":   true,
	"README.md": true,
}

// excludedDirs are the directories at the repo root that are never entries even with a README.md.
var excludedDirs = map[string]bool{
	"skill": true,
	".kb":   true,
	".git":  true,
}

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
}

// Entry is one knowledge entry. Files lists every indexed file, repo-relative, in index order
// (README.md first, then docs/*.md sorted by name). Dir is the directory name for a directory
// entry and "" for a file entry.
//
// Err is non-nil when this entry's front door could not be read or its front matter could not be
// parsed or validated; Meta is the zero value in that case. Discover still returns such an
// entry — it never drops one silently — so every caller that trusts Meta must check Err first.
// A *ValidationError in Err (test with errors.As) means the file itself is fine but its content
// is incomplete (no title/summary); any other error means the file could not be read at all or
// its front matter is structurally broken (e.g. an unterminated "---" fence).
type Entry struct {
	Path  string
	Kind  string
	Dir   string
	Files []string
	Meta  FrontMatter
	Err   error
}

// HasTag reports whether the entry carries tag, case-insensitively.
func (e Entry) HasTag(tag string) bool {
	for _, t := range e.Meta.Tags {
		if strings.EqualFold(t, tag) {
			return true
		}
	}
	return false
}

// Discover walks the repo root one level deep and returns every entry, sorted by Path. A file or
// directory entry whose front matter cannot be read, parsed or validated is still returned, with
// Err set (see the Entry.Err doc). The only error Discover itself returns is a failure to read
// root, which no individual entry can route around.
func Discover(root string) ([]Entry, error) {
	items, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read repo root %s: %w", root, err)
	}
	var entries []Entry
	for _, item := range items {
		name := item.Name()
		switch {
		case item.IsDir():
			if excludedDirs[name] {
				continue
			}
			readme := filepath.Join(root, name, "README.md")
			if info, err := os.Stat(readme); err != nil || info.IsDir() {
				continue
			}
			e := Entry{Path: name + "/README.md", Kind: KindDir, Dir: name}
			files, err := dirFiles(root, name)
			if err != nil {
				e.Err = err
				entries = append(entries, e)
				continue
			}
			e.Files = files
			loadMeta(root, &e)
			entries = append(entries, e)
		case strings.HasSuffix(name, ".md") && !excludedRootFiles[name]:
			e := Entry{Path: name, Kind: KindFile, Files: []string{name}}
			loadMeta(root, &e)
			entries = append(entries, e)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

// dirFiles returns the indexed files of a directory entry: README.md, then docs/*.md by name.
func dirFiles(root, dir string) ([]string, error) {
	files := []string{dir + "/README.md"}
	docs, err := os.ReadDir(filepath.Join(root, dir, "docs"))
	if err != nil {
		if os.IsNotExist(err) {
			return files, nil
		}
		return nil, fmt.Errorf("read %s/docs: %w", dir, err)
	}
	var names []string
	for _, d := range docs {
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			continue
		}
		names = append(names, d.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		files = append(files, dir+"/docs/"+n)
	}
	return files, nil
}

// loadMeta reads the entry's front-door file and fills e.Meta. On any failure — the file cannot
// be read, its front matter is structurally broken, or it validates as incomplete — it sets e.Err
// instead and leaves e.Meta at the zero value. It never returns an error: Discover must keep
// going past one bad entry (see the package and Entry.Err docs).
func loadMeta(root string, e *Entry) {
	src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(e.Path)))
	if err != nil {
		e.Err = fmt.Errorf("read %s: %w", e.Path, err)
		return
	}
	meta, _, err := ParseFrontMatter(src)
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

// Resolve turns one <entry> command-line argument into the Entry it names, per the CLI argument
// forms in plan.md: a root-relative file ("foo.md"), a directory name with or without a trailing
// slash ("droplet", "droplet/"), a directory's explicit front door ("droplet/README.md"), or an
// absolute path inside the repository. It is an error if the argument does not name a discovered
// entry — including an entry whose own front matter is broken (Err is not considered here; a
// broken entry is still an entry, and the caller decides what to do with Err).
func Resolve(root, arg string) (Entry, error) {
	trimmedArg := strings.TrimSpace(arg)
	if trimmedArg == "" {
		return Entry{}, fmt.Errorf("resolve: empty entry argument")
	}

	rel := filepath.ToSlash(trimmedArg)
	if filepath.IsAbs(trimmedArg) {
		absRoot, err := filepath.Abs(root)
		if err != nil {
			return Entry{}, fmt.Errorf("resolve %q: %w", arg, err)
		}
		absArg, err := filepath.Abs(trimmedArg)
		if err != nil {
			return Entry{}, fmt.Errorf("resolve %q: %w", arg, err)
		}
		r, err := filepath.Rel(absRoot, absArg)
		if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			return Entry{}, fmt.Errorf("resolve %q: not inside %s", arg, root)
		}
		rel = filepath.ToSlash(r)
	}
	rel = strings.TrimPrefix(rel, "./")
	rel = strings.TrimSuffix(rel, "/")
	if rel == "" || rel == "." {
		return Entry{}, fmt.Errorf("resolve %q: not a knowledge entry", arg)
	}

	entries, err := Discover(root)
	if err != nil {
		return Entry{}, err
	}
	for _, e := range entries {
		if e.Path == rel || (e.Kind == KindDir && e.Dir == rel) {
			return e, nil
		}
	}
	return Entry{}, fmt.Errorf("resolve %q: not a knowledge entry", arg)
}

// Load reads every file in e.Files and returns them keyed by their repo-relative path, exactly as
// chunk.Split and BodyHash expect.
func Load(root string, e Entry) (map[string][]byte, error) {
	files := make(map[string][]byte, len(e.Files))
	for _, f := range e.Files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil {
			return nil, fmt.Errorf("load %s: %w", f, err)
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
// When a "---" fenced block is present, ParseFrontMatter reads five keys — title, summary, tags,
// updated, verified — tolerating: CRLF line endings and a leading BOM; a quoted or unquoted
// scalar value; a value containing further colons or em/en dashes after the "key:" delimiter
// (only the first colon on the line separates key from value); an inline tag list
// ("tags: [a, b]") or a block tag list ("tags:" followed by "  - a" lines); and blank or comment
// ("#...") lines inside the block. Everything else in the block is ignored.
//
// If the fence never closes, that is a structural error (plain, not *ValidationError) and body is
// the whole original source. If the fence closes but title or summary is missing, or summary
// spans more than one line, ParseFrontMatter returns a *ValidationError — but still returns the
// correctly parsed fm and the correctly stripped body, so a caller that only wants the body
// stripped (chunk.Split, for a docs/*.md file's own front matter) can use errors.As to ignore
// this specific error and keep going.
func ParseFrontMatter(src []byte) (FrontMatter, []byte, error) {
	var fm FrontMatter
	rest := bytes.TrimPrefix(src, []byte("\ufeff"))
	if !bytes.HasPrefix(rest, []byte("---\n")) && !bytes.HasPrefix(rest, []byte("---\r\n")) {
		return fm, rest, nil
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
		return fm, src, fmt.Errorf("front matter is not closed by a '---' line")
	}
	body := []byte(strings.Join(lines[end+1:], "\n"))

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
			if key == "tags" {
				fm.Tags = append(fm.Tags, unquote(strings.TrimSpace(trimmed[2:])))
			}
			continue
		}
		name, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			if key == "title" || key == "summary" {
				return fm, body, &ValidationError{Reason: key + " must be one line"}
			}
			continue
		}
		key = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		switch key {
		case "title":
			if value == "" || isBlockScalarIndicator(value) {
				return fm, body, &ValidationError{Reason: "title must be one line"}
			}
			fm.Title = unquote(value)
		case "summary":
			if value == "" || isBlockScalarIndicator(value) {
				return fm, body, &ValidationError{Reason: "summary must be one line"}
			}
			fm.Summary = unquote(value)
		case "updated":
			fm.Updated = unquote(value)
		case "verified":
			fm.Verified = unquote(value)
		case "tags":
			fm.Tags = parseTags(value)
		}
	}

	if strings.TrimSpace(fm.Title) == "" || strings.TrimSpace(fm.Summary) == "" {
		return fm, body, &ValidationError{Reason: "front matter must set both title and summary"}
	}

	return fm, body, nil
}

// parseTags reads an inline list ("[a, b]") or returns nil for a block list, whose items arrive on
// the following lines.
func parseTags(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	value = strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
	var tags []string
	for _, part := range strings.Split(value, ",") {
		if t := unquote(strings.TrimSpace(part)); t != "" {
			tags = append(tags, t)
		}
	}
	return tags
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
