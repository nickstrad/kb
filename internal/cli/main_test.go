package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickstrad/kb/internal/embed/local"
	"github.com/nickstrad/kb/internal/workspace"
)

// world is the hermetic environment of one CLI test: its own deps, home, user cache and model
// cache (all under t.TempDir), and the working directory that root discovery sees. Nothing in it
// is shared between tests, so tests that need no process state (t.Setenv, a relative path the
// command resolves against the real cwd) call t.Parallel.
type world struct {
	d    *deps
	home string

	mu  sync.Mutex
	cwd string
}

var (
	worldsMu sync.Mutex
	worlds   = map[string]*world{}
)

// worldFor is the test's world, built on first use. A subtest shares its parent's world (found by
// walking up t.Name()), so a field the parent replaced is seen by the subtest's run calls.
func worldFor(t *testing.T) *world {
	t.Helper()
	worldsMu.Lock()
	defer worldsMu.Unlock()
	for name := t.Name(); ; {
		if w, ok := worlds[name]; ok {
			return w
		}
		i := strings.LastIndex(name, "/")
		if i < 0 {
			break
		}
		name = name[:i]
	}
	name := t.Name()
	w := newWorld(t)
	worlds[name] = w
	t.Cleanup(func() {
		worldsMu.Lock()
		delete(worlds, name)
		worldsMu.Unlock()
	})
	return w
}

func newWorld(t *testing.T) *world {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &world{d: productionDeps(), home: filepath.Join(tmp, "home"), cwd: filepath.Join(tmp, "cwd")}
	if err := os.MkdirAll(w.cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	d := w.d
	d.modelCacheDir = func() (string, error) { return filepath.Join(tmp, "models"), nil }
	d.pullModel = func(_ context.Context, spec local.Spec, _ string) error {
		return errors.New("download model " + spec.ID + ": downloads are disabled in tests")
	}
	d.embedCachePath = nil
	d.userCacheDir = func() (string, error) { return filepath.Join(tmp, "cache"), nil }
	d.workspaceEnv = func() workspace.Env {
		e := workspace.OSEnv()
		e.Cwd = w.workdir()
		e.Home, e.XDGDataHome, e.CacheBase = w.home, "", filepath.Join(tmp, "cache")
		return e
	}
	return w
}

func (w *world) workdir() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cwd
}

// depsFor returns the test's own deps (built on first use), off the user's model cache, the network and the
// user cache directory: the model cache is an empty temp directory, a download always fails, no
// shared embedding cache unless a test opts in, and incidents with no store go to a temp
// directory. Root discovery never sees the developer's home or XDG_DATA_HOME, and starts from an
// empty working directory (useRoot, chdir move it). A test that replaces a field changes only its
// own deps.
// run, runKB and runErr execute with it. (PLAN T1 calls this newTestDeps.)
func depsFor(t *testing.T) *deps { return worldFor(t).d }

// chdir sets the working directory root discovery sees for the test. It does not move the
// process, so the test can be parallel; a test that hands the command a relative path needs
// t.Chdir as well (and so cannot be parallel).
func chdir(t *testing.T, dir string) {
	t.Helper()
	w := worldFor(t)
	w.mu.Lock()
	w.cwd = dir
	w.mu.Unlock()
}
