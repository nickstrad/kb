// Command mutate is kb's mutation tester: it changes one thing in a package's source at a time
// (flips a comparison, nudges a constant, drops an error check, ...) and runs the package's tests
// against each change. A change the tests still pass on (a "survivor") marks an assertion, table
// row or boundary the tests are missing. See ADVERSARIAL_TESTING.md and the README beside this file.
//
// It never writes to the live tree: every mutant is written into a snapshot made by
// .agent/scripts/sandbox.sh under .agent/sandbox/, removed at the end unless -keep is given.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Status of one mutant.
const (
	stKilled   = "killed"
	stSurvived = "survived"
	stTimeout  = "timeout"
	stCompile  = "compile-error"
	stSkipped  = "not-run"
)

type result struct {
	ID       int    `json:"id"`
	File     string `json:"file"` // repo-relative
	Line     int    `json:"line"`
	Col      int    `json:"col"`
	Func     string `json:"func,omitempty"`
	Op       string `json:"op"`
	Before   string `json:"before"`
	After    string `json:"after"`
	Status   string `json:"status"`
	KilledBy string `json:"killed_by,omitempty"`
	Millis   int64  `json:"ms"`
	fileIdx  int    // index into the file's mutation list
}

type summary struct {
	Package  string   `json:"package"`
	Total    int      `json:"total"`
	Killed   int      `json:"killed"`
	Survived int      `json:"survived"`
	Timeout  int      `json:"timeout"`
	Compile  int      `json:"compile_error"`
	NotRun   int      `json:"not_run"`
	Score    float64  `json:"score"` // (killed+timeout)/(killed+timeout+survived)
	Baseline string   `json:"baseline"`
	Results  []result `json:"results"`
}

type config struct {
	src, pkg, tags, diffRef string
	funcRe                  *regexp.Regexp
	ops                     map[string]bool
	max, jobs               int
	timeout                 time.Duration
	asJSON, list, keep      bool
}

func main() {
	var c config
	var funcPat, opsFlag string
	flag.StringVar(&c.pkg, "pkg", "", "package directory to mutate, relative to the repo root (e.g. ./internal/stats)")
	flag.StringVar(&funcPat, "func", "", "only mutate inside functions whose name (Recv.Name for methods) matches this regexp")
	flag.StringVar(&opsFlag, "ops", "all", "comma-separated operators: "+strings.Join(allOps, ","))
	flag.IntVar(&c.max, "max", 0, "run at most this many mutants (0 = all), sampled evenly across the list")
	flag.DurationVar(&c.timeout, "timeout", 0, "per-mutant test timeout (0 = 3x baseline + 15s)")
	flag.IntVar(&c.jobs, "j", 2, "parallel workers, each in its own sandbox")
	flag.StringVar(&c.diffRef, "diff", "", "only mutate lines changed against this git ref (plus untracked files)")
	flag.StringVar(&c.tags, "tags", "fts5", "build tags for go test")
	flag.StringVar(&c.src, "src", ".", "repository root (only read; snapshots are taken from it)")
	flag.BoolVar(&c.asJSON, "json", false, "print the summary as JSON")
	flag.BoolVar(&c.list, "list", false, "list the mutants and exit without running anything")
	flag.BoolVar(&c.keep, "keep", false, "keep the sandboxes for inspection")
	flag.Parse()

	if c.pkg == "" {
		fmt.Fprintln(os.Stderr, "mutate: -pkg is required")
		flag.Usage()
		os.Exit(2)
	}
	var err error
	if c.ops, err = parseOps(opsFlag); err != nil {
		fatal(err)
	}
	if funcPat != "" {
		if c.funcRe, err = regexp.Compile(funcPat); err != nil {
			fatal(fmt.Errorf("-func: %w", err))
		}
	}
	if c.jobs < 1 {
		c.jobs = 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sum, err := run(ctx, c, os.Stderr)
	if err != nil {
		fatal(err)
	}
	if c.asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(sum) // stdout write failures have nowhere better to go
	} else {
		printReport(os.Stdout, sum, c.list)
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "mutate: %v\n", err)
	os.Exit(2)
}

// run enumerates the mutants of c.pkg and, unless c.list, tests each one in a sandbox.
func run(ctx context.Context, c config, logw io.Writer) (*summary, error) {
	src, err := gitTop(c.src)
	if err != nil {
		return nil, err
	}
	rel := filepath.Clean(strings.TrimPrefix(c.pkg, "./"))
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("-pkg %s must be relative to the repo root", c.pkg)
	}
	files, err := packageFiles(ctx, src, rel, c.tags)
	if err != nil {
		return nil, err
	}
	var lines map[string]map[int]bool
	if c.diffRef != "" {
		if lines, err = changedLines(ctx, src, rel, c.diffRef); err != nil {
			return nil, err
		}
	}

	// Enumerate against the live tree, read-only.
	var all []result
	for _, f := range files {
		fl := (map[int]bool)(nil)
		if lines != nil {
			if fl = lines[f]; fl == nil {
				continue
			}
		}
		muts, err := parseAndEnumerate(filepath.Join(src, f), c, fl)
		if err != nil {
			return nil, err
		}
		for i, m := range muts {
			all = append(all, result{File: f, Line: m.Pos.Line, Col: m.Pos.Column, Func: m.Func,
				Op: m.Op, Before: m.Before, After: m.After, Status: stSkipped, fileIdx: i})
		}
	}
	all = sample(all, c.max)
	for i := range all {
		all[i].ID = i + 1
	}
	sum := &summary{Package: "./" + rel, Results: all}
	if c.list || len(all) == 0 {
		finish(sum)
		return sum, nil
	}

	boxes := make([]string, c.jobs)
	defer func() {
		for _, b := range boxes {
			if b == "" {
				continue
			}
			if c.keep {
				fmt.Fprintf(logw, "mutate: kept %s\n", b)
				continue
			}
			if err := sandbox(src, "rm", b); err != nil {
				fmt.Fprintf(logw, "mutate: %v\n", err)
			}
		}
	}()
	for i := range boxes {
		out, err := sandboxOut(src, "new", fmt.Sprintf("mutate-%s-w%d", filepath.Base(rel), i+1))
		if err != nil {
			return nil, err
		}
		boxes[i] = out
	}

	// Baseline: the unmutated package must pass, and its time sets the default timeout.
	fmt.Fprintf(logw, "mutate: %d mutants in %s; baseline...\n", len(all), sum.Package)
	t0 := time.Now()
	st, out, _ := goTest(ctx, boxes[0], rel, c.tags, 10*time.Minute)
	base := time.Since(t0)
	if st != stSurvived {
		return nil, fmt.Errorf("baseline tests do not pass in the sandbox (%s):\n%s", st, tail(out, 40))
	}
	sum.Baseline = base.Round(time.Millisecond).String()
	if c.timeout == 0 {
		c.timeout = 3*base + 15*time.Second
	}

	jobs := make(chan int)
	var mu sync.Mutex
	done := 0
	var wg sync.WaitGroup
	for w := range c.jobs {
		wg.Add(1)
		go func(box string) {
			defer wg.Done()
			cache := map[string]*parsedFile{}
			for i := range jobs {
				r := &all[i]
				var fl map[int]bool // nil: every line, as in the enumeration above
				if lines != nil {
					fl = lines[r.File]
				}
				status, by, ms, err := runMutant(ctx, box, rel, r, c, cache, fl)
				mu.Lock()
				if err != nil {
					fmt.Fprintf(logw, "mutate: #%d: %v\n", r.ID, err)
				} else {
					r.Status, r.KilledBy, r.Millis = status, by, ms
				}
				done++
				if status == stSurvived {
					fmt.Fprintf(logw, "  [%d/%d] SURVIVED %s:%d %s %s -> %s\n", done, len(all), r.File, r.Line, r.Op, r.Before, r.After)
				}
				mu.Unlock()
			}
		}(boxes[w])
	}
	for i := range all {
		if ctx.Err() != nil {
			break
		}
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	finish(sum)
	return sum, nil
}

type parsedFile struct {
	fset *token.FileSet
	path string
	orig []byte
	muts []*mutation
	file *ast.File
}

// runMutant writes mutant r into box, runs the tests and restores the file. lineFilter must be
// the same filter the enumeration used (the -diff lines of r.File, or nil): r.fileIdx indexes the
// filtered list, so re-enumerating the sandbox copy without it would apply a different mutant.
func runMutant(ctx context.Context, box, rel string, r *result, c config, cache map[string]*parsedFile, lineFilter map[int]bool) (string, string, int64, error) {
	pf := cache[r.File]
	if pf == nil {
		path := filepath.Join(box, r.File)
		if err := insideBox(box, path); err != nil {
			return "", "", 0, err
		}
		orig, err := os.ReadFile(path)
		if err != nil {
			return "", "", 0, err
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, orig, parser.ParseComments)
		if err != nil {
			return "", "", 0, err
		}
		pf = &parsedFile{fset: fset, path: path, orig: orig, file: f}
		pf.muts = enumerate(fset, f, c.ops, c.funcRe, lineFilter)
		cache[r.File] = pf
	}
	m, err := pickMutant(pf.muts, r)
	if err != nil {
		return "", "", 0, err
	}
	var buf bytes.Buffer
	m.apply()
	err = printer.Fprint(&buf, pf.fset, pf.file)
	m.undo()
	if err != nil {
		return "", "", 0, err
	}
	if err := os.WriteFile(pf.path, buf.Bytes(), 0o644); err != nil {
		return "", "", 0, err
	}
	defer os.WriteFile(pf.path, pf.orig, 0o644) // restore; the sandbox is discarded anyway
	t0 := time.Now()
	st, _, by := goTest(ctx, box, rel, c.tags, c.timeout)
	return st, by, time.Since(t0).Milliseconds(), nil
}

// pickMutant returns the sandbox copy's mutant for r and checks that it is the one r describes
// (line, op, before, after), so a mismatch between the two enumerations is an error rather than a
// silently mislabelled result.
func pickMutant(muts []*mutation, r *result) (*mutation, error) {
	if r.fileIdx >= len(muts) {
		return nil, fmt.Errorf("mutant index %d out of range in sandbox copy of %s", r.fileIdx, r.File)
	}
	m := muts[r.fileIdx]
	if m.Pos.Line != r.Line || m.Op != r.Op || m.Before != r.Before || m.After != r.After {
		return nil, fmt.Errorf("sandbox copy of %s enumerates %s:%d %s %s -> %s at index %d, want line %d %s %s -> %s",
			r.File, r.File, m.Pos.Line, m.Op, m.Before, m.After, r.fileIdx, r.Line, r.Op, r.Before, r.After)
	}
	return m, nil
}

var failRe = regexp.MustCompile(`(?m)^\s*--- FAIL: (\S+)`)

// goTest runs the package's tests in dir and classifies the outcome. "survived" means they passed.
func goTest(ctx context.Context, dir, rel, tags string, timeout time.Duration) (status, output, killedBy string) {
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := []string{"test", "-count=1", "-vet=off", "-failfast", "-timeout", (timeout + 5*time.Second).String()}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, "./"+rel+"/")
	cmd := exec.CommandContext(tctx, "go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOPROXY=off", "GOWORK=off", "GOFLAGS=-mod=mod")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	out := b.String()
	switch {
	case tctx.Err() == context.DeadlineExceeded || strings.Contains(out, "panic: test timed out"):
		return stTimeout, out, ""
	case err == nil:
		return stSurvived, out, ""
	case strings.Contains(out, "[build failed]") || strings.Contains(out, "[setup failed]"):
		return stCompile, out, ""
	}
	if m := failRe.FindStringSubmatch(out); m != nil {
		return stKilled, out, m[1]
	}
	if strings.Contains(out, "panic:") {
		return stKilled, out, "panic"
	}
	return stKilled, out, "exit status"
}

func parseAndEnumerate(path string, c config, lines map[int]bool) ([]*mutation, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	return enumerate(fset, f, c.ops, c.funcRe, lines), nil
}

// sample keeps at most max mutants, spread evenly so every file and operator is represented.
func sample(all []result, max int) []result {
	if max <= 0 || len(all) <= max {
		return all
	}
	out := make([]result, 0, max)
	step := float64(len(all)) / float64(max)
	for i := range max {
		out = append(out, all[int(float64(i)*step)])
	}
	return out
}

func finish(s *summary) {
	sort.SliceStable(s.Results, func(i, j int) bool {
		a, b := s.Results[i], s.Results[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.ID < b.ID
	})
	s.Total = len(s.Results)
	for _, r := range s.Results {
		switch r.Status {
		case stKilled:
			s.Killed++
		case stSurvived:
			s.Survived++
		case stTimeout:
			s.Timeout++
		case stCompile:
			s.Compile++
		default:
			s.NotRun++
		}
	}
	if d := s.Killed + s.Timeout + s.Survived; d > 0 {
		s.Score = float64(s.Killed+s.Timeout) / float64(d)
	}
}

func printReport(w io.Writer, s *summary, listOnly bool) {
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	if listOnly {
		for _, r := range s.Results {
			fmt.Fprintf(bw, "%4d %s:%d:%d %-11s %s  %s -> %s\n", r.ID, r.File, r.Line, r.Col, r.Op, r.Func, r.Before, r.After)
		}
		fmt.Fprintf(bw, "%d mutants in %s\n", s.Total, s.Package)
		return
	}
	fmt.Fprintf(bw, "SURVIVORS (each is a test gap unless the mutant is equivalent)\n")
	for _, r := range s.Results {
		if r.Status == stSurvived {
			fmt.Fprintf(bw, "  %s:%d  %-11s %-24s %s -> %s\n", r.File, r.Line, r.Op, r.Func, r.Before, r.After)
		}
	}
	if s.Timeout > 0 {
		fmt.Fprintf(bw, "TIMEOUTS (counted as killed; check for a hang the tests should name)\n")
		for _, r := range s.Results {
			if r.Status == stTimeout {
				fmt.Fprintf(bw, "  %s:%d  %-11s %s -> %s\n", r.File, r.Line, r.Op, r.Before, r.After)
			}
		}
	}
	fmt.Fprintf(bw, "\n%s: %d mutants, %d killed, %d survived, %d timeout, %d compile-error, %d not run; baseline %s\n",
		s.Package, s.Total, s.Killed, s.Survived, s.Timeout, s.Compile, s.NotRun, s.Baseline)
	fmt.Fprintf(bw, "mutation score %.1f%% = (killed+timeout)/(killed+timeout+survived)\n", 100*s.Score)
}

// --- repository and sandbox helpers (read-only on the live tree) ---

func gitTop(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("%s is not inside a git repository: %w", dir, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// packageFiles lists the package's non-test Go files that the build tags compile, repo-relative.
func packageFiles(ctx context.Context, src, rel, tags string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "go", "list", "-tags", tags, "-f", `{{range .GoFiles}}{{.}} {{end}}{{range .CgoFiles}}{{.}} {{end}}`, "./"+rel+"/")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "GOPROXY=off", "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("go list ./%s: %s", rel, ee.Stderr)
		}
		return nil, err
	}
	var files []string
	for _, f := range strings.Fields(string(out)) {
		files = append(files, filepath.Join(rel, f))
	}
	sort.Strings(files)
	return files, nil
}

var hunkRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// changedLines maps repo-relative file -> new-side line numbers changed against ref (git diff -U0),
// with every line of untracked files.
func changedLines(ctx context.Context, src, rel, ref string) (map[string]map[int]bool, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", "-U0", "--no-color", ref, "--", rel)
	cmd.Dir = src
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff %s: %w", ref, err)
	}
	res := map[string]map[int]bool{}
	var cur string
	for _, ln := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(ln, "+++ "):
			cur = strings.TrimPrefix(strings.TrimPrefix(ln, "+++ "), "b/")
		case strings.HasPrefix(ln, "@@"):
			m := hunkRe.FindStringSubmatch(ln)
			if m == nil || cur == "" {
				continue
			}
			start, _ := strconv.Atoi(m[1])
			n := 1
			if m[2] != "" {
				n, _ = strconv.Atoi(m[2])
			}
			if res[cur] == nil {
				res[cur] = map[int]bool{}
			}
			for l := start; l < start+n; l++ {
				res[cur][l] = true
			}
		}
	}
	cmd = exec.CommandContext(ctx, "git", "ls-files", "-o", "--exclude-standard", "--", rel)
	cmd.Dir = src
	if out, err = cmd.Output(); err != nil {
		return nil, err
	}
	for _, f := range strings.Fields(string(out)) {
		b, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			continue
		}
		res[f] = map[int]bool{}
		for l := 1; l <= bytes.Count(b, []byte("\n"))+1; l++ {
			res[f][l] = true
		}
	}
	return res, nil
}

func sandbox(src string, args ...string) error {
	_, err := sandboxOut(src, args...)
	return err
}

// sandboxOut runs .agent/scripts/sandbox.sh and returns the last line it printed.
func sandboxOut(src string, args ...string) (string, error) {
	cmd := exec.Command("bash", append([]string{filepath.Join(src, ".agent", "scripts", "sandbox.sh")}, args...)...)
	cmd.Dir = src
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("sandbox.sh %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	last := lines[len(lines)-1]
	if args[0] == "new" {
		if !strings.Contains(last, string(filepath.Separator)+filepath.Join(".agent", "sandbox")+string(filepath.Separator)) {
			return "", fmt.Errorf("sandbox.sh returned %q, not a path under .agent/sandbox", last)
		}
	}
	return last, nil
}

// insideBox refuses any write target that is not inside the sandbox.
func insideBox(box, path string) error {
	if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(box)+string(filepath.Separator)) {
		return fmt.Errorf("refusing to write %s: outside sandbox %s", path, box)
	}
	return nil
}

func tail(s string, n int) string {
	l := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}
