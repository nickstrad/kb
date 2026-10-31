package migrate

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/nickstrad/kb/internal/assert"
)

// Effect is what a step leaves for the user to do after it runs. Effects are ordered: a later
// one implies the earlier ones (a re-embed is also a reindex), so the combined effect of several
// steps is the largest.
type Effect uint8

const (
	EffectNone    Effect = iota // nothing to do
	EffectReindex               // derived rows were cleared; `kb reindex` rebuilds them (embeddings reused)
	EffectReembed               // vectors were dropped; `kb reindex` re-embeds (model needed)
	effectEnd                   // first invalid value
)

var effectNames = [...]string{"none", "reindex", "reembed"}

// String is the effect's stable name, as kb migrate prints it: none, reindex or reembed.
func (e Effect) String() string {
	if e < effectEnd {
		return effectNames[e]
	}
	return "effect(" + strconv.Itoa(int(e)) + ")"
}

// Info describes one step. ID is stable forever: "<artefact>/<NNNN>-<slug>" with NNNN = From+1,
// e.g. "index/0001-baseline".
type Info struct {
	From   int    // the step takes the artefact from version From to From+1
	ID     string // stable, unique within the artefact
	Desc   string // one line, shown by kb migrate status
	Effect Effect
}

// Step is one forward migration over a backend-specific handle T (an execer for SQLite, a
// map[string]any tree for TOML). Up uses only h: it never calls kb's current Go code, never
// begins or commits a transaction, and never touches the network or the model.
type Step[T any] struct {
	Info
	Up func(ctx context.Context, h T) error
}

// Steps is an append-only list: Steps[i] takes version i to i+1, so the target version is its
// length. Version 0 means "does not exist yet".
type Steps[T any] []Step[T]

// Target is the version this build writes: one per step.
func (s Steps[T]) Target() int { return len(s) }

// Pending lists the steps that take version current to Target, in order; nil when current is
// at or past Target. current must not be negative (adapters validate what they read from disk).
func (s Steps[T]) Pending(current int) []Info {
	assert.Pre(current >= 0, "Pending: current version %d is negative", current)
	if current >= len(s) {
		return nil
	}
	out := make([]Info, 0, len(s)-current)
	for _, st := range s[current:] {
		out = append(out, st.Info)
	}
	return out
}

// idRe is the shape of a step ID: artefact, a four-digit number and a kebab-case slug.
var idRe = regexp.MustCompile(`^([a-z][a-z0-9-]*)/([0-9]{4})-([a-z0-9]+(?:-[a-z0-9]+)*)$`)

// Check verifies the list's shape for the named artefact: Steps[i].From == i, every ID is
// "<artefact>/<NNNN>-<slug>" with NNNN == From+1, Desc is one non-empty line, the Effect is known
// and Up is set. Every artefact calls it in a test and an init-time assertion.
// IDs are unique because each carries its own index.
func (s Steps[T]) Check(artefact string) error {
	for i, st := range s {
		if err := st.check(artefact, i); err != nil {
			return err
		}
	}
	return nil
}

func (st Step[T]) check(artefact string, i int) error {
	if st.From != i {
		return fmt.Errorf("%s step %d (%s): From is %d, want %d (steps are contiguous from 0)", artefact, i, st.ID, st.From, i)
	}
	m := idRe.FindStringSubmatch(st.ID)
	if m == nil {
		return fmt.Errorf("%s step %d: ID %q is not <artefact>/<NNNN>-<slug>", artefact, i, st.ID)
	}
	if m[1] != artefact {
		return fmt.Errorf("%s step %d: ID %q belongs to artefact %q", artefact, i, st.ID, m[1])
	}
	if n, _ := strconv.Atoi(m[2]); n != i+1 { // the regexp guarantees four digits
		return fmt.Errorf("%s step %d: ID %q is numbered %04d, want %04d (From+1)", artefact, i, st.ID, n, i+1)
	}
	if strings.TrimSpace(st.Desc) == "" || strings.ContainsAny(st.Desc, "\r\n") {
		return fmt.Errorf("%s step %d (%s): Desc must be one non-empty line", artefact, i, st.ID)
	}
	if st.Effect >= effectEnd {
		return fmt.Errorf("%s step %d (%s): unknown effect %d", artefact, i, st.ID, st.Effect)
	}
	if st.Up == nil {
		return fmt.Errorf("%s step %d (%s): Up is nil", artefact, i, st.ID)
	}
	return nil
}

// Up runs the steps from version current to Target on h, calling stamp(ctx, h, v) after the step
// that reaches v. An adapter with transactional DDL calls it inside one transaction (so stamping
// is part of it); one without stamps durably, so a rerun resumes. It returns the steps that
// completed; on error the failing step's ID is in the message and the cause is wrapped.
func (s Steps[T]) Up(ctx context.Context, h T, current int, stamp func(ctx context.Context, h T, version int) error) ([]Info, error) {
	assert.Pre(current >= 0, "Up: current version %d is negative", current)
	var done []Info
	for v := current; v < len(s); v++ {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		st := s[v]
		if err := st.Up(ctx, h); err != nil {
			return done, fmt.Errorf("step %s (v%d to v%d): %w", st.ID, v, v+1, err)
		}
		if err := stamp(ctx, h, v+1); err != nil {
			return done, fmt.Errorf("stamp version %d after %s: %w", v+1, st.ID, err)
		}
		done = append(done, st.Info)
	}
	return done, nil
}

// Combined is the effect a user must act on after the given steps: the largest of theirs.
func Combined(infos []Info) Effect {
	e := EffectNone
	for _, in := range infos {
		e = max(e, in.Effect)
	}
	return e
}

// Status is one artefact's position.
type Status struct {
	Artefact string // "index", "embed-cache", "config", "location"
	Path     string // the versioned file; migrate.lock and backups go beside it
	Current  int    // 0 = does not exist yet
	Target   int
	Pending  []Info // empty when Current >= Target
}

// Newer reports an artefact written by a newer kb (Current > Target); nothing may touch it.
func (s Status) Newer() bool { return s.Current > s.Target }

// Artefact is anything kb versions. Implementations: sqlite.Artefact (index), cache.Artefact
// (embedding cache); the config and location files follow.
type Artefact interface {
	// Status reads the current version without changing the file. A missing file is Current 0.
	Status(ctx context.Context) (Status, error)
	// Backup writes a consistent copy to dst, which does not exist yet (sqlite: VACUUM INTO;
	// toml: a byte copy). Run calls it before Apply, under the lock.
	Backup(ctx context.Context, dst string) error
	// Apply runs every pending step and returns those it applied. Atomic where the backend allows
	// (all steps and the version stamp in one transaction); otherwise it stamps after each step,
	// so a rerun resumes. Implementations say which.
	Apply(ctx context.Context) ([]Info, error)
}

// Verifier is optional: an artefact that can check itself after Apply (and for kb migrate verify).
type Verifier interface {
	Verify(ctx context.Context) error
}
