package eval

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/nickstrad/kb/internal/errs"
)

// BaselineVersion is the baseline file schema this kb writes and reads. A baseline is derived
// (the store's eval/ directory is git-ignored and `kb eval --save-baseline` rebuilds it from the
// log), so it is not in the kb migrate framework: any other version is refused with the command
// that rewrites it, and nothing is converted in place.
const BaselineVersion = 1

// DefaultBaseline is the baseline name when none is given.
const DefaultBaseline = "default"

const maxBaselineName = 64

var baselineName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// ValidBaselineName reports whether name is a usable baseline name: lowercase letters, digits,
// '.', '_' and '-', starting with a letter or digit, at most 64 bytes. It is a file name, so no
// path separator or leading dot gets through.
func ValidBaselineName(name string) bool {
	return len(name) <= maxBaselineName && baselineName.MatchString(name)
}

// Baseline is a saved evaluation: what it ran with and every query's result, enough for
// Compare to pair its queries with another run's. Its JSON is the baseline file.
type Baseline struct {
	Version    int       `json:"version"`
	Name       string    `json:"name"`
	KBVersion  string    `json:"kb_version"`
	CreatedAt  time.Time `json:"created_at"`
	EmbedModel string    `json:"embed_model"`
	EntryCount int       `json:"entry_count"`
	// Settings is the canonical effective-settings JSON; SettingsHash its hash.
	Settings     json.RawMessage `json:"settings"`
	SettingsHash string          `json:"settings_hash"`
	// Report carries mode, k, other_store, the metrics and per_query (first_gold_rank per query).
	Report Report `json:"report"`
}

// Path is where baseline name lives under a store's eval directory.
func baselinePath(dir, name string) string { return filepath.Join(dir, name+".json") }

// SaveBaseline writes b to dir/<b.Name>.json (creating dir), atomically: a reader never sees a
// half-written baseline. b.Version is set to BaselineVersion.
func SaveBaseline(dir string, b Baseline) error {
	const op = "eval.SaveBaseline"
	if !ValidBaselineName(b.Name) {
		return errs.New(errs.Usage, op, "invalid baseline name %q (lowercase letters, digits, '.', '_' or '-', at most %d)", b.Name, maxBaselineName)
	}
	b.Version = BaselineVersion
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(b); err != nil {
		return errs.Wrap(err, errs.Internal, op, "encode baseline %q: %v", b.Name, err)
	}
	// Driver-only: the 0o755 mode is umask-dependent, so no test pins it, and the failure needs a
	// filesystem fault seam (testability: fault-seams-only-in-sqlite).
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errs.Wrap(err, errs.Usage, op, "cannot create %s: %v", dir, err).With("path", dir)
	}
	path := baselinePath(dir, b.Name)
	tmp, err := os.CreateTemp(dir, b.Name+".json.tmp*")
	if err != nil {
		return errs.Wrap(err, errs.Usage, op, "cannot write %s: %v", path, err).With("path", path)
	}
	_, werr := tmp.Write(buf.Bytes())
	// Driver-only: a failed write, chmod or close needs a file fault seam (testability:
	// fault-seams-only-in-sqlite); no unit test reaches this branch.
	if err := errors.Join(werr, tmp.Chmod(0o644), tmp.Close()); err != nil {
		os.Remove(tmp.Name()) // best effort: the error that matters is the failed write
		return errs.Wrap(err, errs.Usage, op, "cannot write %s: %v", path, err).With("path", path)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name()) // best effort: the error that matters is the failed write
		return errs.Wrap(err, errs.Usage, op, "cannot write %s: %v", path, err).With("path", path)
	}
	return nil
}

// LoadBaseline reads dir/<name>.json. A missing file is NotFound; a file that is not a baseline,
// or has another schema version, is Conflict and names the command that rewrites it.
func LoadBaseline(dir, name string) (*Baseline, error) {
	const op = "eval.LoadBaseline"
	if !ValidBaselineName(name) {
		return nil, errs.New(errs.Usage, op, "invalid baseline name %q (lowercase letters, digits, '.', '_' or '-', at most %d)", name, maxBaselineName)
	}
	path := baselinePath(dir, name)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errs.Wrap(err, errs.NotFound, op, "no baseline %q at %s; save one with kb eval --save-baseline %s", name, path, name).With("path", path)
	}
	if err != nil {
		return nil, errs.Wrap(err, errs.Usage, op, "cannot read %s: %v", path, err).With("path", path)
	}
	var head struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, errs.Wrap(err, errs.Conflict, op, "%s is not a baseline (%v); rewrite it with kb eval --save-baseline %s", path, err, name).With("path", path)
	}
	if head.Version != BaselineVersion {
		return nil, errs.New(errs.Conflict, op, "baseline %s is version %d; this kb reads version %d; rewrite it with kb eval --save-baseline %s",
			path, head.Version, BaselineVersion, name).With("path", path)
	}
	var b Baseline
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, errs.Wrap(err, errs.Conflict, op, "%s is damaged (%v); rewrite it with kb eval --save-baseline %s", path, err, name).With("path", path)
	}
	return &b, nil
}
