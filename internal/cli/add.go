// `kb add`: validate front matter, place the file in data/, then chunk and embed it.
//
// Both forms (`kb add <file.md>` and `kb add --dir <topic>/`) follow the same shape:
//  1. validate the name (lowercase kebab-case);
//  2. read and validate front matter, and reject anything that looks like a secret, before
//     touching the repository or the database at all;
//  3. work out where the entry belongs (copy in from outside the repo, or index in place when it
//     is already directly under data/);
//  4. open the database, refuse a path that is already indexed, then copy (if needed) and index.
//
// A directory entry's README.md carries the entry's own front matter and is validated like a file
// entry's front door; its docs/*.md files may or may not have front matter of their own (only a
// structurally broken "---" fence is an error there, per entry.ParseFrontMatter's own doc), and
// every file under the directory — README.md, docs/*.md, scripts/*, anything else — is
// secrets-scanned before anything is copied.
package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/entry"
	"github.com/nickstrad/kb/internal/reindex"
	"github.com/nickstrad/kb/internal/store"
)

// fileNameRe and dirNameRe are the naming rule for a new entry: lowercase kebab-case.
var (
	fileNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*\.md$`)
	dirNameRe  = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
)

// reservedFileNames are the *.md basenames at the repo root that are never entries (AGENTS.md,
// CLAUDE.md.txt and README.md are already excluded by fileNameRe's lowercase rule; index.md and
// plan.md are the two that would otherwise pass it), mirroring internal/entry's excludedRootFiles.
var reservedFileNames = map[string]bool{
	"agents.md": true,
	"claude.md": true,
	"index.md":  true,
	"plan.md":   true,
	"readme.md": true,
}

// reservedDirNames are the directory names that are never a directory entry even with a
// README.md, mirroring internal/entry's excludedDirs (".kb" and ".git" cannot pass dirNameRe
// anyway, since it disallows a leading dot; "skill" can, so it needs the explicit check).
var reservedDirNames = map[string]bool{
	"skill": true,
	".kb":   true,
	".git":  true,
}

// newAddCmd builds `kb add <file.md>` and `kb add --dir <topic>/`.
func newAddCmd(d *deps, stdout, stderr io.Writer) *cobra.Command {
	var asDir bool
	cmd := &cobra.Command{
		Use:   "add <file.md>",
		Short: "Add an entry to the repository and index it",
		Long: "Adds a Markdown file (or, with --dir, a directory entry whose README.md and\n" +
			"docs/*.md are indexed). A source outside data/ is\n" +
			"copied into data/; a source already directly inside it is indexed in place.",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if asDir {
				return runAddDir(cmd, d, stdout, stderr, args[0])
			}
			return runAddFile(cmd, d, stdout, stderr, args[0])
		},
	}
	cmd.Flags().BoolVar(&asDir, "dir", false, "the argument is a directory entry, not a single file")
	return cmd
}

// runAddFile implements `kb add <file.md>`.
func runAddFile(cmd *cobra.Command, d *deps, stdout, stderr io.Writer, srcArg string) (retErr error) {
	ws, cfg, err := d.workspaceConfig(cmd)
	if err != nil {
		return err
	}
	root := ws.Root
	basename := filepath.Base(filepath.Clean(srcArg))

	if !fileNameRe.MatchString(basename) {
		return usageErr("%q must be lowercase kebab-case matching ^[a-z0-9]+(-[a-z0-9]+)*\\.md$", basename)
	}
	if reservedFileNames[basename] {
		return usageErr("%q is a reserved repository file, not a knowledge entry", basename)
	}

	src, err := os.ReadFile(srcArg)
	if err != nil {
		return usageErr("read %s: %s", srcArg, err)
	}

	fm, _, err := entry.ParseFrontMatter(src)
	if err != nil {
		var ve *entry.ValidationError
		if errors.As(err, &ve) {
			return usageErr("%s: %s", srcArg, ve.Reason)
		}
		return usageErr("%s: %s", srcArg, err)
	}
	if strings.TrimSpace(fm.Title) == "" || strings.TrimSpace(fm.Summary) == "" {
		return usageErr("%s: front matter with title and summary is required", srcArg)
	}
	warnMissingMeta(stderr, basename, fm)

	if hits := scanSecrets(src); len(hits) > 0 {
		return secretsErr(srcArg, hits)
	}

	relPath, destPath, copyNeeded, err := placeFile(root, srcArg, basename)
	if err != nil {
		return err
	}

	// Like `kb edit`, `kb add` cannot recover from an embed_meta mismatch (all=false below).
	st, err := d.openStoreForWrite(cmd.Context(), ws)
	if err != nil {
		return err
	}
	defer st.Close()

	embedder, err := d.newEmbedder(cmd, cfg.Embed)
	if err != nil {
		return err
	}
	force, err := ensureEmbedMetaForReindex(cmd.Context(), st, embedder, false, stderr)
	if err != nil {
		return err
	}

	// Check the model before anything is written, so an unusable embedder fails fast with nothing
	// to roll back (exit 2, same as the reindex path).
	if p, ok := embedder.(pinger); ok {
		if err := p.Ping(cmd.Context()); err != nil {
			return embedderErr("embedder not usable, nothing copied: %v", err)
		}
	}

	if err := checkAddDestination(cmd.Context(), st, relPath, destPath, copyNeeded); err != nil {
		return err
	}
	if copyNeeded {
		if err := os.MkdirAll(entry.DataRoot(root), 0o755); err != nil {
			return usageErr("create data directory: %s", err)
		}
		// Claim the destination exclusively before arming cleanup; never remove another writer's files.
		f, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return usageErr("%s", err)
		}
		defer func() {
			if retErr != nil {
				rollbackAdded(cmd.Context(), st, relPath, destPath, stderr)
			}
		}()
		if err := f.Close(); err != nil {
			return usageErr("%s", err)
		}
		if err := writeFileBytes(src, destPath); err != nil {
			return usageErr("%s", err)
		}
	}

	e, err := entry.Resolve(root, relPath)
	if err != nil {
		return usageErr("%s", err)
	}
	if e.Err != nil {
		return usageErr("%s", e.Err)
	}

	sum, err := reindex.One(cmd.Context(), reindex.Options{
		Root:      root,
		Store:     st,
		Embedder:  embedder,
		BatchSize: cfg.Embed.Batch,
		Stdout:    io.Discard,
		Stderr:    stderr,
		Force:     force,
	}, e)
	if err != nil {
		return err
	}

	if sum.Failed > 0 {
		return usageErr("%s: indexing failed; see warnings above", relPath)
	}

	fmt.Fprintf(stdout, "added %s (%d chunks)\n", relPath, sum.Chunks)
	return nil
}

// runAddDir implements `kb add --dir <topic>/`.
func runAddDir(cmd *cobra.Command, d *deps, stdout, stderr io.Writer, argPath string) (retErr error) {
	ws, cfg, err := d.workspaceConfig(cmd)
	if err != nil {
		return err
	}
	root := ws.Root
	srcDir := filepath.Clean(argPath)
	name := filepath.Base(srcDir)

	if !dirNameRe.MatchString(name) {
		return usageErr("%q must be lowercase kebab-case matching ^[a-z0-9]+(-[a-z0-9]+)*$", name)
	}
	if reservedDirNames[name] {
		return usageErr("%q is a reserved directory name, not a knowledge entry", name)
	}

	info, err := os.Stat(srcDir)
	if err != nil {
		return usageErr("%s", err)
	}
	if !info.IsDir() {
		return usageErr("%s is not a directory", srcDir)
	}

	readmePath := filepath.Join(srcDir, "README.md")
	readmeData, err := os.ReadFile(readmePath)
	if err != nil {
		return usageErr("%s: missing or unreadable README.md: %s", srcDir, err)
	}
	fm, _, err := entry.ParseFrontMatter(readmeData)
	if err != nil {
		var ve *entry.ValidationError
		if errors.As(err, &ve) {
			return usageErr("%s: %s", readmePath, ve.Reason)
		}
		return usageErr("%s: %s", readmePath, err)
	}
	if strings.TrimSpace(fm.Title) == "" || strings.TrimSpace(fm.Summary) == "" {
		return usageErr("%s: front matter with title and summary is required", readmePath)
	}
	warnMissingMeta(stderr, name+"/README.md", fm)

	files, names, err := collectDirFiles(srcDir)
	if err != nil {
		return usageErr("%s", err)
	}

	// docs/*.md files may carry no front matter at all; only a structurally broken "---" fence
	// (not a *ValidationError, which just means no title/summary) is an error here, per
	// entry.ParseFrontMatter's documented "tolerate this and keep going" contract for non-front-
	// door files.
	for _, n := range names {
		if !isTopLevelDocsFile(n) {
			continue
		}
		if _, _, err := entry.ParseFrontMatter(files[n].data); err != nil {
			var ve *entry.ValidationError
			if !errors.As(err, &ve) {
				return usageErr("%s/%s: %s", name, n, err)
			}
		}
	}

	for _, n := range names {
		if hits := scanSecrets(files[n].data); len(hits) > 0 {
			return secretsErr(filepath.Join(srcDir, filepath.FromSlash(n)), hits)
		}
	}

	relPath, destDir, copyNeeded, err := placeDir(root, srcDir, name)
	if err != nil {
		return err
	}

	st, err := d.openStoreForWrite(cmd.Context(), ws)
	if err != nil {
		return err
	}
	defer st.Close()

	embedder, err := d.newEmbedder(cmd, cfg.Embed)
	if err != nil {
		return err
	}
	force, err := ensureEmbedMetaForReindex(cmd.Context(), st, embedder, false, stderr)
	if err != nil {
		return err
	}

	// Check the model before anything is written, so an unusable embedder fails fast with nothing
	// to roll back (exit 2, same as the reindex path).
	if p, ok := embedder.(pinger); ok {
		if err := p.Ping(cmd.Context()); err != nil {
			return embedderErr("embedder not usable, nothing copied: %v", err)
		}
	}

	if err := checkAddDestination(cmd.Context(), st, relPath, destDir, copyNeeded); err != nil {
		return err
	}
	if copyNeeded {
		if err := os.MkdirAll(entry.DataRoot(root), 0o755); err != nil {
			return usageErr("create data directory: %s", err)
		}
		// Claim the destination exclusively before arming cleanup; never remove another writer's files.
		if err := os.Mkdir(destDir, 0o755); err != nil {
			return usageErr("%s", err)
		}
		defer func() {
			if retErr != nil {
				rollbackAdded(cmd.Context(), st, relPath, destDir, stderr)
			}
		}()
		if err := copyTree(files, destDir); err != nil {
			return usageErr("%s", err)
		}
	}

	e, err := entry.Resolve(root, name)
	if err != nil {
		return usageErr("%s", err)
	}
	if e.Err != nil {
		return usageErr("%s", e.Err)
	}

	sum, err := reindex.One(cmd.Context(), reindex.Options{
		Root:      root,
		Store:     st,
		Embedder:  embedder,
		BatchSize: cfg.Embed.Batch,
		Stdout:    io.Discard,
		Stderr:    stderr,
		Force:     force,
	}, e)
	if err != nil {
		return err
	}

	if sum.Failed > 0 {
		return usageErr("%s: indexing failed; see warnings above", relPath)
	}

	fmt.Fprintf(stdout, "added %s (%d chunks)\n", relPath, sum.Chunks)
	return nil
}

// warnMissingMeta prints the two non-fatal front-matter warnings this item's spec asks for: no
// tags, or no updated date. Only title and summary are required to proceed.
func warnMissingMeta(stderr io.Writer, path string, fm entry.FrontMatter) {
	if len(fm.Tags) == 0 {
		fmt.Fprintf(stderr, "warning: %s: front matter has no tags\n", path)
	}
	if strings.TrimSpace(fm.Updated) == "" {
		fmt.Fprintf(stderr, "warning: %s: front matter has no 'updated' date\n", path)
	}
}

// placeFile decides where a file entry ends up. relPath is the repo-relative path to index
// ("data/basename.md"); destPath is where the bytes must live on disk; copyNeeded says whether the
// caller still has to write them there (false when srcPath already IS destPath, i.e. the source
// was already directly under data/).
func placeFile(root, srcPath, basename string) (relPath, destPath string, copyNeeded bool, err error) {
	absRoot, err := filepath.Abs(entry.DataRoot(root))
	if err != nil {
		return "", "", false, usageErr("%s", err)
	}
	absRoot = filepath.Clean(absRoot)
	absSrc, err := filepath.Abs(srcPath)
	if err != nil {
		return "", "", false, usageErr("%s", err)
	}
	absSrc = filepath.Clean(absSrc)

	rel, relErr := filepath.Rel(absRoot, absSrc)
	if !isInside(rel, relErr) {
		dest := filepath.Join(absRoot, basename)
		return entry.DataDir + "/" + basename, dest, true, nil
	}

	rel = filepath.ToSlash(rel)
	if filepath.ToSlash(filepath.Dir(rel)) != "." {
		return "", "", false, usageErr("%s: must be directly under data/, not a subdirectory", rel)
	}
	return entry.DataDir + "/" + rel, absSrc, false, nil
}

// placeDir is placeFile's counterpart for `kb add --dir`: relPath is always "data/<name>/README.md".
func placeDir(root, srcDir, name string) (relPath, destDir string, copyNeeded bool, err error) {
	absRoot, err := filepath.Abs(entry.DataRoot(root))
	if err != nil {
		return "", "", false, usageErr("%s", err)
	}
	absRoot = filepath.Clean(absRoot)
	absSrc, err := filepath.Abs(srcDir)
	if err != nil {
		return "", "", false, usageErr("%s", err)
	}
	absSrc = filepath.Clean(absSrc)

	rel, relErr := filepath.Rel(absRoot, absSrc)
	if !isInside(rel, relErr) {
		dest := filepath.Join(absRoot, name)
		return entry.DataDir + "/" + name + "/README.md", dest, true, nil
	}

	rel = filepath.ToSlash(rel)
	if rel != name {
		return "", "", false, usageErr("%s: must be directly under data/, not a subdirectory", rel)
	}
	return entry.DataDir + "/" + name + "/README.md", absSrc, false, nil
}

// isInside reports whether rel (the result of filepath.Rel(root, src)) names a path inside root.
func isInside(rel string, relErr error) bool {
	if relErr != nil || rel == ".." {
		return false
	}
	rel = filepath.ToSlash(rel)
	return !strings.HasPrefix(rel, "../") && !filepath.IsAbs(rel)
}

// collectDirFiles reads every regular file under srcDir into memory, keyed by its slash-separated
// path relative to srcDir, and returns those keys sorted. Used both to secrets-scan the whole tree
// and, when the source is outside the repo, to copy it in one pass.
func collectDirFiles(srcDir string) (files map[string]addFile, names []string, err error) {
	files = make(map[string]addFile)
	walkErr := filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != srcDir && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		if bytes.IndexByte(data[:min(len(data), 8192)], 0) >= 0 {
			return fmt.Errorf("%s: non-text file (NUL in first 8 KB); directory entries may contain text files only", p)
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return fmt.Errorf("relativize %s: %w", p, err)
		}
		files[filepath.ToSlash(rel)] = addFile{data: data, mode: 0o644 | (info.Mode().Perm() & 0o111)}
		return nil
	})
	if walkErr != nil {
		return nil, nil, walkErr
	}
	names = make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	return files, names, nil
}

// isTopLevelDocsFile reports whether rel (a slash-separated path relative to a directory entry's
// root) is one of that entry's docs/*.md files, per "Which files are entries" (docs/*.md, not
// docs/**/*.md).
func isTopLevelDocsFile(rel string) bool {
	return strings.HasSuffix(rel, ".md") && filepath.ToSlash(filepath.Dir(rel)) == "docs"
}

// writeFileBytes writes data to dest, creating its parent directory if needed.
func writeFileBytes(data []byte, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(dest), err)
	}
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", dest, err)
	}
	return nil
}

// copyTree writes every file in files (keyed by slash-separated path relative to the entry root)
// under destDir.
func copyTree(files map[string]addFile, destDir string) error {
	for rel, data := range files {
		if err := writeFileBytes(data.data, filepath.Join(destDir, filepath.FromSlash(rel))); err != nil {
			return err
		}
		if err := os.Chmod(filepath.Join(destDir, filepath.FromSlash(rel)), data.mode); err != nil {
			return err
		}
	}
	return nil
}

// secretPattern is one named regular expression from the secrets scan.
type secretPattern struct {
	name string
	re   *regexp.Regexp
}

// secretPatterns is the fixed list of secret-shaped text this item's spec refuses to let into the
// repository, each compiled case-insensitively.
var secretPatterns = compileSecretPatterns([]struct{ name, pattern string }{
	{"password", `password\s*=`},
	{"passwd", `passwd\s*=`},
	{"token", `token\s*=`},
	{"secret", `secret\s*=`},
	{"api-key", `api[_-]?key\s*=`},
	{"private-key-header", `BEGIN [A-Z ]*PRIVATE KEY`},
	{"aws-access-key-id", `AKIA[0-9A-Z]{16}`},
	{"slack-token", `xox[bpsa]-[A-Za-z0-9-]{10,}`},
	{"github-token", `ghp_[A-Za-z0-9]{20,}`},
})

func compileSecretPatterns(specs []struct{ name, pattern string }) []secretPattern {
	out := make([]secretPattern, len(specs))
	for i, s := range specs {
		out[i] = secretPattern{name: s.name, re: regexp.MustCompile("(?i)" + s.pattern)}
	}
	return out
}

// secretHit is one line that matched one secret pattern.
type secretHit struct {
	line int
	name string
}

// scanSecrets checks content against every secretPattern, line by line so the failure can name
// exactly where the match was.
func scanSecrets(content []byte) []secretHit {
	var hits []secretHit
	for i, line := range strings.Split(string(content), "\n") {
		for _, p := range secretPatterns {
			if p.re.MatchString(line) {
				hits = append(hits, secretHit{line: i + 1, name: p.name})
			}
		}
	}
	return hits
}

// secretsErr renders a secrets-scan failure: exit 1, listing every hit's line number and pattern
// name so the reason is actionable without opening the file in an editor first.
func secretsErr(path string, hits []secretHit) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s must not contain text that looks like a secret:\n", path) // report adds "kb add: "
	for _, h := range hits {
		fmt.Fprintf(&b, "  line %d: matches pattern %q\n", h.line, h.name)
	}
	b.WriteString("reword it, or place the file by hand and run kb reindex <entry>")
	return usageErr("%s", b.String())
}

// addFile captures bytes and executable permissions in the same pre-copy scan.
type addFile struct {
	data []byte
	mode fs.FileMode
}

func checkAddDestination(ctx context.Context, st store.Index, rel, dest string, copying bool) error {
	row, err := st.Entry(ctx, rel)
	if err != nil {
		return err
	}
	_, statErr := os.Lstat(dest)
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return usageErr("%s", statErr)
	}
	if row != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return usageErr("%s: indexed but missing on disk; run kb reindex --all", rel)
		}
		return usageErr("%s: already indexed; use kb edit %s", rel, rel)
	}
	if copying && statErr == nil {
		return usageErr("%s: exists but is not indexed; use kb reindex %s", rel, rel)
	}
	return nil
}

// rollbackAdded removes only paths this invocation created; in-place input is always preserved.
// The index cleanup runs on a context that ignores cancellation: a cancelled ctx is the usual
// reason the command failed, and the cleanup must still happen.
func rollbackAdded(ctx context.Context, st store.Index, rel, dest string, stderr io.Writer) {
	if err := os.RemoveAll(dest); err != nil {
		fmt.Fprintf(stderr, "kb add: cleanup %s failed: %s\n", dest, err)
		return
	}
	fmt.Fprintf(stderr, "kb add: removed copied %s after failure\n", rel)
	if err := st.DeleteEntry(context.WithoutCancel(ctx), rel); err != nil {
		fmt.Fprintf(stderr, "kb add: cleanup index failed: %s; run kb reindex --all\n", err)
	}
}
