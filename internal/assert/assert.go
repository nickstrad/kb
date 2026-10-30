// Package assert checks preconditions, postconditions and invariants that only a kb programmer
// can break (docs/TESTING.md §3). A failed check panics with a Violation; it is a bug by
// definition and is never turned into an error. The top level (cli.Execute) recovers it, writes
// an incident and exits 3.
//
// Build with -tags noassert to compile the checks out. A bare call costs its arguments even
// then, so anything that loops, allocates or calls a function goes inside if assert.Enabled {}.
package assert

// Violation is the panic value of a failed assertion.
type Violation struct{ Kind, Msg string }

// Error lets a recovered Violation be reported like an error.
func (v Violation) Error() string { return "kb assert: " + v.Kind + " violated: " + v.Msg }
