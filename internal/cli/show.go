// `kb show`: print an entry, or the chunks the indexer produced for it. P4.2 implements it.
package cli

import (
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/chunk"
	"github.com/nickstrad/kb/internal/entry"
)

// newShowCmd builds `kb show <entry> [--chunks]`.
func newShowCmd(stdout, stderr io.Writer) *cobra.Command {
	var chunksFlag bool
	cmd := &cobra.Command{
		Use:           "show <entry>",
		Short:         "Print an entry, or its indexed chunks",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runShow(stdout, stderr, Root(), args[0], chunksFlag)
		},
	}
	cmd.Flags().BoolVar(&chunksFlag, "chunks", false, "print the stored chunks instead of the file")
	return cmd
}

// runShow resolves arg to an entry and either prints its file(s) verbatim or its chunks. Exit code
// is ExitUsage (1) when arg does not name a discovered entry.
func runShow(stdout, stderr io.Writer, root, arg string, showChunks bool) error {
	e, err := entry.Resolve(root, arg)
	if err != nil {
		return usageErr("kb show: %s", err)
	}
	if !showChunks {
		return showFiles(stdout, root, e)
	}
	return showEntryChunks(stdout, stderr, root, e)
}

// showFiles prints every file of e verbatim, in index order, with a `==> path <==` header before
// each one only when the entry has more than one file — the same convention `head`/`tail` use when
// given multiple files.
func showFiles(stdout io.Writer, root string, e entry.Entry) error {
	files, err := entry.Load(root, e)
	if err != nil {
		return usageErr("kb show: %s", err)
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
// that fallback on stderr, per plan.md's P4.2 row.
func showEntryChunks(stdout, stderr io.Writer, root string, e entry.Entry) error {
	st, exists, err := openStoreIfExists(root)
	if err != nil {
		return usageErr("kb show: %s", err)
	}
	if exists {
		defer st.Close()

		row, err := st.GetEntryByPath(e.Path)
		if err != nil {
			return usageErr("kb show: %s", err)
		}
		if row != nil {
			chunks, err := st.ListChunks(row.ID)
			if err != nil {
				return usageErr("kb show: %s", err)
			}
			for _, c := range chunks {
				printChunk(stdout, c.Ord, c.SourceFile, c.Heading, c.Text, c.TextHash)
			}
			return nil
		}
	}

	fmt.Fprintln(stderr, "(not indexed; showing what the indexer would produce)")
	files, err := entry.Load(root, e)
	if err != nil {
		return usageErr("kb show: %s", err)
	}
	chunks, err := chunk.Split(e, files)
	if err != nil {
		return usageErr("kb show: %s", err)
	}
	for _, c := range chunks {
		printChunk(stdout, c.Ord, c.SourceFile, c.Heading, c.Text, c.TextHash)
	}
	return nil
}

// printChunk renders one chunk exactly as plan.md's P4.2 row specifies:
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
