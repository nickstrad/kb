package entry

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/glob"
)

// neverWalked are directory names a files source never descends into, at any depth: kb's own
// stores (.kb, also a nested location's), git metadata and npm's dependency tree.
var neverWalked = map[string]bool{".kb": true, ".git": true, "node_modules": true}

// filesSource walks one files source: every regular *.md file under the glob's base that the
// glob matches and no exclude pattern matches becomes a file entry. Symlinked directories are not
// followed; a symlinked file is read only when it resolves inside the root.
func (d *discovery) filesSource(idx int, src config.Source) error {
	base := glob.Base(src.Glob)
	start := d.root
	if base != "" {
		if out, why := d.outside(base); out {
			d.skip(base, why)
			return nil
		}
		// The trailing separator makes WalkDir follow a base that is itself a symlink (inside the
		// root, checked above), so "docs" -> "site/docs" is walked.
		start = filepath.Join(d.root, filepath.FromSlash(base)) + string(filepath.Separator)
	}
	err := filepath.WalkDir(start, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			if p == start && errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipAll // a missing base is an empty source
			}
			return err
		}
		r, relErr := filepath.Rel(d.root, p)
		if relErr != nil {
			return relErr
		}
		rel := filepath.ToSlash(r)
		if de.IsDir() {
			if rel != "." && (neverWalked[de.Name()] || !glob.CouldMatchUnder(src.Glob, rel)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(de.Name(), ".md") || !glob.Match(src.Glob, rel) || excluded(src.Exclude, rel) {
			return nil
		}
		if de.Type()&fs.ModeSymlink != 0 {
			if out, why := d.outside(rel); out {
				d.skip(rel, why)
				return nil
			}
			if info, err := os.Stat(p); err == nil && !info.Mode().IsRegular() {
				return nil // a link to a directory or a device: never an entry
			}
		} else if !de.Type().IsRegular() {
			return nil
		}
		e := Entry{Path: rel, Kind: KindFile, Files: []string{rel}, Layout: config.LayoutFiles, Source: idx}
		d.meta(&e)
		return d.add(e, src)
	})
	if err != nil {
		if errs.KindOf(err) == errs.Usage {
			return err // an overlap from add, already well-formed
		}
		return errs.Wrap(err, errs.Usage, "entry.Discover", "cannot read source %s: %v", src, err)
	}
	return nil
}

// excluded reports whether any exclude pattern matches the root-relative path rel.
func excluded(patterns []string, rel string) bool {
	for _, p := range patterns {
		if glob.Match(p, rel) {
			return true
		}
	}
	return false
}
