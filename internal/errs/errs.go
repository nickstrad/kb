// Package errs is kb's well-formed error: a known edge case with a friendly message for the user,
// plus the context and the stack a developer needs (docs/CODING_STYLE.md §1).
//
// A module boundary is any place that calls Wrap or New. Inside a module, errors are returned as
// they are or annotated with fmt.Errorf("...: %w", err). Kind decides the exit code at the top
// level (internal/cli): an error whose first *Error is Internal, or that has no *Error at all, is
// a bug and becomes an incident.
package errs

import (
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"sort"
	"time"
)

// Kind classifies a well-formed error. The zero value, Internal, is a bug if it reaches the top.
type Kind int

const (
	Internal    Kind = iota // a bug, or a failure nobody turned into a well-formed error: exit 3
	Usage                   // bad flag, argument, input file or config value: exit 1
	NotFound                // no index, no entry, no search id: exit 1
	Conflict                // embed_meta mismatch, wrong schema version, damaged database: exit 1
	Unavailable             // the embedder cannot be reached or loaded: exit 2
	Interrupted             // the user cancelled (SIGINT/SIGTERM) and Msg says what was kept: exit 1
)

var kindNames = [...]string{"internal", "usage", "not_found", "conflict", "unavailable", "interrupted"}

// String names the kind as the incident log records it.
func (k Kind) String() string {
	if k >= 0 && int(k) < len(kindNames) {
		return kindNames[k]
	}
	return fmt.Sprintf("kind(%d)", int(k))
}

// Error is a well-formed error. Error() is Msg alone; %+v prints everything (Format).
type Error struct {
	Kind  Kind
	Op    string         // "sqlite.ReplaceEntry", "local.Embed", "search.Search"
	Msg   string         // friendly, one line, no driver/SQL detail
	Ctx   map[string]any // path, search_id, model, db: what the reader needs
	Err   error          // inner cause; may be nil
	Time  time.Time
	Stack []byte // debug.Stack() at the first wrap only
}

// Error returns the friendly message, or the inner error's text when Msg is empty.
func (e *Error) Error() string {
	if e.Msg == "" && e.Err != nil {
		return e.Err.Error()
	}
	return e.Msg
}

// Unwrap exposes the inner cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.Err }

// With adds context key/value pairs (path, search_id, model, ...) and returns e. A trailing key
// without a value is recorded with a nil value.
func (e *Error) With(kv ...any) *Error {
	if e.Ctx == nil {
		e.Ctx = make(map[string]any, len(kv)/2)
	}
	for i := 0; i < len(kv); i += 2 {
		key := fmt.Sprint(kv[i])
		var v any
		if i+1 < len(kv) {
			v = kv[i+1]
		}
		e.Ctx[key] = v
	}
	return e
}

// New makes a well-formed error with no inner cause.
func New(k Kind, op, format string, args ...any) *Error {
	return &Error{Kind: k, Op: op, Msg: fmt.Sprintf(format, args...), Time: time.Now(), Stack: debug.Stack()}
}

// Wrap makes err well-formed. It keeps the stack and time of an inner *Error rather than
// capturing a second one. A nil err gives an error with no inner cause, never a nil *Error.
func Wrap(err error, k Kind, op, format string, args ...any) *Error {
	e := &Error{Kind: k, Op: op, Msg: fmt.Sprintf(format, args...), Err: err}
	var inner *Error
	if errors.As(err, &inner) && inner.Stack != nil {
		e.Time, e.Stack = inner.Time, inner.Stack
		return e
	}
	e.Time, e.Stack = time.Now(), debug.Stack()
	return e
}

// KindOf returns the Kind of the first *Error in err's chain, or Internal if there is none.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return Internal
}

// Chain lists the message of every error in err's chain, outermost first. It follows Unwrap() error
// only; a joined error is listed as one entry.
func Chain(err error) []string {
	var out []string
	for err != nil {
		var line string
		if e, ok := err.(*Error); ok {
			line = fmt.Sprintf("%s [%s] %s", e.Op, e.Kind, e.Msg)
			if len(e.Ctx) > 0 {
				line += " " + formatCtx(e.Ctx)
			}
		} else {
			line = err.Error()
		}
		out = append(out, line)
		err = errors.Unwrap(err)
	}
	return out
}

// Format implements fmt.Formatter: %v and %s print Error(); %+v prints the chain (Op, Kind, Msg
// and Ctx of every *Error, the text of every other error) and the stack.
func (e *Error) Format(s fmt.State, verb rune) {
	if verb == 'v' && s.Flag('+') {
		for i, line := range Chain(e) {
			if i > 0 {
				io.WriteString(s, "\n  caused by: ")
			}
			io.WriteString(s, line)
		}
		if len(e.Stack) > 0 {
			io.WriteString(s, "\n")
			s.Write(e.Stack)
		}
		return
	}
	io.WriteString(s, e.Error())
}

// formatCtx renders a context map with sorted keys, so the output is stable.
func formatCtx(ctx map[string]any) string {
	keys := make([]string, 0, len(ctx))
	for k := range ctx {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := "{"
	for i, k := range keys {
		if i > 0 {
			out += " "
		}
		out += fmt.Sprintf("%s=%v", k, ctx[k])
	}
	return out + "}"
}
