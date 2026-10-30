// Store refs on the command line: --store REF resolved against the current location, its aliases
// and the global location's (workspace.Resolver), and the read-only open other stores get.
package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/store/sqlite"
	"github.com/nickstrad/kb/internal/workspace"
)

// readLocationInfo is workspace.Resolver.Read: the part of kb.toml that ref resolution needs.
func readLocationInfo(l *workspace.Location) (workspace.LocationInfo, error) {
	lf, err := config.ReadLocation(l.FilePath)
	if err != nil {
		return workspace.LocationInfo{}, err
	}
	return locationInfo(lf), nil
}

func locationInfo(lf config.Location) workspace.LocationInfo {
	return workspace.LocationInfo{DefaultStore: lf.DefaultStore, Aliases: lf.Aliases}
}

// resolver builds the ref resolver for cmd; its warnings go to the command's stderr.
func (d *deps) resolver(cmd *cobra.Command) workspace.Resolver {
	return workspace.Resolver{
		Env:  d.workspaceEnv(),
		Read: readLocationInfo,
		Warn: func(msg string) { fmt.Fprintln(cmd.ErrOrStderr(), msg) },
	}
}

// refResolution is what resolveRef found.
type refResolution struct {
	loc *workspace.Location
	lf  config.Location
	ref workspace.Ref
	how string // for --debug
	ws  *workspace.Workspace
}

// resolveRef applies --root, --global and --store. It is the one place the grammar meets the
// flags:
//   - global, NAME@LOC and PATH name their own location. NAME@LOC and PATH with --root or --global
//     are Usage errors, and so is global with --root (--global is the same thing).
//   - a bare NAME is a store of the current location (--root, --global, or discovery), else an alias
//     of that location or of the global one. With --root or --global an alias is Usage: it names its
//     own location.
//   - no --store is the location's default_store.
//
// With needStore false (kb stores, kb doctor) only the location is resolved, so an unknown store
// does not hide the others; ws is nil then, except for an alias, whose location is its target's.
func (d *deps) resolveRef(cmd *cobra.Command, needStore bool) (rr refResolution, err error) {
	// Get* fails only for an undefined flag; all are root persistent flags, so "" / false is safe.
	root, _ := cmd.Flags().GetString("root")
	global, _ := cmd.Flags().GetBool("global")
	storeFlag, _ := cmd.Flags().GetString("store")
	env := d.workspaceEnv()
	rr.ref, err = workspace.ParseRef(storeFlag)
	if err != nil {
		return refResolution{}, err
	}
	ref := rr.ref
	explicit := root != "" || global
	rr.how = "discovered from " + env.Cwd
	r := d.resolver(cmd)
	switch {
	case ref.Global && root != "":
		return refResolution{}, usageErr("--store global cannot be combined with --root: global names the global location")
	case (ref.At != "" || ref.Path != "") && explicit:
		flag := "--root"
		if global {
			flag = "--global"
		}
		return refResolution{}, usageErr("--store %s names its own location, so it cannot be combined with %s", ref, flag)
	case ref.At != "" || ref.Path != "":
		return d.resolveOwnLocation(r, rr, needStore)
	case ref.Global:
		global, rr.how = true, "--store global"
	case root != "":
		rr.how = "--root"
	case global:
		rr.how = "--global"
	}
	cur, lf, curErr := d.currentLocation(root, global, env)
	if ref.Name == "" { // no --store, or global: the location's default_store
		if curErr != nil {
			return refResolution{}, curErr
		}
		rr.loc, rr.lf = cur, lf
		if needStore {
			if rr.ws, err = defaultStoreOf(cur, lf); err != nil {
				return refResolution{}, err
			}
		}
		return rr, nil
	}

	// A bare NAME: a store of the current location, else an alias.
	r.LocalOnly = explicit
	var curArg *workspace.Location
	var info workspace.LocationInfo
	if curErr == nil {
		curArg, info = cur, locationInfo(lf)
	}
	_, localErr := workspaceStore(cur, ref.Name, curErr)
	if explicit && localErr != nil && curErr == nil && r.HasAlias(ref.Name, cur, info) {
		return refResolution{}, usageErr("--store %s is an alias, which names its own location, so it cannot be combined with --root or --global", ref.Name)
	}
	if !needStore && curErr == nil {
		rr.loc, rr.lf = cur, lf
		if localErr != nil && r.HasAlias(ref.Name, cur, info) { // an alias: the command's location is its target's
			if res, rerr := r.Resolve(ref, curArg, info); rerr == nil {
				return d.fromResolved(rr, res)
			}
		}
		return rr, nil
	}
	res, rerr := r.Resolve(ref, curArg, info)
	if rerr != nil {
		if curErr != nil && errors.Is(rerr, workspace.ErrUnknownRef) {
			return refResolution{}, curErr // no location, no such global alias: say so
		}
		return refResolution{}, rerr
	}
	return d.fromResolved(rr, res)
}

// workspaceStore is cur.Store(name), or curErr when there is no current location.
func workspaceStore(cur *workspace.Location, name string, curErr error) (*workspace.Workspace, error) {
	if curErr != nil {
		return nil, curErr
	}
	return cur.Store(name)
}

// fromResolved fills rr from a store Resolve found.
func (d *deps) fromResolved(rr refResolution, res workspace.Resolved) (refResolution, error) {
	lf, err := config.ReadLocation(res.Workspace.Location.FilePath)
	if err != nil {
		return refResolution{}, err
	}
	rr.loc, rr.lf, rr.ws = res.Workspace.Location, lf, res.Workspace
	if res.Via != "store" {
		rr.how = fmt.Sprintf("--store %s: %s", rr.ref, res.Via)
	}
	return rr, nil
}

// resolveOwnLocation handles NAME@LOC and PATH.
func (d *deps) resolveOwnLocation(r workspace.Resolver, rr refResolution, needStore bool) (refResolution, error) {
	loc, info, err := r.Location(rr.ref)
	if err != nil {
		return refResolution{}, err
	}
	lf, err := config.ReadLocation(loc.FilePath)
	if err != nil {
		return refResolution{}, err
	}
	rr.loc, rr.lf, rr.how = loc, lf, "--store "+rr.ref.String()
	if needStore {
		res, err := r.Resolve(rr.ref, nil, info)
		if err != nil {
			return refResolution{}, err
		}
		rr.ws = res.Workspace
	}
	return rr, nil
}

// currentLocation resolves the location from the flags or discovery and reads its kb.toml; err
// says why there is none.
func (d *deps) currentLocation(root string, global bool, env workspace.Env) (*workspace.Location, config.Location, error) {
	loc, err := workspace.ResolveLocation(root, global, env)
	if err != nil {
		return nil, config.Location{}, err
	}
	lf, err := config.ReadLocation(loc.FilePath)
	if err != nil {
		return nil, config.Location{}, err
	}
	return loc, lf, nil
}

// defaultStoreOf is the location's default_store; one that names no store is NotFound naming kb.toml.
func defaultStoreOf(loc *workspace.Location, lf config.Location) (*workspace.Workspace, error) {
	ws, err := loc.Store(lf.DefaultStore)
	if err != nil {
		if errs.KindOf(err) == errs.NotFound {
			return nil, errs.Wrap(err, errs.NotFound, "cli.workspace",
				"default_store %q in %s names no store in %s; %s", lf.DefaultStore, loc.FilePath, loc.Root, storeHint(loc, lf.DefaultStore))
		}
		return nil, err
	}
	return ws, nil
}

// openedStore is another store opened read-only, with the embedder its index records.
type openedStore struct {
	Workspace *workspace.Workspace
	Canonical string // NAME@ABSROOT
	Store     store.Store
	Meta      store.EmbedMeta
	Indexed   bool // Meta is the recorded embedder; false for a database never indexed
}

// openStoreReadOnly resolves ref and opens that store's database read-only, for the stores a
// search fans out to. A ref that does not parse is a Usage error. A store that cannot be used (the
// ref names nothing, the database is missing, or its schema is older or newer than this kb) is a
// warning on stderr and a nil result with a nil error: the caller skips it. The caller closes the
// store.
func (d *deps) openStoreReadOnly(cmd *cobra.Command, ref string) (*openedStore, error) {
	parsed, err := workspace.ParseRef(ref)
	if err != nil {
		return nil, err
	}
	stderr := cmd.ErrOrStderr()
	skip := func(format string, args ...any) (*openedStore, error) {
		fmt.Fprintf(stderr, "warning: skipping store %s: %s\n", ref, fmt.Sprintf(format, args...))
		return nil, nil
	}
	rr, err := d.resolveRefIn(cmd, parsed)
	if err != nil {
		if errs.KindOf(err) == errs.Usage {
			return nil, err
		}
		return skip("%v", err)
	}
	ws := rr.ws
	canonical := workspace.Canonical(ws.Store, ws.Root)
	ctx := cmd.Context()
	if !fileExists(ws.DBPath) {
		return skip("%s has no database yet (not indexed)", canonical)
	}
	st, err := d.openStore(ctx, ws.DBPath, true)
	if err != nil {
		if errs.KindOf(err) == errs.Conflict {
			msg := err.Error()
			if errors.Is(err, sqlite.ErrSchemaOlder) { // the fix runs in that store
				msg += fmt.Sprintf(" (for that store: kb --store %s migrate up)", canonical)
			}
			return skip("%s", msg)
		}
		return nil, err
	}
	meta, ok, err := st.EmbedMeta(ctx)
	if err != nil {
		st.Close()
		return nil, err
	}
	return &openedStore{Workspace: ws, Canonical: canonical, Store: st, Meta: meta, Indexed: ok}, nil
}

// resolveRefIn resolves a store ref of a command that has no --store of its own (a fan-out ref):
// bare names are stores or aliases of the current location, which is never forced by --root or
// --global here beyond choosing where "current" is.
func (d *deps) resolveRefIn(cmd *cobra.Command, ref workspace.Ref) (refResolution, error) {
	root, _ := cmd.Flags().GetString("root")
	global, _ := cmd.Flags().GetBool("global")
	env := d.workspaceEnv()
	r := d.resolver(cmd)
	cur, lf, curErr := d.currentLocation(root, global, env)
	var curArg *workspace.Location
	var info workspace.LocationInfo
	if curErr == nil {
		curArg, info = cur, locationInfo(lf)
	}
	res, err := r.Resolve(ref, curArg, info)
	if err != nil {
		if curErr != nil && errors.Is(err, workspace.ErrUnknownRef) {
			return refResolution{}, curErr
		}
		return refResolution{}, err
	}
	return refResolution{ref: ref, ws: res.Workspace, loc: res.Workspace.Location}, nil
}

// fileExists reports whether path exists (any kind of entry).
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
