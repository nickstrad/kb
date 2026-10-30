//go:build noassert

package assert

// Enabled is false: the checks are compiled out.
const Enabled = false

// Pre does nothing in a noassert build.
func Pre(bool, string, ...any) {}

// Post does nothing in a noassert build.
func Post(bool, string, ...any) {}

// Invariant does nothing in a noassert build.
func Invariant(bool, string, ...any) {}
