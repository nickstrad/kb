// Error reporting: the top level of kb (docs/CODING_STYLE.md §1.4–1.5). Commands return errors;
// report decides what the user sees and the exit code. A well-formed error (an *errs.Error whose
// kind is not Internal) prints its friendly message. Anything else is a bug: it is logged as an
// incident with its full chain and stack, and the user gets an id to quote.
package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/errs"
)

// incidentLogMax is the size the incident log is trimmed back to once it grows past it.
const incidentLogMax = 1 << 20

// usageErr is a well-formed Usage failure (exit 1). The message must not start with the command
// name: report adds "kb <command>: ". The first error among args, if any, is kept as the inner
// cause, so the chain survives for --debug.
func usageErr(format string, args ...any) error {
	return errs.Wrap(firstErr(args), errs.Usage, "cli", format, args...)
}

// embedderErr is a well-formed Unavailable failure (exit 2), built like usageErr.
func embedderErr(format string, args ...any) error {
	return errs.Wrap(firstErr(args), errs.Unavailable, "cli", format, args...)
}

func firstErr(args []any) error {
	for _, a := range args {
		if err, ok := a.(error); ok {
			return err
		}
	}
	return nil
}

// exitSilently is returned by a command that has already printed its own diagnostics (a search
// that fell back to FTS prints its warning after the results) and only needs the exit code.
type exitSilently int

func (e exitSilently) Error() string { return fmt.Sprintf("exit %d", int(e)) }

// report prints one failure to stderr and returns its exit code:
//
//   - exitSilently: nothing more to print.
//   - interrupted (the chain holds context.Canceled, or an Interrupted kind): exit 1 with the
//     interrupted message; Ctrl-C is a known edge case, not a bug.
//   - a well-formed error: "kb <command>: <Msg>", exit 1 or 2 by kind.
//   - anything else: an incident line in the log, a message with its id, exit 3.
//
// With --debug the full chain and stack follow on stderr in every case.
func report(stderr io.Writer, d *deps, cmd *cobra.Command, args []string, err error) int {
	var silent exitSilently
	if errors.As(err, &silent) {
		return int(silent)
	}
	debugOn := cmd != nil && debugFlag(cmd)
	prefix := "kb"
	if cmd != nil {
		prefix = cmd.CommandPath()
	}
	defer func() {
		if debugOn {
			fmt.Fprintf(stderr, "%s\n", detail(err))
		}
	}()

	kind := errs.KindOf(err)
	if kind == errs.Interrupted || errors.Is(err, context.Canceled) {
		msg := "interrupted"
		if kind == errs.Interrupted {
			msg = err.Error()
		}
		fmt.Fprintf(stderr, "%s: %s\n", prefix, msg)
		return ExitUsage
	}
	if kind != errs.Internal {
		fmt.Fprintf(stderr, "%s: %s\n", prefix, err.Error())
		return exitCode(kind)
	}

	id := newIncidentID()
	where := writeIncident(d, id, args, incident{Kind: kind.String(), Chain: errs.Chain(err), Stack: stackOf(err)})
	fmt.Fprintf(stderr, "%s: internal error (id %s). Details: %s, or rerun with --debug. Please report it.\n", prefix, id, where)
	return ExitInternal
}

// reportPanic turns a recovered panic into an incident and ExitInternal.
func reportPanic(stderr io.Writer, d *deps, cmd *cobra.Command, args []string, r any) int {
	stack := debug.Stack()
	kind := "panic"
	if v, ok := r.(assert.Violation); ok {
		kind = "assert:" + v.Kind
	}
	id := newIncidentID()
	where := writeIncident(d, id, args, incident{Kind: kind, Chain: []string{fmt.Sprint(r)}, Stack: string(stack)})
	prefix := "kb"
	if cmd != nil {
		prefix = cmd.CommandPath()
		if debugFlag(cmd) {
			fmt.Fprintf(stderr, "%v\n%s\n", r, stack)
		}
	}
	fmt.Fprintf(stderr, "%s: internal error (id %s). Details: %s, or rerun with --debug. Please report it.\n", prefix, id, where)
	return ExitInternal
}

// exitCode maps a well-formed kind to the CLI contract.
func exitCode(k errs.Kind) int {
	switch k {
	case errs.Unavailable:
		return ExitEmbedder
	case errs.Internal:
		return ExitInternal
	default:
		return ExitUsage
	}
}

// detail is the --debug rendering: the chain and the stack of the first *errs.Error.
func detail(err error) string {
	var e *errs.Error
	if errors.As(err, &e) {
		return fmt.Sprintf("%+v", e)
	}
	return fmt.Sprintf("%v", err)
}

func stackOf(err error) string {
	var e *errs.Error
	if errors.As(err, &e) {
		return string(e.Stack)
	}
	return ""
}

// incident is one line of the incident log.
type incident struct {
	ID      string   `json:"id"`
	Time    string   `json:"time"`
	Version string   `json:"version"`
	Args    []string `json:"args"`
	Kind    string   `json:"kind"`
	Chain   []string `json:"chain"`
	Stack   string   `json:"stack"`
}

func newIncidentID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error (Go 1.24+)
	return hex.EncodeToString(b)
}

// incidentLogPath is <root>/.kb/kb.log when the store's .kb exists, else the user cache dir's
// kb/kb.log. It never creates a store directory.
func incidentLogPath(d *deps) (string, error) {
	kbDir := filepath.Join(Root(), ".kb")
	if fi, err := os.Stat(kbDir); err == nil && fi.IsDir() {
		return filepath.Join(kbDir, "kb.log"), nil
	}
	base, err := d.userCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "kb", "kb.log"), nil
}

// writeIncident appends one JSON line and returns where it went. A log that cannot be written is
// not fatal: the user still gets the id, and --debug still prints the detail.
func writeIncident(d *deps, id string, args []string, inc incident) string {
	inc.ID, inc.Time, inc.Version, inc.Args = id, time.Now().UTC().Format(time.RFC3339), Version, args
	path, err := incidentLogPath(d)
	if err != nil {
		return "unavailable (" + err.Error() + ")"
	}
	line, err := json.Marshal(inc)
	if err != nil {
		return "unavailable (" + err.Error() + ")"
	}
	if err := appendTrimmed(path, append(line, '\n'), incidentLogMax); err != nil {
		return "unavailable (" + err.Error() + ")"
	}
	return path
}

// appendTrimmed appends line to path, then, if the file is over max bytes, keeps only the last
// max bytes, cut at a line boundary.
func appendTrimmed(path string, line []byte, max int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.Write(line)
	if err := errors.Join(werr, f.Close()); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) <= max {
		return err
	}
	data = data[len(data)-max:]
	for i, b := range data {
		if b == '\n' {
			data = data[i+1:]
			break
		}
	}
	return os.WriteFile(path, data, 0o644)
}
