package index

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/entry"
)

// repoRoot is the fixture knowledge repository the chunk tests also use, so this test does not
// depend on a real corpus next to the module.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "chunk", "testdata", "repo"))
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

// Generate from the fixture corpus without requiring a checked-in snapshot.
func TestGenerateFromCorpus(t *testing.T) {
	root := repoRoot(t)
	entries, err := entry.Discover(root)
	if err != nil || len(entries) == 0 {
		t.Fatalf("discover: %v (%d entries)", err, len(entries))
	}
	out, err := Generate(entries, "2026-09-13")
	if err != nil {
		t.Fatal(err)
	}
	rows := tableRows(string(out), "Knowledge entries")
	if len(rows) != len(entries) {
		t.Fatalf("got %d rows for %d entries", len(rows), len(entries))
	}
	for _, e := range entries {
		if !strings.Contains(string(out), "]("+e.Path+")") {
			t.Errorf("missing entry link: %s", e.Path)
		}
	}
	if !strings.Contains(string(out), "Last reviewed: 2026-09-13") {
		t.Error("missing supplied date")
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
