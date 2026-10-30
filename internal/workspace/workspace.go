// Package workspace finds the location and the named store a kb command works on. A location is a
// directory with a .kb/kb.toml file; it holds n stores, each a directory .kb/stores/<name>/ with
// its own config.toml and kb.sqlite (docs/design/named-stores.md).
//
// Inputs: the --root and --global flags, the working directory and the user's home, XDG_DATA_HOME
// and cache base (Env, so tests never touch process state; OSEnv builds the real one), and a store
// name. Outputs: a Location (ResolveLocation, TargetLocation) and a Workspace for one store
// (Location.Store). Everything is pure apart from stat and read-dir calls on the candidate paths.
//
// Invariants:
//   - Precedence for the location: --root DIR, else --global ($XDG_DATA_HOME/kb, default
//     ~/.local/share/kb), else the nearest directory at or above the working directory that holds
//     .kb/kb.toml as a regular file. --root and --global name a location even when it does not
//     exist yet; discovery never creates anything. There is no legacy marker.
//   - A store exists when stores/<name>/config.toml is a regular file. A valid name (ValidName)
//     makes the store directory a direct child of <KBDir>/stores, so a name can never escape it.
//   - Symlinks: the discovery walk starts from the working directory with symlinks resolved, so
//     "up" means the physical parent. --root and --global are made absolute and cleaned but not
//     resolved: the user named that path.
package workspace

import (
	"os"
	"path/filepath"

	"github.com/nickstrad/kb/internal/errs"
)

// Env is everything ResolveLocation needs from the process. Empty fields mean "unknown".
type Env struct {
	Cwd         string // working directory
	Home        string // user home directory
	XDGDataHome string // $XDG_DATA_HOME, "" when unset
	CacheBase   string // os.UserCacheDir(), "" when unavailable
}

// OSEnv reads the real process environment. It is the only place the package touches os state.
// Each lookup error is dropped on purpose: an empty field means "unknown", and ResolveLocation turns an
// unknown value it needs into a well-formed NotFound or Usage error.
func OSEnv() Env {
	var e Env
	e.Cwd, _ = os.Getwd()
	e.Home, _ = os.UserHomeDir()
	e.XDGDataHome = os.Getenv("XDG_DATA_HOME")
	e.CacheBase, _ = os.UserCacheDir()
	return e
}

// Workspace is one resolved store of a location.
type Workspace struct {
	Root       string // the location root, absolute and clean
	KBDir      string // <Root>/.kb
	DBPath     string // <StoreDir>/kb.sqlite
	ConfigPath string // <StoreDir>/config.toml
	CacheDir   string // <user cache>/kb, "" when there is no user cache directory
	ModelsDir  string // <CacheDir>/models, "" with CacheDir
	Location   *Location
	Store      string // the store's name
	StoreDir   string // <KBDir>/stores/<Store>
}

// File names inside .kb and a store directory.
const (
	configName = "config.toml"
	dbName     = "kb.sqlite"
	kbDirName  = ".kb"
)

func absFrom(p, cwd string) (string, error) {
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	if !filepath.IsAbs(cwd) {
		return "", errs.New(errs.Usage, "workspace", "cannot make --root %q absolute: no working directory", p)
	}
	return filepath.Join(cwd, p), nil
}

// ModelsDirOf is where embedding models are cached for a user cache base: <base>/kb/models. It is
// the one rule for that path (Workspace.ModelsDir and the model commands use it); "" with no base.
func ModelsDirOf(cacheBase string) string {
	if cacheBase == "" {
		return ""
	}
	return filepath.Join(cacheBase, "kb", "models")
}
