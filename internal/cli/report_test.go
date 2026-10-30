package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/store/sqlite"
)

var incidentIDRe = regexp.MustCompile(`internal error \(id ([0-9a-f]{8})\)`)

// readIncidents returns the incident log lines under root/.kb, decoded.
func readIncidents(t *testing.T, path string) []incident {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []incident
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var inc incident
		if err := json.Unmarshal([]byte(line), &inc); err != nil {
			t.Fatalf("incident line %q: %v", line, err)
		}
		out = append(out, inc)
	}
	return out
}

// TestReportDecision is the branch table for report: one row per outcome (silent, interrupted,
// well-formed by kind, bug).
func TestReportDecision(t *testing.T) {
	canceledInternal := errs.Wrap(fmt.Errorf("query: %w", context.Canceled), errs.Internal, "sqlite.FTS", "full-text search failed")
	for _, tc := range []struct {
		name       string
		err        error
		wantCode   int
		wantStderr string // exact; "" = nothing printed; "INCIDENT" = the internal-error line
	}{
		{"silent exit keeps its code", exitSilently(ExitEmbedder), ExitEmbedder, ""},
		{"usage", usageErr("bad --k %q", "x"), ExitUsage, "kb: bad --k \"x\"\n"},
		{"not found", errs.New(errs.NotFound, "t", "no such entry"), ExitUsage, "kb: no such entry\n"},
		{"conflict", errs.New(errs.Conflict, "t", "model mismatch"), ExitUsage, "kb: model mismatch\n"},
		{"unavailable", embedderErr("model not downloaded"), ExitEmbedder, "kb: model not downloaded\n"},
		{"interrupted kind prints its message", errs.Wrap(context.Canceled, errs.Interrupted, "t", "interrupted (3 saved)"), ExitUsage, "kb: interrupted (3 saved)\n"},
		{"raw cancel is interrupted, not a bug", context.Canceled, ExitUsage, "kb: interrupted\n"},
		{"internal wrapping a cancel is interrupted", canceledInternal, ExitUsage, "kb: interrupted\n"},
		{"plain error is a bug", errors.New("nil map"), ExitInternal, "INCIDENT"},
		{"internal kind is a bug", errs.New(errs.Internal, "t", "unexpected"), ExitInternal, "INCIDENT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newTempRoot(t)
			if err := os.MkdirAll(filepath.Join(root, ".kb"), 0o755); err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			code := report(&stderr, testDeps, nil, []string{"x"}, tc.err)
			if code != tc.wantCode {
				t.Fatalf("code = %d, want %d", code, tc.wantCode)
			}
			incidents := readIncidents(t, filepath.Join(root, ".kb", "kb.log"))
			if tc.wantStderr != "INCIDENT" {
				if stderr.String() != tc.wantStderr {
					t.Errorf("stderr = %q, want %q", stderr.String(), tc.wantStderr)
				}
				if len(incidents) != 0 {
					t.Errorf("well-formed error wrote %d incidents", len(incidents))
				}
				return
			}
			m := incidentIDRe.FindStringSubmatch(stderr.String())
			if m == nil || !strings.Contains(stderr.String(), "kb.log") {
				t.Fatalf("stderr = %q, want the internal-error line naming kb.log", stderr.String())
			}
			if len(incidents) != 1 || incidents[0].ID != m[1] || incidents[0].Kind != "internal" ||
				len(incidents[0].Chain) == 0 || incidents[0].Args[0] != "x" || incidents[0].Version != Version {
				t.Fatalf("incidents = %+v, want one with id %s", incidents, m[1])
			}
		})
	}
}

// TestInternalErrorFromStoreExits3: a store failure nobody classified reaches the top as a bug:
// exit 3, an incident in <root>/.kb/kb.log, and --debug prints the chain.
func TestInternalErrorFromStoreExits3(t *testing.T) {
	root := newTempRoot(t)
	useNoneModel(t, root)
	orig := testDeps.openStore
	t.Cleanup(func() { testDeps.openStore = orig })
	testDeps.openStore = func(context.Context, string, bool) (store.Store, error) {
		return nil, errs.Wrap(errors.New("disk I/O error"), errs.Internal, "sqlite.Open", "could not open the database")
	}
	_, stderr, code := run(t, "reindex", "--all", "--debug")
	if code != ExitInternal || !incidentIDRe.MatchString(stderr) {
		t.Fatalf("exit %d, stderr %q; want 3 with an incident id", code, stderr)
	}
	if !strings.Contains(stderr, "sqlite.Open [internal] could not open the database") || !strings.Contains(stderr, "disk I/O error") {
		t.Errorf("--debug did not print the chain:\n%s", stderr)
	}
	if n := len(readIncidents(t, filepath.Join(root, ".kb", "kb.log"))); n != 1 {
		t.Errorf("%d incidents, want 1", n)
	}
}

// TestPanicExits3: the single recover turns any panic, an assertion or a runtime one, into an
// incident and exit 3 (never Go's exit 2).
func TestPanicExits3(t *testing.T) {
	for _, tc := range []struct {
		name     string
		value    any
		wantKind string
	}{
		{"runtime panic", "boom", "panic"},
		{"assertion", assert.Violation{Kind: "invariant", Msg: "f: broken"}, "assert:invariant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newTempRoot(t)
			useNoneModel(t, root)
			orig := testDeps.openStore
			t.Cleanup(func() { testDeps.openStore = orig })
			testDeps.openStore = func(context.Context, string, bool) (store.Store, error) { panic(tc.value) }
			_, stderr, code := run(t, "reindex", "--all")
			if code != ExitInternal || !incidentIDRe.MatchString(stderr) || strings.Contains(stderr, "goroutine") {
				t.Fatalf("exit %d, stderr %q; want 3, an incident id and no stack", code, stderr)
			}
			incidents := readIncidents(t, filepath.Join(root, ".kb", "kb.log"))
			if len(incidents) != 1 {
				t.Fatalf("%d incidents, want 1", len(incidents))
			}
			if last := incidents[0]; last.Kind != tc.wantKind || !strings.Contains(last.Stack, "goroutine") {
				t.Errorf("incident = %+v, want kind %s with a stack", last, tc.wantKind)
			}
		})
	}
}

// TestCobraErrorsAreUsage: errors cobra produces before a command runs are usage errors (exit 1)
// prefixed with the command, not bugs.
func TestCobraErrorsAreUsage(t *testing.T) {
	newTempRoot(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"search", "x", "--no-such-flag"}, "kb search: unknown flag: --no-such-flag"},
		{[]string{"no-such-command"}, "kb: unknown command"},
		{[]string{"show"}, "kb show: accepts 1 arg(s)"},
	} {
		_, stderr, code := run(t, tc.args...)
		if code != ExitUsage || !strings.HasPrefix(stderr, tc.want) {
			t.Errorf("%v: exit %d stderr %q, want 1 and %q", tc.args, code, stderr, tc.want)
		}
	}
}

// TestAppendTrimmed: the log is cut back to the last max bytes at a line boundary once it grows
// past max, and left alone at or below it.
func TestAppendTrimmed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "kb.log")
	for i := 0; i < 5; i++ {
		if err := appendTrimmed(path, []byte(fmt.Sprintf("line%d\n", i)), 18); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(path)
	if string(data) != "line2\nline3\nline4\n" {
		t.Errorf("log = %q, want the last whole lines within 18 bytes", data)
	}
	mid := filepath.Join(t.TempDir(), "kb.log")
	for _, l := range []string{"aaaaaaaaaa\n", "bbbbbbbbbb\n"} {
		if err := appendTrimmed(mid, []byte(l), 15); err != nil {
			t.Fatal(err)
		}
	}
	if data, _ := os.ReadFile(mid); string(data) != "bbbbbbbbbb\n" {
		t.Errorf("mid-line cut = %q, want the partial line dropped", data)
	}
	exact := filepath.Join(t.TempDir(), "kb.log")
	for i := 0; i < 3; i++ {
		if err := appendTrimmed(exact, []byte("abcde\n"), 18); err != nil {
			t.Fatal(err)
		}
	}
	if data, _ := os.ReadFile(exact); len(data) != 18 {
		t.Errorf("log at exactly max = %q, want untouched", data)
	}
	// The cut falls exactly on a line boundary: nothing more is dropped.
	boundary := filepath.Join(t.TempDir(), "kb.log")
	for _, l := range []string{"aaaaa\n", "bbbbb\n", "ccccc\n"} {
		if err := appendTrimmed(boundary, []byte(l), 12); err != nil {
			t.Fatal(err)
		}
	}
	if data, _ := os.ReadFile(boundary); string(data) != "bbbbb\nccccc\n" {
		t.Errorf("cut on a line boundary = %q, want the last two whole lines", data)
	}
	// The cut falls on a newline byte (one character into the partial line's end).
	onNL := filepath.Join(t.TempDir(), "kb.log")
	for _, l := range []string{"aaaaa\n", "bbbbb\n"} {
		if err := appendTrimmed(onNL, []byte(l), 7); err != nil {
			t.Fatal(err)
		}
	}
	if data, _ := os.ReadFile(onNL); string(data) != "bbbbb\n" {
		t.Errorf("cut on a newline byte = %q, want the last line", data)
	}
	// Errors: the log's directory cannot be created.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := appendTrimmed(filepath.Join(blocker, "kb", "kb.log"), []byte("x\n"), 10); err == nil {
		t.Error("appendTrimmed under a regular file = nil error")
	}
	// The log path is a directory: it cannot be opened for append.
	if err := appendTrimmed(t.TempDir(), []byte("x\n"), 10); err == nil {
		t.Error("appendTrimmed on a directory = nil error")
	}
}

// TestExitCode pins the CLI contract.
func TestExitCode(t *testing.T) {
	want := map[errs.Kind]int{errs.Usage: 1, errs.NotFound: 1, errs.Conflict: 1, errs.Interrupted: 1, errs.Unavailable: 2, errs.Internal: 3}
	for k, code := range want {
		if got := exitCode(k); got != code {
			t.Errorf("exitCode(%v) = %d, want %d", k, got, code)
		}
	}
}

// TestIncidentWithoutStoreGoesToCacheDir: with no <root>/.kb the incident goes to the user cache
// dir, and kb never creates a store directory just to log.
func TestIncidentWithoutStoreGoesToCacheDir(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir()) // a directory that is not a store
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	cache := t.TempDir()
	orig := testDeps.userCacheDir
	t.Cleanup(func() { testDeps.userCacheDir = orig })
	testDeps.userCacheDir = func() (string, error) { return cache, nil }
	var stderr bytes.Buffer
	if code := report(&stderr, testDeps, nil, nil, errors.New("bug")); code != ExitInternal {
		t.Fatalf("code = %d", code)
	}
	if n := len(readIncidents(t, filepath.Join(cache, "kb", "kb.log"))); n != 1 {
		t.Errorf("%d incidents in the cache dir, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(root, ".kb")); !os.IsNotExist(err) {
		t.Errorf("report created %s/.kb", root)
	}
	testDeps.userCacheDir = func() (string, error) { return "", errors.New("no home") }
	stderr.Reset()
	if code := report(&stderr, testDeps, nil, nil, errors.New("bug")); code != ExitInternal || !strings.Contains(stderr.String(), "unavailable (no home)") {
		t.Errorf("unwritable log: code %d stderr %q", code, stderr.String())
	}
}

// TestReportPanicDebugAndNoCmd: with --debug the panic value and its stack follow the incident
// line; with no command (a panic before cobra picked one) the prefix is plain "kb"; and an
// incident log that cannot be written still gives the user an id.
func TestReportPanicDebugAndNoCmd(t *testing.T) {
	root := newTempRoot(t)
	useNoneModel(t, root)
	orig := testDeps.openStore
	t.Cleanup(func() { testDeps.openStore = orig })
	testDeps.openStore = func(context.Context, string, bool) (store.Store, error) { panic("boom") }
	_, stderr, code := run(t, "reindex", "--all", "--debug")
	if code != ExitInternal || !strings.Contains(stderr, "boom") || !strings.Contains(stderr, "goroutine") {
		t.Fatalf("exit %d, stderr %q; want 3 with the panic value and stack", code, stderr)
	}

	var buf bytes.Buffer
	if code := reportPanic(&buf, testDeps, nil, nil, "boom"); code != ExitInternal ||
		!strings.HasPrefix(buf.String(), "kb: internal error (id ") || strings.Contains(buf.String(), "goroutine") {
		t.Errorf("reportPanic(nil cmd) = %d, %q", code, buf.String())
	}

	// No <root>/.kb and no user cache dir: the log is unavailable, the id is still printed.
	if err := os.RemoveAll(filepath.Join(root, ".kb")); err != nil {
		t.Fatal(err)
	}
	origCache := testDeps.userCacheDir
	t.Cleanup(func() { testDeps.userCacheDir = origCache })
	testDeps.userCacheDir = func() (string, error) { return "", errors.New("no cache dir") }
	buf.Reset()
	if code := report(&buf, testDeps, nil, nil, errors.New("bug")); code != ExitInternal ||
		!incidentIDRe.MatchString(buf.String()) || !strings.Contains(buf.String(), "unavailable (no cache dir)") {
		t.Errorf("report with no log = %d, %q", code, buf.String())
	}
	// A cache dir that is a regular file: the log cannot be written, the id is still printed.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	testDeps.userCacheDir = func() (string, error) { return blocker, nil }
	buf.Reset()
	if code := report(&buf, testDeps, nil, nil, errors.New("bug")); code != ExitInternal ||
		!incidentIDRe.MatchString(buf.String()) || !strings.Contains(buf.String(), "Details: unavailable (") {
		t.Errorf("report with an unwritable log = %d, %q", code, buf.String())
	}
}

// TestAppendTrimmedBoundaryAtIncidentLogMax: at the real 1 MiB limit, a log that ends exactly at
// max or below is left alone; one byte above is cut back to whole lines within max, newest kept.
func TestAppendTrimmedBoundaryAtIncidentLogMax(t *testing.T) {
	const lineLen = 100
	mk := func(c byte, n int) []byte { return append(bytes.Repeat([]byte{c}, n-1), '\n') }
	newest := mk('n', lineLen)
	for _, tc := range []struct {
		name     string
		existing int // bytes already in the log; lines of lineLen, the last one possibly shorter
		trimmed  bool
	}{
		{"below max", incidentLogMax - lineLen - 1, false},
		{"exactly max", incidentLogMax - lineLen, false},
		{"one byte over", incidentLogMax - lineLen + 1, true},
		{"far over", 2 * incidentLogMax, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "kb.log")
			var pre []byte
			for len(pre) < tc.existing {
				n := min(lineLen, tc.existing-len(pre))
				if n < 2 { // a 1-byte line would be just "\n"; fold it into the previous line
					pre = append(pre, '\n')
					break
				}
				pre = append(pre, mk('o', n)...)
			}
			if err := os.WriteFile(path, pre, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := appendTrimmed(path, newest, incidentLogMax); err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(path)
			if !bytes.HasSuffix(data, newest) {
				t.Fatal("the newest line is not at the end")
			}
			if tc.trimmed {
				if len(data) > incidentLogMax || len(data) < incidentLogMax-lineLen {
					t.Errorf("trimmed size = %d, want within one line below %d", len(data), incidentLogMax)
				}
				// Oracle: keep the longest run of whole trailing lines that fits in max.
				lines := bytes.SplitAfter(append(append([]byte{}, pre...), newest...), []byte("\n"))
				var want []byte
				for i := len(lines) - 1; i >= 0; i-- {
					if len(want)+len(lines[i]) > incidentLogMax {
						break
					}
					want = append(append([]byte{}, lines[i]...), want...)
				}
				if !bytes.Equal(data, want) {
					t.Errorf("trimmed log (%d bytes) differs from the whole-trailing-lines oracle (%d bytes)", len(data), len(want))
				}
			} else if len(data) != len(pre)+len(newest) {
				t.Errorf("size = %d, want %d (untouched)", len(data), len(pre)+len(newest))
			}
			if left, _ := filepath.Glob(path + ".tmp*"); len(left) != 0 {
				t.Errorf("temp files left behind: %v", left)
			}
		})
	}
}

// TestAppendTrimmedConcurrentWriters: many goroutines logging at once (with a small max so trims
// race with appends) never lose or tear a line: every line of the final log is one whole line and
// without trimming (large max) all of them are there.
func TestAppendTrimmedConcurrentWriters(t *testing.T) {
	const writers, perWriter = 8, 50
	path := filepath.Join(t.TempDir(), "kb.log")
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if err := appendTrimmed(path, []byte(fmt.Sprintf("w%d-%03d\n", w, i)), incidentLogMax); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	checkLines(t, path, writers*perWriter)

	// Trimming under contention: the survivors are whole, well-formed lines, the cap holds, and
	// each writer's own lines survive in order (a trim never reorders).
	trim := filepath.Join(t.TempDir(), "kb.log")
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if err := appendTrimmed(trim, []byte(fmt.Sprintf("w%d-%03d\n", w, i)), 200); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	data, _ := os.ReadFile(trim)
	if len(data) > 200 {
		t.Errorf("size %d over the cap 200", len(data))
	}
	last := map[string]string{}
	for _, l := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if len(l) != 6 || l[0] != 'w' {
			t.Fatalf("torn line %q", l)
		}
		if prev, ok := last[l[:2]]; ok && prev >= l {
			t.Errorf("writer %s out of order: %s then %s", l[:2], prev, l)
		}
		last[l[:2]] = l
	}
}

// TestAppendTrimmedTwoProcesses: separate processes (this test binary re-run as a writer) append
// to one log at the same time; no line is lost.
func TestAppendTrimmedTwoProcesses(t *testing.T) {
	if p := os.Getenv("KB_TEST_LOG_WRITER"); p != "" {
		id := os.Getenv("KB_TEST_LOG_ID")
		for i := 0; i < 100; i++ {
			if err := appendTrimmed(p, []byte(fmt.Sprintf("p%s-%03d\n", id, i)), incidentLogMax); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	path := filepath.Join(t.TempDir(), "kb.log")
	var cmds []*exec.Cmd
	for _, id := range []string{"a", "b", "c"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestAppendTrimmedTwoProcesses$")
		cmd.Env = append(os.Environ(), "KB_TEST_LOG_WRITER="+path, "KB_TEST_LOG_ID="+id)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
	}
	for _, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	checkLines(t, path, 300)
}

// checkLines asserts path holds exactly want distinct, whole lines.
func checkLines(t *testing.T, path string, want int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		seen[l] = true
	}
	if len(seen) != want || strings.Count(string(data), "\n") != want {
		t.Errorf("log has %d distinct lines (%d newlines), want %d", len(seen), strings.Count(string(data), "\n"), want)
	}
}

// TestReplaceFileIsAtomicRename: the trim goes through a temp file and rename, so the log
// path never holds a truncated file (the rename target is replaced whole).
func TestReplaceFileIsAtomicRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kb.log")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	if err := replaceFile(path, []byte("new\n")); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if data, _ := os.ReadFile(path); string(data) != "new\n" || os.SameFile(before, after) {
		t.Errorf("log = %q, same inode %v; want new content in a renamed-in file", data, os.SameFile(before, after))
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644", fi.Mode().Perm())
	}
	if err := replaceFile(filepath.Join(dir, "missing", "kb.log"), []byte("x")); err == nil {
		t.Error("replaceFile into a missing dir = nil error")
	}
}

// TestDiskFullIsExit1: a real SQLITE_FULL (the page limit is pinned at the current size and the
// feedback note needs new pages) reaches report as a well-formed failure: exit 1 with the "free
// space and retry" message and no incident, not exit 3. Feedback is the probe because it writes
// only plain tables; a full disk met inside a sqlite-vec insert is reported by that extension as a
// generic error and stays Internal (queue Q47).
func TestDiskFullIsExit1(t *testing.T) {
	root := newSearchTestRepo(t, testModel768, testDim768)
	useRoot(t, root)
	id := searchID(t, "widget")
	orig := testDeps.openStore
	t.Cleanup(func() { testDeps.openStore = orig })
	testDeps.openStore = func(ctx context.Context, path string, readOnly bool) (store.Store, error) {
		st, err := sqlite.Open(ctx, path, sqlite.Options{ReadOnly: readOnly})
		if err != nil || readOnly {
			return st, err
		}
		st.DB().SetMaxOpenConns(1) // max_page_count is per connection
		var pages int
		if err := st.DB().QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
			return nil, err
		}
		if _, err := st.DB().Exec(fmt.Sprintf("PRAGMA max_page_count = %d", pages)); err != nil {
			return nil, err
		}
		return st, nil
	}
	_, stderr, code := runKB(t, []string{"feedback", id, "1", "--useful", "--note", strings.Repeat("x", 256<<10)})
	if code != ExitUsage || !strings.Contains(stderr, "disk full or unwritable") || !strings.Contains(stderr, "free space and retry") ||
		strings.Contains(stderr, "internal error") {
		t.Fatalf("exit %d, stderr %q; want 1 with the disk-full message and no incident", code, stderr)
	}
	if inc := readIncidents(t, filepath.Join(root, ".kb", "kb.log")); len(inc) != 0 {
		t.Errorf("incidents = %+v, want none", inc)
	}
}

// TestIncidentLogPathDecision is the branch table for incidentLogPath (carried from W3 into 1.1):
// an incident lands in the store the command ran on, chosen the way the command chose it, and
// falls back to the user cache dir when no store with a .kb directory resolves.
func TestIncidentLogPathDecision(t *testing.T) {
	discovered := newTempRoot(t) // cwd; has .kb/config.toml
	named := realTempDir(t)
	writeFile(t, filepath.Join(named, ".kb", "config.toml"), "")
	bare := filepath.Join(realTempDir(t), "no-kb-yet") // --root that names a store not created yet
	home := testDeps.workspaceEnv().Home
	globalKB := filepath.Join(home, ".local", "share", "kb", ".kb")
	cache, err := testDeps.userCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	cacheLog := filepath.Join(cache, "kb", "kb.log")
	outside := realTempDir(t)

	for _, tc := range []struct {
		name   string
		cwd    string
		args   []string // root flags; nil cmd when noCmd
		noCmd  bool
		global bool // create the global store's .kb first
		want   string
	}{
		{name: "discovered store", cwd: discovered, want: filepath.Join(discovered, ".kb", "kb.log")},
		{name: "--root store wins over the discovered one", cwd: discovered, args: []string{"--root", named}, want: filepath.Join(named, ".kb", "kb.log")},
		{name: "--root without .kb goes to the cache", cwd: discovered, args: []string{"--root", bare}, want: cacheLog},
		{name: "--global store", cwd: discovered, args: []string{"--global"}, global: true, want: filepath.Join(globalKB, "kb.log")},
		{name: "--global without .kb goes to the cache", cwd: discovered, args: []string{"--global"}, want: cacheLog},
		{name: "--root with --global (unresolvable) goes to the cache", cwd: discovered, args: []string{"--root", named, "--global"}, want: cacheLog},
		{name: "no store found goes to the cache", cwd: outside, want: cacheLog},
		{name: "no command: discovery from cwd", cwd: discovered, noCmd: true, want: filepath.Join(discovered, ".kb", "kb.log")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(tc.cwd)
			if tc.global {
				if err := os.MkdirAll(globalKB, 0o755); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.RemoveAll(filepath.Join(home, ".local")) })
			}
			var cmd *cobra.Command
			if !tc.noCmd {
				cmd = newRootCmd(testDeps, io.Discard, io.Discard)
				if err := cmd.ParseFlags(tc.args); err != nil {
					t.Fatal(err)
				}
			}
			got, err := incidentLogPath(testDeps, cmd)
			if err != nil || got != tc.want {
				t.Fatalf("incidentLogPath = %q, %v; want %q", got, err, tc.want)
			}
			if _, err := os.Stat(filepath.Join(bare, ".kb")); err == nil {
				t.Fatal("incidentLogPath created a store directory")
			}
		})
	}
}
