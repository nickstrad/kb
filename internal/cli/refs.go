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

// locationReader reads a location's kb.toml for ref resolution. Every command reads it strictly
// (config.ReadLocation); kb migrate reads it with rawLocation, since an older kb.toml is what that
// command upgrades. It is the only thing that differs between them: the grammar is resolveRefWith.
type locationReader func(path string) (config.Location, error)

func locationInfo(lf config.Location) workspace.LocationInfo {
	return workspace.LocationInfo{DefaultStore: lf.DefaultStore, Aliases: lf.Aliases}
}

// resolver builds the ref resolver for cmd, reading kb.toml files with read; its warnings go to
// the command's stderr.
func (d *deps) resolver(cmd *cobra.Command, read locationReader) workspace.Resolver {
	return workspace.Resolver{
		Env: d.workspaceEnv(),
		Read: func(l *workspace.Location) (workspace.LocationInfo, error) {
			lf, err := read(l.FilePath)
			if err != nil {
				return workspace.LocationInfo{}, err
			}
			return locationInfo(lf), nil
		},
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

// refQuery is one ref to resolve. fromFlag marks the command's own --store, which meets --root and
// --global: a ref that names its own location (or an alias) cannot be combined with them, and
// under them a bare NAME is a store of that location only. A fan-out ref (--stores, search.stores)
// has fromFlag false: --root/--global only choose the current location it is resolved against.
// needStore false resolves only the location (kb stores, kb doctor; see resolveRef).
type refQuery struct {
	ref       workspace.Ref
	fromFlag  bool
	needStore bool
	read      locationReader
}

// resolveRef applies --root, --global and --store, reading kb.toml strictly. It is the one place
// the grammar meets the flags:
//   - global, NAME@LOC and PATH name their own location. NAME@LOC and PATH with --root or --global
//     are Usage errors, and so is global with --root (--global is the same thing).
//   - a bare NAME is a store of the current location (--root, --global, or discovery), else an alias
//     of that location or of the global one. With --root or --global an alias is Usage: it names its
//     own location.
//   - no --store is the location's default_store.
//
// With needStore false (kb stores, kb doctor) only the location is resolved, so an unknown store
// does not hide the others; ws is nil then, except for an alias, whose location is its target's.
func (d *deps) resolveRef(cmd *cobra.Command, needStore bool) (refResolution, error) {
	return d.resolveStoreFlag(cmd, needStore, config.ReadLocation)
}

// resolveStoreFlag is resolveRef with the kb.toml reader given (kb migrate passes rawLocation).
func (d *deps) resolveStoreFlag(cmd *cobra.Command, needStore bool, read locationReader) (refResolution, error) {
	storeFlag, _ := cmd.Flags().GetString("store") // a root persistent flag: "" is safe
	ref, err := workspace.ParseRef(storeFlag)
	if err != nil {
		return refResolution{}, err
	}
	return d.resolveRefWith(cmd, refQuery{ref: ref, fromFlag: true, needStore: needStore, read: read})
}

// resolveFanOutRef resolves a ref a search fans out to (always needing the store).
func (d *deps) resolveFanOutRef(cmd *cobra.Command, ref workspace.Ref) (refResolution, error) {
	return d.resolveRefWith(cmd, refQuery{ref: ref, needStore: true, read: config.ReadLocation})
}

// resolveRefWith is the grammar of resolveRef and of fan-out refs (refQuery).
func (d *deps) resolveRefWith(cmd *cobra.Command, q refQuery) (rr refResolution, err error) {
	// Get* fails only for an undefined flag; all are root persistent flags, so "" / false is safe.
	root, _ := cmd.Flags().GetString("root")
	global, _ := cmd.Flags().GetBool("global")
	env := d.workspaceEnv()
	rr.ref = q.ref
	ref := rr.ref
	explicit := root != "" || global
	rr.how = "discovered from " + env.Cwd
	r := d.resolver(cmd, q.read)
	switch {
	case !q.fromFlag && ref.NamesLocation():
		return d.resolveOwnLocation(r, rr, q)
	case ref.Global && root != "":
		return refResolution{}, usageErr("--store global cannot be combined with --root: global names the global location")
	case (ref.At != "" || ref.Path != "") && explicit:
		flag := "--root"
		if global {
			flag = "--global"
		}
		return refResolution{}, usageErr("--store %s names its own location, so it cannot be combined with %s", ref, flag)
	case ref.At != "" || ref.Path != "":
		return d.resolveOwnLocation(r, rr, q)
	case ref.Global:
		global, rr.how = true, "--store global"
	case root != "":
		rr.how = "--root"
	case global:
		rr.how = "--global"
	}
	cur, lf, curErr := currentLocation(root, global, env, q.read)
	if ref.Name == "" { // no --store, or global: the location's default_store
		if curErr != nil {
			return refResolution{}, curErr
		}
		rr.loc, rr.lf = cur, lf
		if q.needStore {
			if rr.ws, err = defaultStoreOf(cur, lf); err != nil {
				return refResolution{}, err
			}
		}
		return rr, nil
	}

	// A bare NAME: a store of the current location, else an alias.
	localOnly := explicit && q.fromFlag
	r.LocalOnly = localOnly
	var curArg *workspace.Location
	var info workspace.LocationInfo
	if curErr == nil {
		curArg, info = cur, locationInfo(lf)
	}
	_, localErr := workspaceStore(cur, ref.Name, curErr)
	if localOnly && localErr != nil && curErr == nil && r.HasAlias(ref.Name, cur, info) {
		return refResolution{}, usageErr("--store %s is an alias, which names its own location, so it cannot be combined with --root or --global", ref.Name)
	}
	if !q.needStore && curErr == nil {
		rr.loc, rr.lf = cur, lf
		if localErr != nil && r.HasAlias(ref.Name, cur, info) { // an alias: the command's location is its target's
			if res, rerr := r.Resolve(ref, curArg, info); rerr == nil {
				return fromResolved(rr, res, q.read)
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
	return fromResolved(rr, res, q.read)
}

// workspaceStore is cur.Store(name), or curErr when there is no current location.
func workspaceStore(cur *workspace.Location, name string, curErr error) (*workspace.Workspace, error) {
	if curErr != nil {
		return nil, curErr
	}
	return cur.Store(name)
}

// fromResolved fills rr from a store Resolve found.
func fromResolved(rr refResolution, res workspace.Resolved, read locationReader) (refResolution, error) {
	lf, err := read(res.Workspace.Location.FilePath)
	if err != nil {
		return refResolution{}, err
	}
	rr.loc, rr.lf, rr.ws = res.Workspace.Location, lf, res.Workspace
	if res.Via != "store" {
		rr.how = fmt.Sprintf("--store %s: %s", rr.ref, res.Via)
	}
	return rr, nil
}

// resolveOwnLocation handles refs that name their own location: NAME@LOC and PATH (and global,
// for a fan-out ref).
func (d *deps) resolveOwnLocation(r workspace.Resolver, rr refResolution, q refQuery) (refResolution, error) {
	loc, info, err := r.Location(rr.ref)
	if err != nil {
		return refResolution{}, err
	}
	lf, err := q.read(loc.FilePath)
	if err != nil {
		return refResolution{}, err
	}
	rr.loc, rr.lf, rr.how = loc, lf, "--store "+rr.ref.String()
	if q.needStore {
		res, err := r.Resolve(rr.ref, nil, info)
		if err != nil {
			return refResolution{}, err
		}
		rr.ws = res.Workspace
	}
	return rr, nil
}

// currentLocation resolves the location from the flags or discovery and reads its kb.toml with
// read; err says why there is none.
func currentLocation(root string, global bool, env workspace.Env, read locationReader) (*workspace.Location, config.Location, error) {
	loc, err := workspace.ResolveLocation(root, global, env)
	if err != nil {
		return nil, config.Location{}, err
	}
	lf, err := read(loc.FilePath)
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
	ws, err := d.resolveFanOut(cmd, ref)
	if err != nil || ws == nil {
		return nil, err
	}
	return d.openResolvedReadOnly(cmd, ref, ws)
}

// warnSkip prints the one warning for a fan-out store that is left out.
func warnSkip(cmd *cobra.Command, ref, format string, args ...any) {
	fmt.Fprintf(cmd.ErrOrStderr(), "warning: skipping store %s: %s\n", ref, fmt.Sprintf(format, args...))
}

// resolveFanOut is openStoreReadOnly's first half: the store ref names. A malformed ref, or a
// Usage error from resolution, is returned; a ref that names nothing is warned about and nil.
func (d *deps) resolveFanOut(cmd *cobra.Command, ref string) (*workspace.Workspace, error) {
	parsed, err := workspace.ParseRef(ref)
	if err != nil {
		return nil, err
	}
	if parsed.IsZero() {
		return nil, usageErr("empty store ref")
	}
	rr, err := d.resolveFanOutRef(cmd, parsed)
	if err != nil {
		if errs.KindOf(err) == errs.Usage {
			return nil, err
		}
		warnSkip(cmd, ref, "%v", err)
		return nil, nil
	}
	return rr.ws, nil
}

// openResolvedReadOnly is openStoreReadOnly's second half: open ws's database read-only, warning
// about (and returning nil for) a store with no database or with a schema this kb does not read.
func (d *deps) openResolvedReadOnly(cmd *cobra.Command, ref string, ws *workspace.Workspace) (*openedStore, error) {
	canonical := workspace.Canonical(ws.Store, ws.Root)
	ctx := cmd.Context()
	if !fileExists(ws.DBPath) {
		warnSkip(cmd, ref, "%s has no database yet (not indexed)", canonical)
		return nil, nil
	}
	st, err := d.openStore(ctx, ws.DBPath, true)
	if err != nil {
		if errs.KindOf(err) == errs.Conflict {
			msg := err.Error()
			if errors.Is(err, sqlite.ErrSchemaOlder) { // the fix runs in that store
				msg += fmt.Sprintf(" (for that store: kb --store %s migrate up)", canonical)
			}
			warnSkip(cmd, ref, "%s", msg)
			return nil, nil
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

// fileExists reports whether path exists (any kind of entry).
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
