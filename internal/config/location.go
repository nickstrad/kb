package config

import (
	"bytes"
	"errors"
	"io/fs"
	"sort"

	"github.com/BurntSushi/toml"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/workspace"
)

// LocationSchemaVersion is the version of the location file (.kb/kb.toml) this kb reads and writes.
const LocationSchemaVersion = 1

// DefaultStoreName is the store kb init creates first, and the default_store of a fresh location.
const DefaultStoreName = "default"

// MaxAliasTargetLen bounds one alias value, so a hostile file cannot make kb build a huge path.
const MaxAliasTargetLen = 4096

// Location is the content of a location file, .kb/kb.toml: which store commands use when --store
// is absent, and the aliases (store refs) of this location. Aliases are parsed and validated
// here and resolved by PLAN 3.1. Store-level settings are not here: they are in Config.
type Location struct {
	Version      int               `toml:"version"`
	DefaultStore string            `toml:"default_store"`
	Aliases      map[string]string `toml:"aliases"` // alias name -> store ref
}

// DefaultLocation is a new location whose default store is def.
func DefaultLocation(def string) Location {
	return Location{Version: LocationSchemaVersion, DefaultStore: def, Aliases: map[string]string{}}
}

// ReadLocation reads and validates the location file at path. A missing file is NotFound (the
// message says to run kb init); a bigger file than MaxFileBytes, one that is not a regular file,
// bad TOML, an unknown key, a
// newer or older version, a bad store name or an empty alias target is a Usage (version: Conflict)
// error that names the file and key.
func ReadLocation(path string) (Location, error) {
	data, _, err := readCapped("config.ReadLocation", "location", path)
	if errors.Is(err, fs.ErrNotExist) {
		return Location{}, errs.Wrap(err, errs.NotFound, "config.ReadLocation", "no kb location file at %s; run kb init", path)
	}
	if err != nil {
		return Location{}, err
	}
	return ParseLocation(data, path)
}

// ParseLocation decodes location-file TOML onto the defaults and validates it. file names the
// origin in errors.
func ParseLocation(data []byte, file string) (Location, error) {
	var head map[string]any
	if _, err := toml.Decode(string(data), &head); err == nil {
		if v, ok := head["version"].(int64); ok {
			if err := checkVersion(file, v, LocationSchemaVersion); err != nil {
				return Location{}, err
			}
		}
	}
	l := DefaultLocation(DefaultStoreName)
	md, err := toml.Decode(string(data), &l)
	if err != nil {
		return Location{}, decodeErr(file, err)
	}
	if un := md.Undecoded(); len(un) > 0 {
		keys := make([]string, len(un))
		for i, k := range un {
			keys[i] = k.String()
		}
		sort.Strings(keys)
		noun := "key"
		if len(keys) > 1 {
			noun = "keys"
		}
		return Location{}, errs.New(errs.Usage, "config.ParseLocation", "%s: unknown %s %s", file, noun, quoteJoin(keys))
	}
	if l.Aliases == nil {
		l.Aliases = map[string]string{}
	}
	if err := l.Validate(file); err != nil {
		return Location{}, err
	}
	return l, nil
}

// Validate checks every value; src names where l came from. The first problem is a Usage error
// "<src>: <key>: <problem>".
func (l Location) Validate(src string) error {
	problem := func(key, format string, args ...any) error {
		return errs.New(errs.Usage, "config.Location.Validate", "%s: %s: "+format, append([]any{src, key}, args...)...)
	}
	if l.Version != LocationSchemaVersion {
		return problem("version", "unsupported location version %d; this kb reads version %d", l.Version, LocationSchemaVersion)
	}
	if err := workspace.ValidName(l.DefaultStore); err != nil {
		return problem("default_store", "%s", err.Error())
	}
	names := make([]string, 0, len(l.Aliases))
	for name := range l.Aliases {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		key := "aliases." + name
		if err := workspace.ValidName(name); err != nil {
			return problem("aliases", "%s", err.Error())
		}
		switch target := l.Aliases[name]; {
		case target == "":
			return problem(key, "the target is empty")
		case len(target) > MaxAliasTargetLen:
			return problem(key, "the target is %d bytes; at most %d", len(target), MaxAliasTargetLen)
		}
	}
	return nil
}

// SaveLocation renders l as TOML. ParseLocation of the result gives l back.
func SaveLocation(l Location) ([]byte, error) {
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(l); err != nil {
		return nil, errs.Wrap(err, errs.Internal, "config.SaveLocation", "cannot encode the location file")
	}
	return buf.Bytes(), nil
}
