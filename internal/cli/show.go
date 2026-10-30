// `kb show`: print an entry, or the chunks the indexer produced for it.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/chunk"
	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/entry"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/workspace"
)

// newShowCmd builds `kb show <entry> [--chunks]`.
func newShowCmd(d *deps, stdout, stderr io.Writer) *cobra.Command {
	var chunksFlag, noLog bool
	cmd := &cobra.Command{
		Use:   "show <entry>",
		Short: "Print an entry, or its indexed chunks",
		Long: "Prints the entry's file(s) verbatim, or with --chunks the chunks the indexer stored.\n" +
			"REF:path reads an entry of another store, as a cross-store search shows it (REF is a\n" +
			"store name, an alias or global); the open is still recorded in this store's log.\n\n" +
			"If a recent search (log.open_window, 30 minutes by default) returned the entry, kb show also records an\n" +
			"\"open\" against the newest such search; kb stats reports it as opened_pct. That is\n" +
			"best effort: a failure never changes the exit code, and is only reported with\n" +
			"--debug. --no-log turns the recording off.",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ws, cfg, err := d.workspaceConfig(cmd)
			if err != nil {
				return err
			}
			tgt, arg, err := d.showTarget(cmd, ws, cfg, args[0])
			if err != nil {
				return err
			}
			return runShowIn(cmd.Context(), d, stdout, stderr, tgt, arg, chunksFlag, !noLog, debugFlag(cmd))
		},
	}
	cmd.Flags().BoolVar(&chunksFlag, "chunks", false, "print the stored chunks instead of the file")
	cmd.Flags().BoolVar(&noLog, "no-log", false, "do not record this show as an open of a recent search")
	return cmd
}

// showStore is where kb show reads the entry (ws, with its config) and where it records the open:
// logWS's log, under store (the canonical ref of ws, "" when ws is logWS itself), within window.
type showStore struct {
	ws     *workspace.Workspace
	cfg    config.Config
	logWS  *workspace.Workspace
	store  string
	window time.Duration
}

// showTarget splits a store-qualified REF:path (REF a store name, an alias or global, so the
// prefix has no "/", "@" or "."). An arg whose prefix is not such a word, or names no store or
// alias while the whole arg is an entry of this store, is a plain path. A ref naming this store
// itself is a plain path too.
func (d *deps) showTarget(cmd *cobra.Command, ws *workspace.Workspace, cfg config.Config, arg string) (showStore, string, error) {
	here := showStore{ws: ws, cfg: cfg, logWS: ws, window: cfg.Log.OpenWindowDuration()}
	prefix, path, ok := strings.Cut(arg, ":")
	if !ok || path == "" || (prefix != workspace.GlobalName && workspace.ValidName(prefix) != nil) {
		return here, arg, nil
	}
	ref, err := workspace.ParseRef(prefix)
	if err != nil {
		return here, arg, nil
	}
	rr, err := d.resolveFanOutRef(cmd, ref)
	if err != nil {
		if _, perr := entry.Resolve(ws.Root, cfg.Sources, arg); errors.Is(err, workspace.ErrUnknownRef) && perr == nil {
			return here, arg, nil // a path that happens to contain a colon
		}
		return showStore{}, "", err
	}
	canonical := workspace.Canonical(rr.ws.Store, rr.ws.Root)
	if canonical == workspace.Canonical(ws.Store, ws.Root) {
		return here, path, nil
	}
	tcfg, err := d.loadConfig(cmd, rr.ws)
	if err != nil {
		return showStore{}, "", err
	}
	return showStore{ws: rr.ws, cfg: tcfg, logWS: ws, store: canonical, window: here.window}, path, nil
}

// runShow is runShowIn for an entry of ws itself.
func runShow(ctx context.Context, d *deps, stdout, stderr io.Writer, ws *workspace.Workspace, cfg config.Config, arg string, showChunks, logOpen, debug bool) error {
	return runShowIn(ctx, d, stdout, stderr, showStore{ws: ws, cfg: cfg, logWS: ws, window: cfg.Log.OpenWindowDuration()}, arg, showChunks, logOpen, debug)
}

// runShowIn resolves arg to an entry of t.ws and either prints its file(s) verbatim or its chunks,
// then (when logOpen) records the open in t.logWS. Exit code is ExitUsage (1) when arg does not
// name a discovered entry; recording the open never changes it.
func runShowIn(ctx context.Context, d *deps, stdout, stderr io.Writer, t showStore, arg string, showChunks, logOpen, debug bool) error {
	root := t.ws.Root
	e, err := entry.Resolve(root, t.cfg.Sources, arg)
	if err != nil {
		return err
	}
	if !showChunks {
		err = showFiles(stdout, root, e)
	} else {
		err = showEntryChunks(ctx, d, stdout, stderr, t.ws, e)
	}
	if err != nil || !logOpen {
		return err
	}
	recordOpen(ctx, d, stderr, t.logWS, store.OpenRecord{Store: t.store, EntryPath: e.Path, At: d.clock(), Window: t.window}, debug)
	return nil
}

// recordOpen is the deliberately silent path (CODING_STYLE §1): the entry has already been printed,
// which is what the user asked for, so a failure to note the open must not fail the command or add
// noise to stderr. It is not swallowed: with debug the cause is printed as one stderr line. A
// missing database is not a failure; there is no search to attach to.
func recordOpen(ctx context.Context, d *deps, stderr io.Writer, ws *workspace.Workspace, o store.OpenRecord, debug bool) {
	report := func(format string, args ...any) {
		if debug {
			fmt.Fprintf(stderr, "kb show: "+format+"\n", args...)
		}
	}
	st, exists, err := d.openStoreIfExists(ctx, ws)
	if err != nil {
		report("open not recorded: %s", err)
		return
	}
	if !exists {
		report("open not recorded: no index yet")
		return
	}
	defer st.Close()
	recorded, err := st.RecordOpen(ctx, o)
	if err != nil {
		report("open not recorded: %s", err)
		return
	}
	if !recorded {
		report("open not recorded: no search in the last %s returned %s, or the open is already on record", o.Window, o.EntryPath)
	}
}

// showFiles prints every file of e verbatim, in index order, with a `==> path <==` header before
// each one only when the entry has more than one file — the same convention `head`/`tail` use when
// given multiple files.
func showFiles(stdout io.Writer, root string, e entry.Entry) error {
	files, err := entry.Load(root, e)
	if err != nil {
		return err
	}
	multi := len(e.Files) > 1
	for i, f := range e.Files {
		if multi {
			if i > 0 {
				fmt.Fprintln(stdout)
			}
			fmt.Fprintf(stdout, "==> %s <==\n", f)
		}
		if _, err := stdout.Write(files[f]); err != nil {
			return err
		}
	}
	return nil
}

// showEntryChunks prints the chunks stored in the database for e, or, when e has never been
// indexed (or there is no database yet), the chunks chunk.Split would produce right now — noting
// that fallback on stderr.
func showEntryChunks(ctx context.Context, d *deps, stdout, stderr io.Writer, ws *workspace.Workspace, e entry.Entry) error {
	st, exists, err := d.openStoreIfExists(ctx, ws)
	if err != nil {
		return err
	}
	if exists {
		defer st.Close()

		row, err := st.Entry(ctx, e.Path)
		if err != nil {
			return err
		}
		if row != nil {
			chunks, err := st.Chunks(ctx, row.ID)
			if err != nil {
				return err
			}
			for _, c := range chunks {
				printChunk(stdout, c.Ord, c.SourceFile, c.Heading, c.Text, c.TextHash)
			}
			return nil
		}
	}

	fmt.Fprintln(stderr, "(not indexed; showing what the indexer would produce)")
	files, err := entry.Load(ws.Root, e)
	if err != nil {
		return err
	}
	chunks, err := chunk.Split(e, files)
	if err != nil {
		return usageErr("%s", err)
	}
	for _, c := range chunks {
		printChunk(stdout, c.Ord, c.SourceFile, c.Heading, c.Text, c.TextHash)
	}
	return nil
}

// printChunk renders one chunk as:
//
//	--- chunk <ord> [<source_file>] <heading or "(summary)"> (<len> chars, <text_hash[:12]>)
//	<text>
//
// The summary chunk's heading ("" in both the chunks table and chunk.Chunk) prints as "(summary)".
func printChunk(stdout io.Writer, ord int, sourceFile, heading, text, textHash string) {
	h := heading
	if h == "" {
		h = "(summary)"
	}
	short := textHash
	if len(short) > 12 {
		short = short[:12]
	}
	fmt.Fprintf(stdout, "--- chunk %d [%s] %s (%d chars, %s)\n", ord, sourceFile, h, utf8.RuneCountInString(text), short)
	fmt.Fprintln(stdout, text)
}

// debugFlag reports whether the root persistent --debug flag is set.
func debugFlag(cmd *cobra.Command) bool {
	on, _ := cmd.Flags().GetBool("debug") // only fails if the flag is undefined; false is the safe default
	return on
}
