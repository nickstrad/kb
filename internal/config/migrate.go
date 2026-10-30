package config

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/migrate"
)

// The store config and the location file are versioned artefacts (docs/design/migrations.md): the
// `version` key is their schema version, and kb migrate up is the only thing that rewrites them
// to a newer one. A step transforms the decoded TOML tree (map[string]any); the result is
// validated by the same Parse the commands use, written to a temp file beside the original and
// renamed over it, so a failure or a Ctrl-C leaves the old file. Comments are lost on a rewrite;
// the backup keeps the original text. Steps are append-only, like the index's.

// Artefact names, as kb migrate prints them and as step IDs begin.
const (
	ConfigArtefactName   = "config"
	LocationArtefactName = "location"
)

// configSteps[v] takes a store config from version v to v+1. SchemaVersion == len(configSteps).
// v1 is the first released schema, so there is only the baseline, which changes nothing.
var configSteps = migrate.Steps[map[string]any]{
	{
		Info: migrate.Info{From: 0, ID: "config/0001-baseline", Desc: "the first config schema (version = 1)"},
		Up:   func(context.Context, map[string]any) error { return nil },
	},
}

// locationSteps is configSteps for the location file; LocationSchemaVersion == len(locationSteps).
var locationSteps = migrate.Steps[map[string]any]{
	{
		Info: migrate.Info{From: 0, ID: "location/0001-baseline", Desc: "the first location schema (version = 1)"},
		Up:   func(context.Context, map[string]any) error { return nil },
	},
}

func init() {
	if assert.Enabled {
		assert.Invariant(configSteps.Check(ConfigArtefactName) == nil, "config migrations: %v", configSteps.Check(ConfigArtefactName))
		assert.Invariant(locationSteps.Check(LocationArtefactName) == nil, "location migrations: %v", locationSteps.Check(LocationArtefactName))
		assert.Invariant(configSteps.Target() == SchemaVersion, "config steps reach v%d, SchemaVersion is %d", configSteps.Target(), SchemaVersion)
		assert.Invariant(locationSteps.Target() == LocationSchemaVersion, "location steps reach v%d, LocationSchemaVersion is %d", locationSteps.Target(), LocationSchemaVersion)
	}
}

// tomlArtefact is a TOML file with a top-level `version` key as a migrate.Artefact.
type tomlArtefact struct {
	name, path string
	steps      migrate.Steps[map[string]any]
	// check parses text the way commands read the file; it must accept exactly a current file.
	check func(data []byte, file string) error
}

var (
	_ migrate.Artefact = (*tomlArtefact)(nil)
	_ migrate.Verifier = (*tomlArtefact)(nil)
)

// NewArtefact returns the migration adapter for the store config at path. A missing file is
// version 0 with nothing pending: kb init creates configs, kb migrate never does.
func NewArtefact(path string) migrate.Artefact {
	return &tomlArtefact{name: ConfigArtefactName, path: path, steps: configSteps,
		check: func(data []byte, file string) error { _, err := Parse(data, file); return err }}
}

// NewLocationArtefact is NewArtefact for the location file (.kb/kb.toml).
func NewLocationArtefact(path string) migrate.Artefact {
	return &tomlArtefact{name: LocationArtefactName, path: path, steps: locationSteps,
		check: func(data []byte, file string) error { _, err := ParseLocation(data, file); return err }}
}

// version reads the file's `version` key: found is false for a missing file, and a file without
// the key is at the current version (that is how Parse reads it too). Anything but a positive
// integer is a Usage error naming the file.
func (a *tomlArtefact) version() (v int, found bool, data []byte, err error) {
	const op = "config.Artefact.Status"
	data, _, err = readCapped(op, a.name, a.path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil, nil
	}
	if err != nil {
		return 0, false, nil, err
	}
	var head map[string]any
	if _, err := toml.Decode(string(data), &head); err != nil {
		return 0, true, nil, decodeErr(a.path, err)
	}
	raw, ok := head["version"]
	if !ok {
		return a.steps.Target(), true, data, nil
	}
	n, ok := raw.(int64)
	if !ok || n < 1 || n > 1<<31 {
		return 0, true, nil, errs.New(errs.Usage, op, "%s: version: must be a positive integer, got %v", a.path, raw)
	}
	return int(n), true, data, nil
}

// Status reads the file's version without changing it.
func (a *tomlArtefact) Status(ctx context.Context) (migrate.Status, error) {
	st := migrate.Status{Artefact: a.name, Path: a.path, Target: a.steps.Target()}
	if err := ctx.Err(); err != nil {
		return st, errs.Wrap(err, errs.Interrupted, "config.Artefact.Status", "interrupted")
	}
	v, found, _, err := a.version()
	if err != nil {
		return st, err
	}
	if !found {
		return st, nil
	}
	st.Current = v
	st.Pending = a.steps.Pending(min(v, st.Target))
	return st, nil
}

// Backup copies the file's bytes to dst, which must not exist.
func (a *tomlArtefact) Backup(ctx context.Context, dst string) error {
	const op = "config.Artefact.Backup"
	data, _, err := readCapped(op, a.name, a.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return errs.Wrap(err, errs.NotFound, op, "no %s file at %s to back up", a.name, a.path)
		}
		return err
	}
	if err := ctx.Err(); err != nil {
		return errs.Wrap(err, errs.Interrupted, op, "interrupted")
	}
	return writeFileExcl(op, dst, data, a.mode())
}

// Apply runs every pending step on the decoded tree, checks the result with the commands' own
// parser, and replaces the file atomically. It is all-or-nothing: on any failure the file is as it
// was. A newer or missing file is refused.
func (a *tomlArtefact) Apply(ctx context.Context) ([]migrate.Info, error) {
	const op = "config.Artefact.Apply"
	v, found, data, err := a.version()
	switch {
	case err != nil:
		return nil, err
	case !found:
		return nil, errs.New(errs.NotFound, op, "no %s file at %s; kb init creates it", a.name, a.path)
	case v > a.steps.Target():
		return nil, errs.New(errs.Conflict, op, "%s is at version %d; this kb only understands %d; use a newer kb", a.path, v, a.steps.Target())
	case v == a.steps.Target():
		return nil, nil
	}
	tree := map[string]any{}
	if _, err := toml.Decode(string(data), &tree); err != nil {
		return nil, decodeErr(a.path, err)
	}
	stamp := func(_ context.Context, h map[string]any, version int) error {
		h["version"] = int64(version)
		return nil
	}
	applied, err := a.steps.Up(ctx, tree, v, stamp)
	if err != nil {
		return nil, errs.Wrap(err, migrateKind(ctx, err), op, "could not migrate %s: %v", a.path, err)
	}
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(tree); err != nil {
		return nil, errs.Wrap(err, errs.Internal, op, "cannot encode the migrated %s", a.path)
	}
	if err := a.check(buf.Bytes(), a.path); err != nil {
		return nil, errs.Wrap(err, errs.Internal, op, "the migrated %s would not be valid (%v); the file was not changed", a.path, err)
	}
	if err := ctx.Err(); err != nil { // the last chance to stop before the rename
		return nil, errs.Wrap(err, errs.Interrupted, op, "interrupted")
	}
	if err := replaceFile(op, a.path, buf.Bytes(), a.mode()); err != nil {
		return nil, err
	}
	return applied, nil
}

// Verify checks that the file is valid at the current version, with the parser commands use.
func (a *tomlArtefact) Verify(ctx context.Context) error {
	const op = "config.Artefact.Verify"
	data, _, err := readCapped(op, a.name, a.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return errs.Wrap(err, errs.NotFound, op, "no %s file at %s to verify", a.name, a.path)
		}
		return err
	}
	if err := a.check(data, a.path); err != nil {
		return errs.Wrap(err, errs.Conflict, op, "%v", err)
	}
	return nil
}

func migrateKind(ctx context.Context, err error) errs.Kind {
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return errs.Interrupted
	}
	return errs.Internal
}

// mode is the file's permission bits, 0o644 when it cannot be read.
func (a *tomlArtefact) mode() fs.FileMode {
	if fi, err := os.Stat(a.path); err == nil {
		return fi.Mode().Perm()
	}
	return 0o644
}

// writeFileExcl creates path with data and fails when it exists (a backup never overwrites).
func writeFileExcl(op, path string, data []byte, mode fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return errs.Wrap(err, errs.Usage, op, "cannot create %s: %v", path, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path) // a half-written backup must not pass for a good one
		return errs.Wrap(err, errs.Usage, op, "cannot write %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return errs.Wrap(err, errs.Usage, op, "cannot write %s: %v", path, err)
	}
	return nil
}

// replaceFile writes data to a temp file beside path, syncs it and renames it over path.
func replaceFile(op, path string, data []byte, mode fs.FileMode) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".migrate-*")
	if err != nil {
		return errs.Wrap(err, errs.Usage, op, "cannot write beside %s: %v", path, err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name()) // best effort: the original is intact either way
		}
	}()
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Chmod(mode)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		return errs.Wrap(err, errs.Usage, op, "cannot replace %s: %v", path, err)
	}
	return nil
}
