// `kb doctor`: check the driver, the embedder, and the drift between the database, the files and
// index.md. P4.4 implements it.
//
// Every check prints exactly one line while it runs — "ok   <check>: <detail>",
// "FAIL <check>: <detail>" or "warn <check>: <detail>" — except entries/db-vs-files, which print
// one FAIL or warn line per offending path plus a summary ok line when there were none. The
// command exits non-zero if any check FAILed; a warn never affects the exit code.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/entry"
	"github.com/nickstrad/kb/internal/index"
	"github.com/nickstrad/kb/internal/store"
)

// newDoctorCmd builds `kb doctor`.
func newDoctorCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:           "doctor",
		Short:         "Check driver, embedder and index health",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(cmd.Context(), cmd, stdout, Root())
		},
	}
}

// doctorState accumulates the ok/FAIL/warn lines doctor prints and the counts that decide its
// exit code and final summary line.
type doctorState struct {
	stdout io.Writer
	failed int
	warned int
}

func (d *doctorState) ok(check, detail string) {
	fmt.Fprintf(d.stdout, "ok   %s: %s\n", check, detail)
}

func (d *doctorState) fail(check, detail string) {
	fmt.Fprintf(d.stdout, "FAIL %s: %s\n", check, detail)
	d.failed++
}

func (d *doctorState) warn(check, detail string) {
	fmt.Fprintf(d.stdout, "warn %s: %s\n", check, detail)
	d.warned++
}

// runDoctor runs every check in turn and prints the final "doctor: N failed, M warnings" line.
// It returns a non-nil error (ExitUsage) only when at least one check FAILed.
func runDoctor(ctx context.Context, cmd *cobra.Command, stdout io.Writer, root string) error {
	d := &doctorState{stdout: stdout}

	dbPath := DBPath(root)
	var st *store.Store
	if _, statErr := os.Stat(dbPath); statErr != nil {
		d.fail("database", fmt.Sprintf("none at %s; run kb reindex --all", dbPath))
	} else if opened, err := store.Open(dbPath); err != nil {
		d.fail("driver", err.Error())
	} else {
		st = opened
		defer st.Close()
		checkDriver(d, st)
	}

	entries, err := entry.Discover(root)
	if err != nil {
		d.fail("entries", err.Error())
		entries = nil
	}
	goodEntries := make([]entry.Entry, 0, len(entries))
	for _, e := range entries {
		if e.Err == nil {
			goodEntries = append(goodEntries, e)
		}
	}

	embedder, err := newEmbedder(cmd)
	if err != nil {
		d.fail("embedder", err.Error())
	} else {
		if st != nil {
			checkEmbedMeta(d, st, embedder)
		}
		checkEmbedder(ctx, d, embedder)
	}
	checkEntries(d, entries)
	if st != nil {
		checkDBVsFiles(d, root, st, goodEntries)
	}
	checkIndexMD(d, root, goodEntries)
	checkBinary(d)

	fmt.Fprintf(stdout, "doctor: %d failed, %d warnings\n", d.failed, d.warned)
	if d.failed > 0 {
		return usageErr("kb doctor: %d check(s) failed", d.failed)
	}
	return nil
}

// checkDriver confirms the open database's driver has fts5 and sqlite-vec, and prints their
// versions. store.Open has already refused to open a database missing either, so a failure here
// would mean the pragma queries themselves failed, not that the extensions are absent.
func checkDriver(d *doctorState, st *store.Store) {
	rows, err := st.DB().Query("PRAGMA compile_options")
	if err != nil {
		d.fail("driver", fmt.Sprintf("read compile_options: %s", err))
		return
	}
	hasFTS5 := false
	for rows.Next() {
		var opt string
		if err := rows.Scan(&opt); err != nil {
			rows.Close()
			d.fail("driver", fmt.Sprintf("read compile_options: %s", err))
			return
		}
		if strings.Contains(opt, "ENABLE_FTS5") {
			hasFTS5 = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		d.fail("driver", fmt.Sprintf("read compile_options: %s", err))
		return
	}
	rows.Close()
	if !hasFTS5 {
		d.fail("driver", "sqlite compiled without ENABLE_FTS5; rebuild with `go build -tags fts5 ./cmd/kb`")
		return
	}

	var vecVer string
	if err := st.DB().QueryRow("select vec_version()").Scan(&vecVer); err != nil {
		d.fail("driver", fmt.Sprintf("vec_version(): %s", err))
		return
	}
	var sqliteVer string
	if err := st.DB().QueryRow("select sqlite_version()").Scan(&sqliteVer); err != nil {
		d.fail("driver", fmt.Sprintf("sqlite_version(): %s", err))
		return
	}
	d.ok("driver", fmt.Sprintf("fts5 enabled, sqlite %s, sqlite-vec %s", sqliteVer, vecVer))
}

// checkEmbedMeta compares the stored embed_meta row against the configured embedder.
func checkEmbedMeta(d *doctorState, st *store.Store, embedder embed.Embedder) {
	model, dim, ok, err := st.EmbedMeta()
	if err != nil {
		d.fail("embed_meta", err.Error())
		return
	}
	if !ok {
		d.fail("embed_meta", "not set (empty database); run kb reindex --all")
		return
	}
	storable := dim == store.VecDim || (model == store.FTSOnlyModel && dim == 0)
	if model != embedder.Model() || dim != embedder.Dim() || !storable {
		d.fail("embed_meta", fmt.Sprintf(
			"database has %s (%d dims), configured embedder is %s (%d dims); run kb reindex --all",
			model, dim, embedder.Model(), embedder.Dim()))
		return
	}
	d.ok("embed_meta", fmt.Sprintf("model=%s dim=%d", model, dim))
}

// checkEmbedder pings the configured embedder when it supports it (the pinger interface, defined
// in reindex.go, is satisfied by the Ollama and OpenAI-style clients). The none embedder has
// nothing to reach. A test embedder (internal/embed/fake) does not implement Ping, so it is
// reported as a warning rather than skipped silently or faked as ok.
func checkEmbedder(ctx context.Context, d *doctorState, embedder embed.Embedder) {
	if embed.IsNone(embedder) {
		d.ok("embedder", "none: FTS-only index and search")
		return
	}
	p, isPinger := embedder.(pinger)
	if !isPinger {
		d.warn("embedder", "not checked (test embedder)")
		return
	}
	if err := p.Ping(ctx); err != nil {
		d.fail("embedder", err.Error())
		return
	}
	where := ""
	if u, ok := embedder.(interface{ BaseURL() string }); ok {
		where = " at " + u.BaseURL()
	}
	d.ok("embedder", fmt.Sprintf("reachable%s, model %s", where, embedder.Model()))
}

// checkEntries reports every entry whose front matter failed to parse or validate as a FAIL, and
// a missing tags or updated field as a warn, then an ok summary when there were no failures.
func checkEntries(d *doctorState, entries []entry.Entry) {
	failures := 0
	for _, e := range entries {
		if e.Err != nil {
			d.fail("entry", fmt.Sprintf("%s: %s", e.Path, e.Err))
			failures++
			continue
		}
		if len(e.Meta.Tags) == 0 {
			d.warn("entry", fmt.Sprintf("%s: no tags", e.Path))
		}
		if strings.TrimSpace(e.Meta.Updated) == "" {
			d.warn("entry", fmt.Sprintf("%s: no updated date", e.Path))
		}
	}
	if failures == 0 {
		d.ok("entries", fmt.Sprintf("%d entries valid", len(entries)))
	}
}

// checkDBVsFiles compares every good (Err == nil) entry's on-disk body_hash against the database,
// and reports database rows whose file no longer exists on disk.
func checkDBVsFiles(d *doctorState, root string, st *store.Store, goodEntries []entry.Entry) {
	rows, err := st.ListEntries()
	if err != nil {
		d.fail("db-vs-files", err.Error())
		return
	}
	byPath := make(map[string]store.EntryRow, len(rows))
	for _, r := range rows {
		byPath[r.Path] = r
	}

	onDisk := make(map[string]bool, len(goodEntries))
	stale, inSync := 0, 0
	for _, e := range goodEntries {
		onDisk[e.Path] = true
		files, err := entry.Load(root, e)
		if err != nil {
			d.fail("db-vs-files", fmt.Sprintf("stale %s: %s; run kb reindex %s", e.Path, err, e.Path))
			stale++
			continue
		}
		hash := entry.BodyHash(files, e.Files)
		row, known := byPath[e.Path]
		if !known || row.BodyHash != hash {
			d.fail("db-vs-files", fmt.Sprintf("stale %s; run kb reindex %s", e.Path, e.Path))
			stale++
			continue
		}
		inSync++
	}

	orphan := 0
	for _, r := range rows {
		if !onDisk[r.Path] {
			d.fail("db-vs-files", fmt.Sprintf("orphan %s", r.Path))
			orphan++
		}
	}

	if stale == 0 && orphan == 0 {
		d.ok("db-vs-files", fmt.Sprintf("%d entries in sync", inSync))
	}
}

// checkIndexMD accepts an absent optional snapshot and warns when an existing one is stale.
func checkIndexMD(d *doctorState, root string, goodEntries []entry.Entry) {
	if _, err := os.Stat(filepath.Join(root, index.Name)); err != nil {
		if os.IsNotExist(err) {
			d.ok("index.md", "absent (optional export)")
			return
		}
		d.fail("index.md", err.Error())
		return
	}
	differs, err := index.Diff(root, goodEntries, index.Today())
	if err != nil {
		d.fail("index.md", err.Error())
		return
	}
	if differs {
		d.warn("index.md", "optional snapshot is stale; run kb index to refresh or delete it")
		return
	}
	d.ok("index.md", "up to date")
}

// checkBinary is informational only: it warns when /usr/local/bin/kb, which scripts/install.sh
// links to the freshly built binary, does not resolve to the kb that is running now, so a stale
// or foreign install shows up.
func checkBinary(d *doctorState) {
	const linkPath = "/usr/local/bin/kb"
	target, err := filepath.EvalSymlinks(linkPath)
	if err != nil {
		d.warn("binary", fmt.Sprintf("%s: %s; run scripts/install.sh", linkPath, err))
		return
	}
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		d.warn("binary", fmt.Sprintf("cannot resolve the running executable: %s", err))
		return
	}
	if target != self {
		d.warn("binary", fmt.Sprintf("%s -> %s, but this kb is %s; run scripts/install.sh", linkPath, target, self))
		return
	}
	d.ok("binary", fmt.Sprintf("%s -> %s", linkPath, target))
}
