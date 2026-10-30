package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/errs"
)

// File and directory names of a location (docs/design/named-stores.md).
const (
	locationFileName = "kb.toml"
	storesDirName    = "stores"
	// GlobalName is the reserved store word that means "the global location's default store".
	GlobalName = "global"
)

// MaxNameLen is the longest store name ValidName accepts.
const MaxNameLen = 32

var nameRE = regexp.MustCompile(fmt.Sprintf(`^[a-z0-9][a-z0-9_-]{0,%d}$`, MaxNameLen-1))

// Location is a directory that holds a .kb/ folder with a kb.toml file; it holds n named stores.
type Location struct {
	Root     string // absolute, clean
	KBDir    string // <Root>/.kb
	FilePath string // <Root>/.kb/kb.toml

	cacheBase string // os.UserCacheDir(), "" when unavailable
}

// NewLocation builds the Location for an absolute root without touching the disk.
func NewLocation(root, cacheBase string) *Location {
	kb := filepath.Join(root, kbDirName)
	return &Location{Root: root, KBDir: kb, FilePath: filepath.Join(kb, locationFileName), cacheBase: cacheBase}
}

// StoresDir is <KBDir>/stores, the parent of every store directory.
func (l *Location) StoresDir() string { return filepath.Join(l.KBDir, storesDirName) }

// ValidName reports whether name is a legal store name: ^[a-z0-9][a-z0-9_-]{0,31}$, and not the
// reserved word "global". The error is a Usage error.
func ValidName(name string) error {
	switch {
	case name == GlobalName:
		return errs.New(errs.Usage, "workspace.ValidName", "store name %q is reserved: it means the global location", name)
	case !nameRE.MatchString(name):
		return errs.New(errs.Usage, "workspace.ValidName",
			"invalid store name %q: use 1-%d characters from a-z, 0-9, _ and -, starting with a letter or digit", name, MaxNameLen)
	}
	return nil
}

// ResolveLocation picks the location. rootFlag and global are mutually exclusive. --root and
// --global name a location even when it does not exist (kb init creates it); the commands that
// need an existing one read its kb.toml and fail with NotFound. Discovery walks up from the
// working directory for the nearest .kb/kb.toml (a regular file) and never creates anything.
func ResolveLocation(rootFlag string, global bool, env Env) (*Location, error) {
	root, err := locationRoot("workspace.ResolveLocation", rootFlag, global, env, func() (string, error) { return discoverLocation(env.Cwd) })
	if err != nil {
		return nil, err
	}
	return NewLocation(root, env.CacheBase), nil
}

// TargetLocation is ResolveLocation for the command that creates a location (kb init): with no
// flag, the target is the working directory itself (symlinks resolved), never an ancestor.
func TargetLocation(rootFlag string, global bool, env Env) (*Location, error) {
	root, err := locationRoot("workspace.TargetLocation", rootFlag, global, env, func() (string, error) {
		if !filepath.IsAbs(env.Cwd) {
			return "", errs.New(errs.Usage, "workspace.TargetLocation", "no working directory: use --root DIR")
		}
		dir := filepath.Clean(env.Cwd)
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			dir = real
		}
		return dir, nil
	})
	if err != nil {
		return nil, err
	}
	return NewLocation(root, env.CacheBase), nil
}

func locationRoot(op, rootFlag string, global bool, env Env, fallback func() (string, error)) (string, error) {
	if rootFlag != "" && global {
		return "", errs.New(errs.Usage, op, "--root and --global cannot be used together")
	}
	var root string
	switch {
	case rootFlag != "":
		abs, err := absFrom(rootFlag, env.Cwd)
		if err != nil {
			return "", err
		}
		root = abs
	case global:
		base := env.XDGDataHome
		if base == "" || !filepath.IsAbs(base) { // the XDG spec: a relative value is invalid, ignore it
			if env.Home == "" {
				return "", errs.New(errs.Usage, op, "cannot find the global location: no home directory and XDG_DATA_HOME is unset")
			}
			base = filepath.Join(env.Home, ".local", "share")
		}
		root = filepath.Join(base, "kb")
	default:
		found, err := fallback()
		if err != nil {
			return "", err
		}
		root = found
	}
	root = filepath.Clean(root)
	assert.Post(filepath.IsAbs(root) && root == filepath.Clean(root), "workspace: location root %q is not absolute and clean", root)
	return root, nil
}

// discoverLocation walks up from cwd to the filesystem root and returns the first directory that
// holds .kb/kb.toml as a regular file (a symlink to one counts). Any stat error means "not here".
func discoverLocation(cwd string) (string, error) {
	notFound := func() error {
		where := cwd
		if where == "" { // os.Getwd failed (the directory was deleted under us)
			where = "the working directory (unknown)"
		}
		return errs.New(errs.NotFound, "workspace.ResolveLocation",
			"no kb location found in %s or its parents; run \"kb init\" here, or use --global / --root", where)
	}
	if !filepath.IsAbs(cwd) {
		return "", notFound()
	}
	dir := filepath.Clean(cwd)
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	for {
		if fi, err := os.Stat(filepath.Join(dir, kbDirName, locationFileName)); err == nil && fi.Mode().IsRegular() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", notFound()
		}
		dir = parent
	}
}

// Store resolves the store called name: a directory <KBDir>/stores/<name> whose config.toml is a
// regular file. An invalid name is Usage; a store that does not exist is NotFound and names the
// stores that do.
func (l *Location) Store(name string) (*Workspace, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	w := l.workspace(name)
	if !isRegular(w.ConfigPath) {
		have, _, err := l.Stores()
		if err != nil {
			return nil, err
		}
		list := "none"
		if len(have) > 0 {
			list = strings.Join(have, ", ")
		}
		return nil, errs.New(errs.NotFound, "workspace.Store",
			"no store %q in %s (stores: %s); create it with kb init --name %s", name, l.Root, list, name)
	}
	return w, nil
}

// StoreAt builds the Workspace of the named store without checking that it exists (kb init
// creates it). The name must be valid.
func (l *Location) StoreAt(name string) (*Workspace, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	return l.workspace(name), nil
}

func (l *Location) workspace(name string) *Workspace {
	dir := filepath.Join(l.StoresDir(), name)
	assert.Post(filepath.Dir(dir) == l.StoresDir(), "workspace: store dir %q is not a direct child of %q", dir, l.StoresDir())
	w := &Workspace{
		Root:       l.Root,
		KBDir:      l.KBDir,
		DBPath:     filepath.Join(dir, dbName),
		ConfigPath: filepath.Join(dir, configName),
		Location:   l,
		Store:      name,
		StoreDir:   dir,
	}
	if l.cacheBase != "" {
		w.CacheDir = filepath.Join(l.cacheBase, "kb")
		w.ModelsDir = ModelsDirOf(l.cacheBase)
	}
	return w
}

// Stores lists the names of the stores in the location, sorted: the directories under
// <KBDir>/stores with a valid name whose config.toml is a regular file. Directories whose name is
// invalid are returned in skipped (sorted), so doctor can show them. A missing stores directory
// is no stores; any other read failure is a Usage error.
func (l *Location) Stores() (names, skipped []string, err error) {
	entries, err := os.ReadDir(l.StoresDir())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, errs.Wrap(err, errs.Usage, "workspace.Stores", "cannot read %s: %v", l.StoresDir(), err)
	}
	for _, e := range entries {
		// A symlink to a directory counts as a directory here; the config check follows links too.
		if !e.IsDir() && e.Type()&fs.ModeSymlink == 0 {
			continue
		}
		if ValidName(e.Name()) != nil {
			skipped = append(skipped, e.Name())
			continue
		}
		if isRegular(filepath.Join(l.StoresDir(), e.Name(), configName)) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	sort.Strings(skipped)
	return names, skipped, nil
}

// isRegular reports whether path is a regular file (a symlink to one counts).
func isRegular(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}
