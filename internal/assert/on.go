//go:build !noassert

package assert

import "fmt"

// Enabled is true when the checks are compiled in.
const Enabled = true

// Pre panics with a precondition Violation when cond is false.
func Pre(cond bool, format string, args ...any) {
	if !cond {
		fail("precondition", format, args)
	}
}

// Post panics with a postcondition Violation when cond is false.
func Post(cond bool, format string, args ...any) {
	if !cond {
		fail("postcondition", format, args)
	}
}

// Invariant panics with an invariant Violation when cond is false.
func Invariant(cond bool, format string, args ...any) {
	if !cond {
		fail("invariant", format, args)
	}
}

func fail(kind, format string, args []any) {
	panic(Violation{Kind: kind, Msg: fmt.Sprintf(format, args...)})
}
