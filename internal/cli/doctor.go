// `kb doctor`: check the driver, the embedder, and the drift between the database, the files and
// index.md.
//
// The checks, in order: config, sources, database, migrate, driver, entries, embed_meta, model,
// entries/db-vs-files, index.md, version. A location or store that does not resolve is one
// "FAIL location" / "FAIL store" line ("not initialised: run kb init").
//
// When the store's config.toml cannot be read, the config FAIL stands alone: sources discovery,
// embed_meta, model, entries, entries/db-vs-files and index.md are skipped rather than computed
// from defaults.
//
// Every check prints exactly one line while it runs — "ok   <check>: <detail>",
// "FAIL <check>: <detail>" or "warn <check>: <detail>" — except entries/db-vs-files, which print
// one FAIL or warn line per offending path plus a summary ok line when there were none. The
// command exits non-zero if any check FAILed; a warn never affects the exit code.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/entry"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/index"
	"github.com/nickstrad/kb/internal/migrate"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/store/sqlite"
	"github.com/nickstrad/kb/internal/workspace"
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
			// Resolution failures are doctor's findings, not aborts: one FAIL line, the summary
			// and the failing exit code, like any other failed check.
			if _, _, _, _, err := d.location(cmd); err != nil {
				return doctorUnresolved(stdout, "location", locationProblem(err), err)
			}
			rr, err := d.resolveWorkspace(cmd)
			if err != nil {
				return doctorUnresolved(stdout, "store", err.Error(), err)
			}
			return runDoctor(cmd.Context(), cmd, d, stdout, rr.ws, rr.lf.DefaultStore)
		},
	}
}

// locationProblem is the detail of the location FAIL line: a missing location says to run kb init.
func locationProblem(err error) string {
	if errs.KindOf(err) == errs.NotFound {
		return "not initialised: run kb init (" + err.Error() + ")"
	}
	return err.Error()
}

// doctorUnresolved prints the one FAIL line for a location or store that could not be resolved.
func doctorUnresolved(stdout io.Writer, check, detail string, cause error) error {
	fmt.Fprintf(stdout, "FAIL %s: %s\n", check, detail)
	fmt.Fprintln(stdout, "doctor: 1 failed, 0 warnings")
	return errs.Wrap(cause, errs.KindOf(cause), "cli.doctor", "1 check(s) failed")
}

// doctorLevel is a check's verdict.
type doctorLevel string

const (
	levelOK   doctorLevel = "ok"
	levelWarn doctorLevel = "warn"
	levelFail doctorLevel = "FAIL"
)

// doctorCheck is one check's verdict: what was checked, how it went, and the detail line. Tests
// assert (Name, Level), and Detail only where its wording is the contract.
type doctorCheck struct {
	Name   string
	Level  doctorLevel
	Detail string
}

// String is the line doctor prints for the check.
func (c doctorCheck) String() string { return fmt.Sprintf("%-4s %s: %s", c.Level, c.Name, c.Detail) }

// doctorState records the checks in the order they ran. Nothing is printed until print, once at
// the end, so a check is a function from inputs to recorded verdicts.
type doctorState struct {
	checks []doctorCheck
}

func (d *doctorState) add(level doctorLevel, check, detail string) {
	d.checks = append(d.checks, doctorCheck{Name: check, Level: level, Detail: detail})
}

func (d *doctorState) ok(check, detail string)   { d.add(levelOK, check, detail) }
func (d *doctorState) fail(check, detail string) { d.add(levelFail, check, detail) }
func (d *doctorState) warn(check, detail string) { d.add(levelWarn, check, detail) }

// count is the number of recorded checks at level.
func (d *doctorState) count(level doctorLevel) (n int) {
	for _, c := range d.checks {
		if c.Level == level {
			n++
		}
	}
	return n
}

// print writes every check line and the final "doctor: N failed, M warnings" line.
func (d *doctorState) print(w io.Writer) {
	for _, c := range d.checks {
		fmt.Fprintln(w, c)
	}
	fmt.Fprintf(w, "doctor: %d failed, %d warnings\n", d.count(levelFail), d.count(levelWarn))
}

// runDoctor runs every check in turn and prints the final "doctor: N failed, M warnings" line.
// It returns a non-nil error (ExitUsage) only when at least one check FAILed. defaultStore is the
// location's default_store, from the kb.toml ref resolution read: index.md is checked only for it.
func runDoctor(ctx context.Context, cmd *cobra.Command, dp *deps, stdout io.Writer, ws *workspace.Workspace, defaultStore string) error {
	d := &doctorState{}
	root, dbPath := ws.Root, ws.DBPath

	// A broken config is a FAIL, not an abort. The checks that read nothing from it (database,
	// migrate, driver, version) still run; the ones that need its settings are skipped, since
	// defaults would only add FAILs that echo the real one (runConfigChecks).
	cfg, err := dp.loadConfig(cmd, ws)
	configOK := err == nil
	if err != nil {
		d.fail("config", err.Error())
		cfg = config.Default()
	} else {
		d.ok("config", ws.ConfigPath)
		checkSources(d, root, ws.ConfigPath, cfg.Sources)
	}

	var st store.Store
	if _, statErr := os.Stat(dbPath); statErr != nil {
		d.warn("database", fmt.Sprintf("not indexed: run kb reindex --all (no database at %s)", dbPath))
	} else if pending := pendingMigration(ctx, dbPath); pending != "" {
		// Commands refuse an older index (PLAN 1.8b), so doctor says what to run instead of
		// opening it, which would fail the same way.
		d.fail("migrate", pending)
	} else if opened, err := dp.openStore(ctx, dbPath, false); err != nil {
		d.fail("driver", err.Error())
	} else {
		st = opened
		defer st.Close()
		checkDriver(ctx, d, st)
	}

	if partials := migrate.Partials(dbPath); len(partials) > 0 {
		d.warn("migrate", fmt.Sprintf("%d unfinished backup(s) from an interrupted migration, e.g. %s; delete them or run kb migrate up", len(partials), partials[0]))
	}

	if configOK {
		runConfigChecks(ctx, cmd, d, dp, ws, cfg, st, defaultStore)
	}
	checkVersion(d)

	d.print(stdout)
	if n := d.count(levelFail); n > 0 {
		return usageErr("%d check(s) failed", n)
	}
	return nil
}

// runConfigChecks runs the checks that depend on the store's settings: source discovery, the
// embedder against embed_meta, entry validity, db-vs-files and index.md. They are skipped when
// the config is unreadable, since defaults would only produce FAILs that echo the real one.
func runConfigChecks(ctx context.Context, cmd *cobra.Command, d *doctorState, dp *deps, ws *workspace.Workspace, cfg config.Config, st store.Store, defaultStore string) {
	root := ws.Root
	entries, skipped, err := entry.Discover(root, cfg.Sources)
	if err != nil {
		d.fail("sources", err.Error())
		entries = nil
	}
	checkDiscovered(d, cfg.Sources, entries, skipped)
	goodEntries := make([]entry.Entry, 0, len(entries))
	for _, e := range entries {
		if e.Err == nil {
			goodEntries = append(goodEntries, e)
		}
	}

	embedder, err := dp.newEmbedder(cmd, cfg.Embed)
	if err != nil {
		d.fail("model", err.Error())
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
	if defaultStore == ws.Store {
		checkIndexMD(d, root, goodEntries)
	} else {
		d.ok("index.md", fmt.Sprintf("not checked: it describes the default store %q", defaultStore))
	}
}

// pendingMigration describes the steps an older index database still needs, "" when it is current,
// newer (the open reports that) or unreadable (the open reports that too). It never changes the file.
func pendingMigration(ctx context.Context, dbPath string) string {
	st, err := sqlite.NewArtefact(dbPath).Status(ctx)
	if err != nil || st.Newer() || len(st.Pending) == 0 || st.Current == 0 {
		return ""
	}
	ids := make([]string, len(st.Pending))
	for i, p := range st.Pending {
		ids[i] = p.ID
	}
	return fmt.Sprintf("%s is at schema v%d, this kb needs v%d (%s); run kb migrate up", dbPath, st.Current, st.Target, strings.Join(ids, ", "))
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

// checkSources reports the configured sources (PLAN 1.5): none is a warning, since the store then
// indexes nothing; an entries directory that does not exist yet is a warning (kb add creates it).
// Overlaps, skipped symlinks and globs that match nothing need discovery: checkDiscovered.
func checkSources(d *doctorState, root, cfgPath string, sources []config.Source) {
	if len(sources) == 0 {
		d.warn("sources", fmt.Sprintf("none configured: this store indexes nothing; add a [[sources]] table to %s", cfgPath))
		return
	}
	names := make([]string, len(sources))
	for i, s := range sources {
		names[i] = s.String()
		if s.Layout != config.LayoutEntries {
			continue
		}
		dir := filepath.Join(root, filepath.FromSlash(s.Path))
		if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
			d.warn("sources", fmt.Sprintf("sources[%d] %s: no directory %s yet, nothing indexed from it; kb add creates it", i+1, s, dir))
		}
	}
	d.ok("sources", fmt.Sprintf("%d configured: %s", len(sources), strings.Join(names, ", ")))
}

// checkDiscovered reports what discovery found per source: a warning for each path it skipped (a
// symlink out of the location root, DEFAULTS #65) and for a files glob that matches no *.md file.
func checkDiscovered(d *doctorState, sources []config.Source, entries []entry.Entry, skipped []entry.Skipped) {
	for _, s := range skipped {
		d.warn("sources", "skipped "+s.String())
	}
	count := make([]int, len(sources))
	for _, e := range entries {
		if e.Source >= 0 && e.Source < len(count) {
			count[e.Source]++
		}
	}
	for i, s := range sources {
		if s.Layout == config.LayoutFiles && count[i] == 0 {
			d.warn("sources", fmt.Sprintf("sources[%d] %s matches no *.md file", i+1, s))
		}
	}
}

// checkEmbedder loads the configured embedder when it supports it (the pinger interface, defined
// in reindex.go, is satisfied by embed/local, which loads the model), then embeds the text "ping"
// and reports both timings. The none embedder has nothing to reach. A test embedder
// (internal/embed/fake) does not implement Ping, so it is reported as a warning rather than
// skipped silently or faked as ok. The check is called "model".
func checkEmbedder(ctx context.Context, d *doctorState, embedder embed.Embedder) {
	if embed.IsNone(embedder) {
		d.ok("model", "none: FTS-only index and search")
		return
	}
	p, isPinger := embedder.(pinger)
	if !isPinger {
		d.warn("model", "not checked (test embedder)")
		return
	}
	t0 := time.Now()
	if err := p.Ping(ctx); err != nil {
		d.fail("model", err.Error())
		return
	}
	load := time.Since(t0)
	t1 := time.Now()
	vecs, err := embedder.Embed(ctx, []string{embed.DocText(embedder, "ping")})
	if err != nil {
		d.fail("model", fmt.Sprintf("loaded %s but embedding \"ping\" failed: %v", embedder.Model(), err))
		return
	}
	if len(vecs) != 1 || len(vecs[0]) != embedder.Dim() {
		d.fail("model", fmt.Sprintf("%s returned the wrong shape for \"ping\": want 1 vector of %d dims", embedder.Model(), embedder.Dim()))
		return
	}
	d.ok("model", fmt.Sprintf("loaded %s in %dms, embeds \"ping\" in %dms", embedder.Model(), load.Milliseconds(), time.Since(t1).Milliseconds()))
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
		if e.InPlace() {
			continue // in-place files need no front matter: no tags or updated to expect
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
