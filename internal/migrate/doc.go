// Package migrate is kb's framework for versioned files: the store index DB, the embedding
// cache and (later) the store config and location files. It knows nothing about SQL or TOML.
// Each artefact package lists its forward-only steps as a Steps[T] over its own handle type and
// implements Artefact in a small adapter; Run drives one or more of them.
//
// Inputs: Steps[T] (append-only; Steps[i] takes version i to i+1), Artefact implementations and
// Options. Outputs: a Report of what was (or, in a dry run, would be) applied, with the backup
// paths and the combined Effect the user still has to act on (reindex, re-embed).
//
// Invariants:
//   - A released step is never edited or reordered; a fix is a new step. Steps.Check enforces
//     the shape (From == index, IDs "<artefact>/<NNNN>-<slug>" with NNNN == From+1, hence unique; a
//     one-line Desc, a known Effect, a non-nil Up).
//   - Run takes an advisory flock on "migrate.lock" beside the artefact for the whole
//     re-read-status, backup, apply and verify sequence, so two runners never both apply.
//   - An artefact newer than the binary (Status.Newer) is never touched.
//   - If the backup fails nothing is applied. Apply is atomic where the backend allows (SQLite:
//     every pending step and the version stamp in one transaction), so a failure or a cancel
//     leaves the starting version and the backup.
//
// Errors: Run returns *errs.Error. A lock held past Options.LockWait or a newer artefact is a
// Conflict; a cancelled ctx is Interrupted; an artefact's own error keeps its kind.
package migrate
