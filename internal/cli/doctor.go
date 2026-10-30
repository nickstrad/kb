// `kb doctor`: check the driver, the embedder, and the drift between the database, the files and
// index.md.
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
	"github.com/nickstrad/kb/internal/store/sqlite"
)

// newDoctorCmd builds `kb doctor`.
func newDoctorCmd(d *deps, stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:           "doctor",
		Short:         "Check driver, embedder and index health",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(cmd.Context(), cmd, d, stdout, Root())
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
func runDoctor(ctx context.Context, cmd *cobra.Command, dp *deps, stdout io.Writer, root string) error {
	d := &doctorState{stdout: stdout}

	dbPath := DBPath(root)
	var st store.Store
	if _, statErr := os.Stat(dbPath); statErr != nil {
		d.fail("database", fmt.Sprintf("none at %s; run kb reindex --all", dbPath))
	} else if opened, err := dp.openStore(ctx, dbPath, false); err != nil {
		d.fail("driver", err.Error())
	} else {
		st = opened
		defer st.Close()
		checkDriver(ctx, d, st)
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

	embedder, err := dp.newEmbedder(cmd)
	if err != nil {
		d.fail("embedder", err.Error())
	} else {
		if st != nil {
			checkEmbedMeta(ctx, d, st, embedder)
		}
		checkEmbedder(ctx, d, embedder)
	}
	checkEntries(d, entries)
	if st != nil {
		checkDBVsFiles(ctx, d, root, st, goodEntries)
	}
	checkIndexMD(d, root, goodEntries)
	checkVersion(d)

	fmt.Fprintf(stdout, "doctor: %d failed, %d warnings\n", d.failed, d.warned)
	if d.failed > 0 {
		return usageErr("%d check(s) failed", d.failed)
	}
	return nil
}

// checkDriver prints the SQLite and sqlite-vec versions. Open has already refused a driver
// without FTS5 or sqlite-vec, so a failure here means the queries themselves failed.
func checkDriver(ctx context.Context, d *doctorState, st store.Store) {
	di, ok := st.(interface {
		DriverInfo(context.Context) (sqlite.DriverInfo, error)
	})
	if !ok {
		d.ok("driver", "not a SQLite store")
		return
	}
	info, err := di.DriverInfo(ctx)
	if err != nil {
		d.fail("driver", err.Error())
		return
	}
	if !info.FTS5 {
		d.fail("driver", "sqlite compiled without ENABLE_FTS5; rebuild with `go build -tags fts5 ./cmd/kb`")
		return
	}
	d.ok("driver", fmt.Sprintf("fts5 enabled, sqlite %s, sqlite-vec %s", info.SQLite, info.Vec))
}

// checkEmbedMeta compares the stored embed_meta row against the configured embedder.
func checkEmbedMeta(ctx context.Context, d *doctorState, st store.Index, embedder embed.Embedder) {
	m, ok, err := st.EmbedMeta(ctx)
	if err != nil {
		d.fail("embed_meta", err.Error())
		return
	}
	if !ok {
		if entries, err := st.Entries(ctx); err == nil && len(entries) > 0 {
			noun, verb := "entries", "are"
			if len(entries) == 1 {
				noun, verb = "entry", "is"
			}
			d.fail("embed_meta", fmt.Sprintf("not set, but %d %s %s indexed (no vectors yet: upgraded or never embedded); run kb reindex --all", len(entries), noun, verb))
			return
		}
		d.fail("embed_meta", "not set (empty database); run kb reindex --all")
		return
	}
	storable := (m.Dim > 0 && m.Dim <= sqlite.MaxVecDim) || (m.Model == store.FTSOnlyModel && m.Dim == 0)
	if m.Model != embedder.Model() || m.Dim != embedder.Dim() || !storable {
		d.fail("embed_meta", fmt.Sprintf(
			"database has %s (%d dims), configured embedder is %s (%d dims); run kb reindex --all",
			m.Model, m.Dim, embedder.Model(), embedder.Dim()))
		return
	}
	d.ok("embed_meta", fmt.Sprintf("model=%s dim=%d", m.Model, m.Dim))
}

// checkEmbedder pings the configured embedder when it supports it (the pinger interface, defined
// in reindex.go, is satisfied by embed/local, which loads the model). The none embedder has
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
	d.ok("embedder", fmt.Sprintf("loaded, model %s", embedder.Model()))
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
func checkDBVsFiles(ctx context.Context, d *doctorState, root string, st store.Index, goodEntries []entry.Entry) {
	rows, err := st.Entries(ctx)
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

// checkVersion reports which kb is running and where it lives. It is informational: kb can be
// installed anywhere (go install, a release download), so there is no expected path to check.
func checkVersion(d *doctorState) {
	where := "unknown path"
	if self, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(self); err == nil {
			self = resolved
		}
		where = self
	}
	d.ok("version", fmt.Sprintf("kb %s at %s", Version, where))
}
