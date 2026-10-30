// Command harness builds the kb binary once inside a sandbox and runs hostile scenarios against
// it: concurrent processes on one database, SIGINT mid-reindex, corrupted and newer-schema
// databases, Markdown and front-matter edge cases, hostile search queries, bad feedback, path
// traversal, the no-model fallback and the exit-code contract. Each scenario gets fresh temp
// directories inside the run's sandbox; nothing touches ~/knowledge, the live tree or the network.
// See ADVERSARIAL_TESTING.md and the README beside this file.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// deadProxy is where every HTTP(S) request of a kb process goes: nothing listens there, so an
// attempted model download fails at once instead of reaching the network.
const deadProxy = "http://127.0.0.1:9"

// defaultModelDir is the cache directory name of local.DefaultModel (rembed's <org>--<name>).
const defaultModelDir = "BAAI--bge-small-en-v1.5"

type scenario struct {
	Name string
	Desc string
	Run  func(h *H)
}

type outcome struct {
	Name     string   `json:"name"`
	Desc     string   `json:"desc"`
	Pass     bool     `json:"pass"`
	Failures []string `json:"failures,omitempty"`
	Notes    []string `json:"notes,omitempty"`
	Millis   int64    `json:"ms"`
}

func main() { os.Exit(run()) }

// run is main with an exit code, so the sandbox cleanup (a defer) always runs: 0 all scenarios
// passed, 1 some failed, 2 the harness itself could not run.
func run() int {
	var (
		src, bin, only string
		keep, asJSON   bool
		listOnly       bool
	)
	flag.StringVar(&src, "src", ".", "repository root (only read; the sandbox is a snapshot of it)")
	flag.StringVar(&bin, "bin", "", "use this kb binary instead of building one in the sandbox")
	flag.StringVar(&only, "run", "", "only scenarios whose name matches this regexp")
	flag.BoolVar(&keep, "keep", false, "keep the sandbox (binary, temp roots) for inspection")
	flag.BoolVar(&asJSON, "json", false, "print results as JSON")
	flag.BoolVar(&listOnly, "list", false, "list scenarios and exit")
	flag.Parse()

	var re *regexp.Regexp
	if only != "" {
		var err error
		if re, err = regexp.Compile(only); err != nil {
			return fail(fmt.Errorf("-run: %w", err))
		}
	}
	var picked []scenario
	for _, s := range scenarios {
		if re == nil || re.MatchString(s.Name) {
			picked = append(picked, s)
		}
	}
	if listOnly {
		for _, s := range picked {
			fmt.Printf("%-26s %s\n", s.Name, s.Desc)
		}
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	top, err := gitTop(src)
	if err != nil {
		return fail(err)
	}
	box, err := sandboxOut(top, "new", "harness")
	if err != nil {
		return fail(err)
	}
	defer func() {
		if keep {
			fmt.Fprintf(os.Stderr, "harness: kept %s\n", box)
			return
		}
		if _, err := sandboxOut(top, "rm", box); err != nil {
			fmt.Fprintf(os.Stderr, "harness: %v\n", err)
		}
	}()
	work := filepath.Join(box, "_adv")
	if bin == "" {
		bin = filepath.Join(work, "kb")
		fmt.Fprintf(os.Stderr, "harness: building kb in %s\n", box)
		cmd := exec.CommandContext(ctx, "go", "build", "-tags", "fts5", "-o", bin, "./cmd/kb")
		cmd.Dir = box
		cmd.Env = append(os.Environ(), "GOPROXY=off", "GOWORK=off")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fail(fmt.Errorf("build kb: %v\n%s", err, out))
		}
	} else if bin, err = filepath.Abs(bin); err != nil {
		return fail(err)
	}

	var results []outcome
	failed := 0
	for _, s := range picked {
		if ctx.Err() != nil {
			break
		}
		o := runScenario(ctx, s, bin, filepath.Join(work, "runs", s.Name))
		if !o.Pass {
			failed++
		}
		results = append(results, o)
		if !asJSON {
			printOutcome(os.Stdout, o)
		}
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(results) // nowhere better to report a stdout failure
	} else {
		fmt.Printf("\n%d scenarios, %d passed, %d failed\n", len(results), len(results)-failed, failed)
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "harness: %v\n", err)
	return 2
}

func printOutcome(w io.Writer, o outcome) {
	status := "PASS"
	if !o.Pass {
		status = "FAIL"
	}
	fmt.Fprintf(w, "%s %-26s %6dms  %s\n", status, o.Name, o.Millis, o.Desc)
	for _, f := range o.Failures {
		fmt.Fprintf(w, "     ✗ %s\n", indent(f))
	}
	for _, n := range o.Notes {
		fmt.Fprintf(w, "     · %s\n", indent(n))
	}
}

func indent(s string) string { return strings.ReplaceAll(s, "\n", "\n       ") }

func runScenario(ctx context.Context, s scenario, bin, dir string) (o outcome) {
	h := &H{ctx: ctx, bin: bin, dir: dir,
		root: filepath.Join(dir, "root"), home: filepath.Join(dir, "home"), outside: filepath.Join(dir, "outside")}
	for _, d := range []string{h.root, h.home, h.outside, filepath.Join(dir, "tmp")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return outcome{Name: s.Name, Desc: s.Desc, Failures: []string{err.Error()}}
		}
	}
	t0 := time.Now()
	func() {
		defer func() {
			if r := recover(); r != nil && r != errAbort {
				h.fail("scenario panicked: %v", r)
			}
		}()
		s.Run(h)
	}()
	h.checkNoDownload()
	return outcome{Name: s.Name, Desc: s.Desc, Pass: len(h.failures) == 0, Failures: h.failures,
		Notes: h.notes, Millis: time.Since(t0).Milliseconds()}
}

// errAbort stops a scenario after a failed precondition (H.must).
var errAbort = errors.New("abort scenario")

// H is one scenario's world: a repo root, a HOME, a directory outside the root, and the binary.
type H struct {
	ctx                  context.Context
	bin, dir, root, home string
	outside              string
	failures, notes      []string
}

// res is one finished kb process.
type res struct {
	Args           []string
	Code           int
	Stdout, Stderr string
	Dur            time.Duration
}

func (r res) String() string {
	return fmt.Sprintf("kb %s -> exit %d\n  stderr: %s\n  stdout: %s", quoteArgs(r.Args), r.Code, clip(r.Stderr, 300), clip(r.Stdout, 300))
}

func (h *H) fail(format string, args ...any) {
	h.failures = append(h.failures, fmt.Sprintf(format, args...))
}
func (h *H) note(format string, args ...any) { h.notes = append(h.notes, fmt.Sprintf(format, args...)) }

// must aborts the scenario when a setup step fails.
func (h *H) must(ok bool, format string, args ...any) {
	if !ok {
		h.fail("setup: "+format, args...)
		panic(errAbort)
	}
}

func (h *H) env() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + h.home,
		"TMPDIR=" + filepath.Join(h.dir, "tmp"),
		"KB_ROOT=" + h.root,
		"KB_CALLER=harness",
		"REMBED_CACHE=" + filepath.Join(h.home, "rembed"),
		"HF_HOME=" + filepath.Join(h.home, "hf"),
		"HTTPS_PROXY=" + deadProxy, "HTTP_PROXY=" + deadProxy, "ALL_PROXY=" + deadProxy,
		"https_proxy=" + deadProxy, "http_proxy=" + deadProxy,
	}
}

// seedGarbageModel puts an unparsable config.json where the default model would be cached.
// rembed parses it before fetching anything, so a Pull fails offline even if the proxy were
// bypassed (and Pull deletes it, which is why it is reseeded before every command).
func (h *H) seedGarbageModel() {
	d := filepath.Join(h.home, "Library", "Caches", "kb", "models", defaultModelDir)
	_ = os.MkdirAll(d, 0o755) // a failure shows up as a download attempt in checkNoDownload
	_ = os.WriteFile(filepath.Join(d, "config.json"), []byte("not json {"), 0o644)
}

func (h *H) cmd(args ...string) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer) {
	h.seedGarbageModel()
	c := exec.CommandContext(h.ctx, h.bin, args...)
	c.Dir = h.root
	c.Env = h.env()
	var o, e bytes.Buffer
	c.Stdout, c.Stderr = &o, &e
	c.Stdin = nil // /dev/null: nothing may wait on a prompt
	c.WaitDelay = 5 * time.Second
	return c, &o, &e
}

// kb runs one command to completion (60 s cap) and checks the universal invariants on it.
func (h *H) kb(args ...string) res {
	ctx, cancel := context.WithTimeout(h.ctx, 60*time.Second)
	defer cancel()
	c, o, e := h.cmd(args...)
	c.Cancel = func() error { return c.Process.Kill() }
	t0 := time.Now()
	err := c.Start()
	if err == nil {
		done := make(chan error, 1)
		go func() { done <- c.Wait() }()
		select {
		case err = <-done:
		case <-ctx.Done():
			_ = c.Process.Kill()
			<-done
			h.fail("kb %s: still running after 60s (hang)", quoteArgs(args))
		}
	}
	r := res{Args: args, Code: exitCode(err), Stdout: o.String(), Stderr: e.String(), Dur: time.Since(t0)}
	h.invariants(r)
	return r
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ee.ExitCode() < 0 {
			return 128 + int(ee.Sys().(syscall.WaitStatus).Signal())
		}
		return ee.ExitCode()
	}
	return -1
}

var panicRe = regexp.MustCompile(`(?m)^(panic: |fatal error: |goroutine \d+ \[)`)

// invariants hold for every kb run: the CLI contract has exit codes 0, 1 and 2 only (a Go panic
// also exits 2, so a panic trace is checked separately), and nothing prints a Go stack.
func (h *H) invariants(r res) {
	if panicRe.MatchString(r.Stderr) || panicRe.MatchString(r.Stdout) {
		h.fail("Go panic or runtime crash:\n%s", r)
	}
	switch r.Code {
	case 0, 1, 2:
	case 130:
		// SIGINT, only from the scenario that sends one
	default:
		h.fail("exit code %d is outside the 0/1/2 contract:\n%s", r.Code, r)
	}
}

// expect fails unless r exited with one of codes.
func (h *H) expect(r res, codes ...int) bool {
	for _, c := range codes {
		if r.Code == c {
			return true
		}
	}
	h.fail("want exit %v:\n%s", codes, r)
	return false
}

// checkNoDownload fails if any model weights appeared in the scenario's HOME.
func (h *H) checkNoDownload() {
	_ = filepath.WalkDir(h.home, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && (strings.HasSuffix(p, ".safetensors") || strings.HasSuffix(p, ".onnx")) {
			h.fail("model weights downloaded: %s", p)
		}
		return nil
	})
}

// write creates root-relative (or absolute) path with content.
func (h *H) write(path, content string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(h.root, path)
	}
	h.must(os.MkdirAll(filepath.Dir(path), 0o755) == nil, "mkdir for %s", path)
	h.must(os.WriteFile(path, []byte(content), 0o644) == nil, "write %s", path)
	return path
}

// entryText is a valid file entry with the given title and body.
func entryText(title, body string) string {
	return "---\ntitle: " + title + "\nsummary: " + title + " summary\ntags: [adv]\n---\n\n" + body
}

func (h *H) dbPath() string { return filepath.Join(h.root, ".kb", "kb.sqlite") }

func quoteArgs(a []string) string {
	q := make([]string, len(a))
	for i, s := range a {
		s = clip(s, 60)
		if s == "" || strings.ContainsAny(s, " \t\n\"'*") {
			s = fmt.Sprintf("%q", s)
		}
		q[i] = s
	}
	return strings.Join(q, " ")
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + fmt.Sprintf("…(+%d bytes)", len(s)-n)
	}
	return s
}

// --- sandbox helpers (same protocol as the mutate tool) ---

func gitTop(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("%s is not inside a git repository: %w", dir, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func sandboxOut(top string, args ...string) (string, error) {
	cmd := exec.Command("bash", append([]string{filepath.Join(top, ".agent", "scripts", "sandbox.sh")}, args...)...)
	cmd.Dir = top
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("sandbox.sh %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	last := lines[len(lines)-1]
	if args[0] == "new" && !strings.Contains(last, "/.agent/sandbox/") {
		return "", fmt.Errorf("sandbox.sh returned %q, not a path under .agent/sandbox", last)
	}
	return last, nil
}
