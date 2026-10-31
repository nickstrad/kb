package main

// Scenarios for kb migrate (PLAN 1.8b, docs/design/migrations.md): the refusal of an older or
// newer durable file, two migrators at once, and Ctrl-C during a migration. They need an older
// index database: the helper loadfixture loads the newest checked-in SQL fixture and kb runs one
// index release ahead (H.probe, KB_HARNESS_INDEX_PROBE=1 in the harnessfake build), since the
// baseline squash (D8) left no real older schema. They skip (with a note) under -bin, when the
// harness did not build kb and the helper.

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

var migrateScenarios = []scenario{
	{"migrate-refusal", "an older index is refused with 'run kb migrate up' by every command that opens it and left byte-identical; doctor and stores explain; migrate up backs it up, reports the effect, and search works; a newer index is refused everywhere, migrate included", scMigrateRefusal},
	{"migrate-concurrent", "two kb migrate up on one older store at once: both exit 0, exactly one migrates and writes the backup, the file ends current and verified", scMigrateConcurrent},
	{"migrate-interrupt", "SIGINT at several moments during kb migrate up on a big older store: never a crash, the file is wholly old or wholly new, every backup is a valid database, and a rerun finishes", scMigrateInterrupt},
}

func init() { scenarios = append(scenarios, migrateScenarios...) }

// loadOld replaces the scenario's default store database with the newest index fixture and makes
// kb a probe build, so the file is what the previous kb release left. It returns the fixture's
// version, and false when the helper is not available.
func (h *H) loadOld() (int, bool) {
	if loadTool == "" {
		h.note("skipped: no loadfixture helper (the harness was started with -bin)")
		return 0, false
	}
	v := 0
	files, _ := filepath.Glob(filepath.Join(fixtureDir, "index-v*.sql"))
	for _, f := range files {
		var n int
		if _, err := fmt.Sscanf(filepath.Base(f), "index-v%d.sql", &n); err == nil && n > v {
			v = n
		}
	}
	h.must(v > 0, "no index fixture in %s", fixtureDir)
	h.probe = true
	h.helper("load", h.dbPath(), filepath.Join(fixtureDir, fmt.Sprintf("index-v%d.sql", v)))
	return v, true
}

// helper runs loadfixture and aborts the scenario when it fails.
func (h *H) helper(args ...string) string {
	c := exec.CommandContext(h.ctx, loadTool, args...)
	var o, e bytes.Buffer
	c.Stdout, c.Stderr = &o, &e
	h.must(c.Run() == nil, "loadfixture %s: %s", strings.Join(args, " "), strings.TrimSpace(e.String()))
	return strings.TrimSpace(o.String())
}

func (h *H) sum(path string) string {
	b, err := os.ReadFile(path)
	h.must(err == nil, "read %s: %v", path, err)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

// backups lists the backup files beside the default store's database.
func (h *H) backups() []string {
	m, _ := filepath.Glob(h.dbPath() + ".bak-v*")
	return m
}

func scMigrateRefusal(h *H) {
	old, ok := h.loadOld()
	if !ok {
		return
	}
	db := h.dbPath()
	before := h.sum(db)

	// Every command that opens the index refuses it, names the fix, and writes nothing (H37: the
	// entry commands and a NAME@LOC ref too; edit is a unit test, it would start an editor).
	entryFile := h.write("data/mig.md", entryText("Mig", "## A\n\nmigword body\n"))
	entryBytes, _ := os.ReadFile(entryFile)
	for _, args := range [][]string{
		{"search", "anything", "--mode", "fts"}, {"reindex", "--all"}, {"stats", "overview"},
		{"feedback", "1", "1", "--useful"}, {"explain", "anything"},
		{"rm", "mig.md", "--yes"}, {"add", entryFile}, {"reindex", "mig.md"},
	} {
		r := h.kb(args...)
		if h.expect(r, 1) && !strings.Contains(r.Stderr, "kb migrate up") {
			h.fail("an older index: the refusal does not say 'kb migrate up':\n%s", r)
		}
	}
	if r := h.kbAt(h.outside, "--store", "default@"+h.root, "search", "anything", "--mode", "fts"); h.expect(r, 1) && !strings.Contains(r.Stderr, "kb migrate up") {
		h.fail("an older index via NAME@LOC: the refusal does not say 'kb migrate up':\n%s", r)
	}
	if r := h.kbAt(h.outside, "--store", "default@"+h.root, "migrate", "status"); h.expect(r, 1) && !strings.Contains(r.Stdout, h.dbPath()) {
		h.fail("migrate status via NAME@LOC must list that store's index:\n%s", r)
	}
	if got, _ := os.ReadFile(entryFile); !bytes.Equal(got, entryBytes) {
		h.fail("a refused rm/add changed the entry file")
	}
	// Commands that read only the files (or the cache) work on an older index.
	for _, args := range [][]string{{"show", "mig.md"}, {"list"}, {"model", "cache", "--stats"}} {
		h.expect(h.kb(args...), 0)
	}
	// The exempt commands explain instead of failing the same way.
	if r := h.kb("doctor"); h.expect(r, 1) && !strings.Contains(r.Stdout, "FAIL migrate") {
		h.fail("doctor on an older index should report FAIL migrate:\n%s", r)
	}
	if r := h.kb("stores"); h.expect(r, 0) && !strings.Contains(r.Stdout, "kb migrate up") {
		h.fail("kb stores should show the migrate hint in the row:\n%s", r)
	}
	if r := h.kb("config", "--path"); h.expect(r, 0) {
		_ = r
	}
	if r := h.kb("migrate", "status"); h.expect(r, 1) && !strings.Contains(r.Stdout, "pending") {
		h.fail("migrate status should list the pending steps:\n%s", r)
	}
	if h.sum(db) != before || len(h.backups()) != 0 {
		h.fail("refusals, doctor, stores or migrate status changed the older index (or wrote a backup)")
	}
	if _, err := os.Stat(db + "-wal"); err == nil {
		h.fail("a refused open left a WAL file")
	}

	// --dry-run writes nothing either.
	if r := h.kb("migrate", "up", "--dry-run"); h.expect(r, 0) && !strings.Contains(r.Stdout, "would migrate") {
		h.fail("dry run should print the plan:\n%s", r)
	}
	if h.sum(db) != before || len(h.backups()) != 0 {
		h.fail("--dry-run changed the database or wrote a backup")
	}

	// migrate up: backup, the effect, and then everything works.
	r := h.kb("migrate", "up")
	if h.expect(r, 0) {
		for _, want := range []string{"backup: ", "needs: kb reindex"} {
			if !strings.Contains(r.Stdout, want) {
				h.fail("migrate up output lacks %q:\n%s", want, r)
			}
		}
	}
	baks := h.backups()
	if len(baks) != 1 {
		h.fail("want exactly one backup, got %v", baks)
	} else if v := h.helper("check", baks[0]); v != fmt.Sprintf("%d ok", old) {
		h.fail("the backup should be a valid v%d database, check says %q", old, v)
	}
	h.expect(h.kb("search", "anything", "--mode", "fts"), 0)
	h.expect(h.kb("migrate", "status"), 0)
	h.expect(h.kb("migrate", "verify"), 0)
	if r := h.kb("migrate", "up"); h.expect(r, 0) && (!strings.Contains(r.Stdout, "up to date") || len(h.backups()) != 1) {
		h.fail("a second migrate up should do nothing and write no second backup:\n%s", r)
	}

	// A newer file: refused everywhere, migrate up included, and never touched.
	h.helper("setver", db, "9999")
	newer := h.sum(db)
	for _, args := range [][]string{{"search", "x", "--mode", "fts"}, {"reindex", "--all"}, {"migrate", "up"}, {"migrate", "up", "--no-backup"}} {
		r := h.kb(args...)
		if h.expect(r, 1) && !strings.Contains(r.Stderr, "newer kb") {
			h.fail("a newer index: the refusal does not say 'newer kb':\n%s", r)
		}
	}
	h.expect(h.kb("migrate", "status"), 1)
	h.expect(h.kb("migrate", "verify"), 1)
	if h.sum(db) != newer || len(h.backups()) != 1 {
		h.fail("a newer index was touched or backed up")
	}
}

// bgKB starts kb in the scenario root without waiting.
func (h *H) bgKB(args ...string) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer) {
	c, o, e := h.cmd(args...)
	h.must(c.Start() == nil, "start kb %s", strings.Join(args, " "))
	return c, o, e
}

func scMigrateConcurrent(h *H) {
	if loadTool == "" {
		h.note("skipped: no loadfixture helper (the harness was started with -bin)")
		return
	}
	db := h.dbPath()
	for round := 1; round <= 6; round++ {
		h.loadOld()
		for _, b := range h.backups() {
			os.Remove(b)
		}
		type out struct {
			code   int
			stdout string
			stderr string
		}
		outs := make([]out, 3)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range outs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c, o, e := h.cmd("migrate", "up")
				<-start
				err := c.Run()
				outs[i] = out{exitCode(err), o.String(), e.String()}
			}()
		}
		close(start)
		wg.Wait()
		migrated := 0
		for i, o := range outs {
			h.invariants(res{Args: []string{"migrate", "up"}, Code: o.code, Stdout: o.stdout, Stderr: o.stderr})
			if o.code != 0 {
				h.fail("round %d: migrate up #%d exited %d (the loser must wait and find nothing to do)\nstderr: %s", round, i, o.code, clip(o.stderr, 300))
			}
			if strings.Contains(o.stdout, "migrated store default index") {
				migrated++
			}
		}
		if migrated != 1 {
			h.fail("round %d: %d runners migrated the store, want exactly 1", round, migrated)
		}
		if n := len(h.backups()); n != 1 {
			h.fail("round %d: %d backups, want 1: %v", round, n, h.backups())
		}
		h.expect(h.kb("migrate", "status"), 0)
		h.expect(h.kb("migrate", "verify"), 0)
		_ = db
	}
}

func scMigrateInterrupt(h *H) {
	old, ok := h.loadOld()
	if !ok {
		return
	}
	oldOK := fmt.Sprintf("%d ok", old)
	db := h.dbPath()
	// Big enough that the backup (VACUUM INTO) and the steps take a visible time.
	const filler = "600000"
	h.helper("fill", db, filler)
	big := filepath.Join(h.dir, "big-old.sqlite")
	h.must(copyFile(db, big) == nil, "keep a copy of the big fixture")

	for _, delay := range []time.Duration{0, 30 * time.Millisecond, 120 * time.Millisecond, 300 * time.Millisecond, 700 * time.Millisecond, 1500 * time.Millisecond} {
		h.must(copyFile(big, db) == nil, "restore the big fixture")
		for _, suffix := range []string{"-wal", "-shm"} {
			os.Remove(db + suffix)
		}
		for _, b := range h.backups() {
			os.Remove(b)
		}
		c, o, e := h.bgKB("migrate", "up")
		time.Sleep(delay)
		_ = c.Process.Signal(syscall.SIGINT)
		err := c.Wait()
		r := res{Args: []string{"migrate", "up"}, Code: exitCode(err), Stdout: o.String(), Stderr: e.String()}
		h.invariants(r)
		if r.Code != 0 && r.Code != 1 && r.Code != 130 {
			h.fail("delay %v: exit %d after SIGINT, want 0 (finished first), 1 or 130:\n%s", delay, r.Code, r)
		}
		// Whole old or whole new, never in between.
		v := h.helper("check", db)
		h.note("SIGINT after %v: exit %d, database %s, %d backup(s)", delay, r.Code, v, len(h.backups()))
		switch {
		case v == oldOK:
			if r.Code == 0 {
				h.fail("delay %v: exit 0 but the database is still v%d:\n%s", delay, old, r)
			}
		case strings.HasSuffix(v, " ok"):
			// done before the signal landed, or the apply committed: any later version
		default:
			h.fail("delay %v: the database is %q after an interrupted migrate (want %q or the target version)", delay, v, oldOK)
		}
		// Every backup that is left must be a complete database: a half-written one would be
		// kept by the pruning and trusted by the user.
		for _, b := range h.backups() {
			if got := h.helper("check", b); got != oldOK {
				h.fail("delay %v: backup %s is %q, want a valid v%d database", delay, filepath.Base(b), got, old)
			}
		}
		// A rerun finishes the job.
		if r := h.kb("migrate", "up"); !h.expect(r, 0) {
			continue
		}
		h.expect(h.kb("migrate", "status"), 0)
		h.expect(h.kb("migrate", "verify"), 0)
		h.expect(h.kb("search", "x", "--mode", "fts"), 0)
		os.Remove(db + "-wal")
	}
}

func copyFile(from, to string) error {
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, b, 0o644)
}
