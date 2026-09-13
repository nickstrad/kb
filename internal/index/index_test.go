package index

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knowledge/kb/internal/entry"
)

// repoRoot is the knowledge repository this module lives in: kb/internal/index -> ../../..
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return root
}

// tableRows returns the "| ... |" lines of the table under the given "## Heading".
func tableRows(doc, heading string) []string {
	_, after, ok := strings.Cut(doc, "\n## "+heading+"\n")
	if !ok {
		return nil
	}
	var rows []string
	for _, line := range strings.Split(after, "\n") {
		if !strings.HasPrefix(line, "|") {
			if len(rows) > 0 {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "| --- ") || strings.HasPrefix(line, "| Entry |") || strings.HasPrefix(line, "| Path |") {
			continue
		}
		rows = append(rows, line)
	}
	return rows
}

// section returns everything from "## Heading" to the end of the document.
func section(doc, heading string) string {
	i := strings.Index(doc, "## "+heading+"\n")
	if i < 0 {
		return ""
	}
	return doc[i:]
}

// TestGenerateMatchesCommittedIndex is the golden test: generating from the real repository must
// reproduce every row of the index.md on disk, byte for byte and in the same relative order, and
// the static "Repository files" table exactly. Extra rows are allowed — entries added since that
// file was last written are exactly what the generator is for. The header paragraph is
// deliberately different (D6: the file is generated now) and is not compared.
func TestGenerateMatchesCommittedIndex(t *testing.T) {
	root := repoRoot(t)
	refBytes, err := os.ReadFile(filepath.Join(root, Name))
	if err != nil {
		t.Fatalf("no reference %s to compare against: %v", Name, err)
	}
	ref := string(refBytes)

	entries, err := entry.Discover(root)
	if err != nil {
		t.Fatalf("discover %s: %v", root, err)
	}
	if len(entries) == 0 {
		t.Fatalf("discover %s returned no entries", root)
	}
	out, err := Generate(entries, "2026-09-13")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got := string(out)

	// Every reference row must appear byte-identical in the output, in the same relative order.
	want := tableRows(ref, "Knowledge entries")
	have := tableRows(got, "Knowledge entries")
	if len(want) == 0 {
		t.Fatalf("reference %s has no knowledge-entry rows", Name)
	}
	i := 0
	for _, row := range want {
		found := false
		for ; i < len(have); i++ {
			if have[i] == row {
				i++
				found = true
				break
			}
		}
		if !found {
			t.Errorf("row from %s missing (or out of order) in generated output:\n  %s\ngenerated rows:\n  %s",
				Name, row, strings.Join(have, "\n  "))
		}
	}

	if gotSec, wantSec := section(got, "Repository files"), section(ref, "Repository files"); gotSec != wantSec {
		t.Errorf("Repository files section differs.\n--- generated ---\n%s\n--- reference ---\n%s", gotSec, wantSec)
	}

	// Structural invariants of the whole file.
	if !strings.HasPrefix(got, "# Index\n\nEvery file and folder in this knowledge store, with what it holds.\n") {
		t.Errorf("generated file does not start with the expected header:\n%.120q", got)
	}
	if !strings.Contains(got, "\nLast reviewed: 2026-09-13\n") {
		t.Errorf("generated file has no Last reviewed line for the date passed in")
	}
	if !strings.HasSuffix(got, "|\n") || strings.HasSuffix(got, "|\n\n") {
		t.Errorf("generated file must end with exactly one trailing newline, got %q", tail(got, 40))
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func fileEntry(path, summary string) entry.Entry {
	return entry.Entry{
		Path:  path,
		Kind:  entry.KindFile,
		Files: []string{path},
		Meta:  entry.FrontMatter{Title: path, Summary: summary},
	}
}

func dirEntry(dir, summary string) entry.Entry {
	return entry.Entry{
		Path:  dir + "/README.md",
		Kind:  entry.KindDir,
		Dir:   dir,
		Files: []string{dir + "/README.md"},
		Meta:  entry.FrontMatter{Title: dir, Summary: summary},
	}
}

// TestGenerateRowFormats covers the two link shapes and the pipe escape.
func TestGenerateRowFormats(t *testing.T) {
	entries := []entry.Entry{
		fileEntry("foo.md", "A plain summary with `backticks`."),
		dirEntry("droplet", "Hardware and OS."),
		fileEntry("pipes.md", "Use a | b to fuse, not a || b."),
		fileEntry("wrapped.md", "First line\nsecond   line."),
	}
	out, err := Generate(entries, "2026-01-02")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got := string(out)
	for _, want := range []string{
		"| [foo.md](foo.md) | A plain summary with `backticks`. |\n",
		"| [droplet/](droplet/README.md) | Hardware and OS. |\n",
		`| [pipes.md](pipes.md) | Use a \| b to fuse, not a \|\| b. |` + "\n",
		"| [wrapped.md](wrapped.md) | First line second line. |\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("generated output is missing row:\n%q\ngot:\n%s", want, got)
		}
	}
}

// TestGenerateSortOrder pins the ordering rule: by displayed link text in byte order, so a
// directory sorts among the files rather than being grouped apart. "protobuf/" before
// "protoc-go-codegen.md" is the case that distinguishes this from sorting by anything else.
func TestGenerateSortOrder(t *testing.T) {
	entries := []entry.Entry{
		fileEntry("protoc-go-codegen.md", "c"),
		dirEntry("protobuf", "b"),
		fileEntry("grpc-reflection-and-grpcurl.md", "g"),
		dirEntry("droplet", "d"),
		fileEntry("bash-sourcing-and-exports.md", "a"),
	}
	out, err := Generate(entries, "2026-01-02")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var texts []string
	for _, row := range tableRows(string(out), "Knowledge entries") {
		texts = append(texts, row[strings.Index(row, "[")+1:strings.Index(row, "]")])
	}
	want := []string{
		"bash-sourcing-and-exports.md",
		"droplet/",
		"grpc-reflection-and-grpcurl.md",
		"protobuf/",
		"protoc-go-codegen.md",
	}
	if strings.Join(texts, ",") != strings.Join(want, ",") {
		t.Errorf("sort order:\n got %v\nwant %v", texts, want)
	}
}

// TestGenerateMissingSummary: a row is never silently blank.
func TestGenerateMissingSummary(t *testing.T) {
	e := entry.Entry{Path: "broken.md", Kind: entry.KindFile, Files: []string{"broken.md"}}
	out, err := Generate([]entry.Entry{e}, "2026-01-02")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(string(out), "| [broken.md](broken.md) | (no `summary` in front matter — fix broken.md) |") {
		t.Errorf("missing-summary row not flagged:\n%s", out)
	}
}

// TestGenerateRejectsBadInput: an unknown kind and an empty date are programming errors, not rows.
func TestGenerateRejectsBadInput(t *testing.T) {
	if _, err := Generate(nil, ""); err == nil {
		t.Error("Generate with an empty date should fail")
	}
	bad := entry.Entry{Path: "x.md", Kind: "sideways"}
	if _, err := Generate([]entry.Entry{bad}, "2026-01-02"); err == nil {
		t.Error("Generate with an unknown entry kind should fail")
	}
}

// TestWriteAndDiff exercises the on-disk side in a temp dir: Write lands the file, Diff says
// "same" straight afterwards, ignores a changed Last reviewed line, and notices a real edit.
func TestWriteAndDiff(t *testing.T) {
	dir := t.TempDir()
	entries := []entry.Entry{fileEntry("foo.md", "A summary."), dirEntry("droplet", "Hardware.")}

	if err := Write(dir, entries, "2026-01-02"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, Name))
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	want, err := Generate(entries, "2026-01-02")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if string(onDisk) != string(want) {
		t.Errorf("written file differs from Generate output")
	}
	if info, err := os.Stat(filepath.Join(dir, Name)); err != nil {
		t.Fatalf("stat: %v", err)
	} else if info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644", info.Mode().Perm())
	}
	// No temp files left behind.
	names, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(names) != 1 || names[0].Name() != Name {
		var got []string
		for _, n := range names {
			got = append(got, n.Name())
		}
		t.Errorf("temp files left behind: %v", got)
	}

	differs, err := Diff(dir, entries, "2026-01-02")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if differs {
		t.Error("Diff reports drift immediately after Write")
	}

	// A different date alone is not drift.
	differs, err = Diff(dir, entries, "2030-12-25")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if differs {
		t.Error("Diff should ignore the Last reviewed line")
	}

	// A changed summary is drift.
	differs, err = Diff(dir, []entry.Entry{fileEntry("foo.md", "Something else."), dirEntry("droplet", "Hardware.")}, "2026-01-02")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !differs {
		t.Error("Diff missed a changed summary")
	}

	// A hand edit is drift.
	if err := os.WriteFile(filepath.Join(dir, Name), append(want, []byte("hand edit\n")...), 0o644); err != nil {
		t.Fatalf("write hand edit: %v", err)
	}
	differs, err = Diff(dir, entries, "2026-01-02")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !differs {
		t.Error("Diff missed a hand edit")
	}
}

// TestDiffMissingFile: no index.md at all is drift, not an error.
func TestDiffMissingFile(t *testing.T) {
	differs, err := Diff(t.TempDir(), []entry.Entry{fileEntry("foo.md", "A summary.")}, "2026-01-02")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !differs {
		t.Error("a missing index.md should count as differing")
	}
}

// TestWriteOverwrites: Write replaces an existing file rather than appending to it.
func TestWriteOverwrites(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, Name), []byte("stale content\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	entries := []entry.Entry{fileEntry("foo.md", "A summary.")}
	if err := Write(dir, entries, "2026-01-02"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, Name))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(got), "stale content") {
		t.Errorf("Write did not replace the old file:\n%s", got)
	}
}

// TestToday is the shape of the helper, not the clock.
func TestToday(t *testing.T) {
	today := Today()
	if len(today) != 10 || today[4] != '-' || today[7] != '-' {
		t.Errorf("Today() = %q, want YYYY-MM-DD", today)
	}
}
