// Package workspace finds the kb store a command works on. Resolve is pure apart from stat calls
// on the candidate paths: the working directory, home, XDG_DATA_HOME and the user cache directory
// arrive in Env, so tests never touch process state. OSEnv builds the real Env.
//
// Precedence: --root DIR, else --global ($XDG_DATA_HOME/kb, default ~/.local/share/kb), else the
// nearest directory at or above the working directory that holds .kb/config.toml or the legacy
// .kb/kb.sqlite. --root and --global name a store even when it does not exist yet (the commands
// that create one, add and reindex, create .kb/ there); discovery never creates anything.
//
// Symlinks: the walk starts from the working directory with symlinks resolved, so "up" means the
// physical parent and a symlinked cwd inside a store finds that store. --root and --global are
// made absolute and cleaned but not resolved: the user named that path.
package workspace

import (
	"os"
	"path/filepath"

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/errs"
)

// Env is everything Resolve needs from the process. Empty fields mean "unknown".
type Env struct {
	Cwd         string // working directory
	Home        string // user home directory
	XDGDataHome string // $XDG_DATA_HOME, "" when unset
	CacheBase   string // os.UserCacheDir(), "" when unavailable
}

// OSEnv reads the real process environment. It is the only place the package touches os state.
// Each lookup error is dropped on purpose: an empty field means "unknown", and Resolve turns an
// unknown value it needs into a well-formed NotFound or Usage error.
func OSEnv() Env {
	var e Env
	e.Cwd, _ = os.Getwd()
	e.Home, _ = os.UserHomeDir()
	e.XDGDataHome = os.Getenv("XDG_DATA_HOME")
	e.CacheBase, _ = os.UserCacheDir()
	return e
}

// Workspace is one resolved store.
type Workspace struct {
	Root       string // absolute, clean
	KBDir      string // <Root>/.kb
	DBPath     string // <Root>/.kb/kb.sqlite
	ConfigPath string // <Root>/.kb/config.toml
	CacheDir   string // <user cache>/kb, "" when there is no user cache directory
	ModelsDir  string // <CacheDir>/models, "" with CacheDir
}

// File names inside .kb.
const (
	configName = "config.toml"
	dbName     = "kb.sqlite"
	kbDirName  = ".kb"
)

// New builds the Workspace for an absolute root.
func New(root, cacheBase string) *Workspace {
	kb := filepath.Join(root, kbDirName)
	w := &Workspace{
		Root:       root,
		KBDir:      kb,
		DBPath:     filepath.Join(kb, dbName),
		ConfigPath: filepath.Join(kb, configName),
	}
	if cacheBase != "" {
		w.CacheDir = filepath.Join(cacheBase, "kb")
		w.ModelsDir = filepath.Join(w.CacheDir, "models")
	}
	return w
}

// Resolve picks the workspace. rootFlag and global are mutually exclusive.
func Resolve(rootFlag string, global bool, env Env) (*Workspace, error) {
	if rootFlag != "" && global {
		return nil, errs.New(errs.Usage, "workspace.Resolve", "--root and --global cannot be used together")
	}
	var root string
	switch {
	case rootFlag != "":
		abs, err := absFrom(rootFlag, env.Cwd)
		if err != nil {
			return nil, err
		}
		root = abs
	case global:
		base := env.XDGDataHome
		if base == "" || !filepath.IsAbs(base) { // the XDG spec: a relative value is invalid, ignore it
			if env.Home == "" {
				return nil, errs.New(errs.Usage, "workspace.Resolve", "cannot find the global store: no home directory and XDG_DATA_HOME is unset")
			}
			base = filepath.Join(env.Home, ".local", "share")
		}
		root = filepath.Join(base, "kb")
	default:
		found, err := discover(env.Cwd)
		if err != nil {
			return nil, err
		}
		root = found
	}
	root = filepath.Clean(root)
	assert.Post(filepath.IsAbs(root) && root == filepath.Clean(root), "workspace: root %q is not absolute and clean", root)
	return New(root, env.CacheBase), nil
}

func absFrom(p, cwd string) (string, error) {
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	if !filepath.IsAbs(cwd) {
		return "", errs.New(errs.Usage, "workspace.Resolve", "cannot make --root %q absolute: no working directory", p)
	}
	return filepath.Join(cwd, p), nil
}

// discover walks up from cwd to the filesystem root and returns the first directory that is a
// store. A stat error of any kind (missing, unreadable, not a directory) means "not a store here".
func discover(cwd string) (string, error) {
	notFound := func() error {
		where := cwd
		if where == "" { // os.Getwd failed (the directory was deleted under us)
			where = "the working directory (unknown)"
		}
		return errs.New(errs.NotFound, "workspace.Resolve",
			"no kb store found in %s or its parents; run \"kb init\" here, or use --global / --root", where)
	}
	if !filepath.IsAbs(cwd) {
		return "", notFound()
	}
	dir := filepath.Clean(cwd)
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	for {
		if isStore(dir) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", notFound()
		}
		dir = parent
	}
}

// isStore reports whether dir holds .kb/config.toml or the legacy .kb/kb.sqlite as a regular
// file (a symlink to one counts; a directory of that name does not).
func isStore(dir string) bool {
	for _, name := range []string{configName, dbName} {
		if fi, err := os.Stat(filepath.Join(dir, kbDirName, name)); err == nil && fi.Mode().IsRegular() {
			return true
		}
	}
	return false
}
