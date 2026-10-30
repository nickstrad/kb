package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// BaseNow is the clock every journey starts with (KB_HARNESS_NOW). It is normalised in output, but
// pinned so time-window behaviour does not depend on the run.
const BaseNow = "2026-03-01T09:00:00Z"

// Runner executes transcripts against one kb binary.
type Runner struct {
	// Kb is the path of the kb binary (built with -tags fts5,harnessfake).
	Kb string
	// Editor is the program EDITOR names: the test binary, in its editor helper mode.
	Editor string
	// Fixture implements "mkfixture ARTEFACT-vN PATH"; nil makes the builtin fail.
	Fixture func(artefact, dst string) error
	// Timeout bounds one command; zero is 2 minutes.
	Timeout time.Duration
}

// CmdResult is one executed command.
type CmdResult struct {
	N        int // 1-based position among the transcript's commands
	Cmd      string
	Expected []string
	Actual   []string
	OK       bool
}

// Report is a readable failure message naming the journey and command.
func (c CmdResult) Report(journey string) string {
	return fmt.Sprintf("journey %s, command %d: $ %s\n%s", journey, c.N, c.Cmd, Diff(c.Expected, c.Actual))
}

// session is the state of one journey run.
type session struct {
	r     *Runner
	tr    *Transcript
	work  string
	alt   []string
	cwd   string
	env   map[string]string
	order []string // env keys in insertion order
}

// Run executes tr in work (an empty directory; it is resolved through EvalSymlinks) and returns
// one result per command. The error is for setup problems and the no-model-weights guard, not for
// output mismatches.
func (r *Runner) Run(work string, tr *Transcript) ([]CmdResult, error) {
	resolved, err := filepath.EvalSymlinks(work)
	if err != nil {
		return nil, err
	}
	s := &session{r: r, tr: tr, work: resolved, env: map[string]string{}}
	if resolved != work {
		s.alt = []string{work}
	}
	if err := s.setup(); err != nil {
		return nil, err
	}
	var res []CmdResult
	for _, b := range tr.Blocks {
		if !b.IsCmd {
			continue
		}
		out := s.exec(b.Cmd)
		res = append(res, CmdResult{N: len(res) + 1, Cmd: b.Cmd, Expected: b.Out, Actual: out, OK: Match(b.Out, out)})
	}
	return res, NoWeights(s.work)
}

func (s *session) setup() error {
	for _, d := range []string{"home", "tmp", "bin", "proj", "rembed", "hf"} {
		if err := os.MkdirAll(filepath.Join(s.work, d), 0o755); err != nil {
			return err
		}
	}
	// kb edit runs the editor through sh, the one host tool a journey needs.
	if err := os.Symlink("/bin/sh", filepath.Join(s.work, "bin", "sh")); err != nil {
		return err
	}
	for _, f := range s.tr.Files {
		if strings.HasPrefix(f.Path, "@") {
			continue
		}
		if err := s.writeSection(f.Path, f.Path, s.work); err != nil {
			return err
		}
	}
	s.cwd = filepath.Join(s.work, "proj")
	dead := "http://127.0.0.1:1"
	for _, kv := range [][2]string{
		{"HOME", filepath.Join(s.work, "home")},
		{"TMPDIR", filepath.Join(s.work, "tmp")},
		{"REMBED_CACHE", filepath.Join(s.work, "rembed")},
		{"HF_HOME", filepath.Join(s.work, "hf")},
		{"PATH", filepath.Join(s.work, "bin")},
		{"HTTP_PROXY", dead}, {"HTTPS_PROXY", dead}, {"ALL_PROXY", dead},
		{"http_proxy", dead}, {"https_proxy", dead}, {"all_proxy", dead},
		{"TZ", "UTC"}, {"LANG", "C"},
		{"EDITOR", s.r.Editor},
		{"KB_HARNESS_NOW", BaseNow},
	} {
		s.setenv(kv[0], kv[1])
	}
	return nil
}

func (s *session) setenv(k, v string) {
	if _, ok := s.env[k]; !ok {
		s.order = append(s.order, k)
	}
	s.env[k] = v
}

func (s *session) environ() []string {
	var e []string
	for _, k := range s.order {
		if v, ok := s.env[k]; ok {
			e = append(e, k+"="+v)
		}
	}
	return e
}

func (s *session) abs(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(s.cwd, p)
}

func (s *session) writeSection(dest, section, base string) error {
	content, ok := s.tr.Section(section)
	if !ok {
		return fmt.Errorf("no file section %q", section)
	}
	if !filepath.IsAbs(dest) {
		dest = filepath.Join(base, dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dest, []byte(content), 0o644)
}

// exec runs one command line and returns its output lines (normalised, stderr prefixed, exit
// line last).
func (s *session) exec(line string) []string {
	args, err := splitArgs(line, s.work)
	var stdout, stderr string
	code := 0
	switch {
	case err != nil:
		stderr, code = "e2e: "+err.Error(), 127
	case len(args) == 0:
		stderr, code = "e2e: empty command", 127
	case args[0] == "kb":
		stdout, stderr, code = s.runKb(args[1:])
	default:
		stdout, stderr, code = s.builtin(args)
	}
	norm := func(x string) []string {
		x = Normalise(x, s.work, s.alt...)
		if x == "" {
			return nil
		}
		return strings.Split(strings.TrimSuffix(x, "\n"), "\n")
	}
	out := norm(stdout)
	for _, l := range norm(stderr) {
		out = append(out, stderrPrefix+l)
	}
	if code != 0 {
		out = append(out, fmt.Sprintf("[exit %d]", code))
	}
	return out
}

func (s *session) runKb(args []string) (stdout, stderr string, code int) {
	timeout := s.r.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.r.Kb, args...)
	cmd.Dir = s.cwd
	cmd.Env = s.environ()
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e // Stdin stays nil: /dev/null
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		code = ee.ExitCode()
	default:
		return "", "e2e: running kb: " + err.Error(), 127
	}
	if ctx.Err() == context.DeadlineExceeded {
		e.WriteString("e2e: kb timed out after " + timeout.String() + "\n")
	}
	return o.String(), e.String(), code
}

// builtin runs a runner builtin; failures are stderr text and exit 1.
func (s *session) builtin(args []string) (stdout, stderr string, code int) {
	out, err := s.builtinOut(args)
	if err != nil {
		return out, err.Error(), 1
	}
	return out, "", 0
}

var (
	reRelTime   = regexp.MustCompile(`^\+(\d+)([dhm])$`)
	errNotFound = errors.New("not found")
)

func (s *session) builtinOut(args []string) (string, error) {
	name, rest := args[0], args[1:]
	need := func(n int) error {
		if len(rest) != n {
			return fmt.Errorf("%s: want %d argument(s), got %d", name, n, len(rest))
		}
		return nil
	}
	switch name {
	case "cd":
		if err := need(1); err != nil {
			return "", err
		}
		d := s.abs(rest[0])
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			return "", fmt.Errorf("cd: %s is not a directory", rest[0])
		}
		s.cwd = d
	case "env":
		if len(rest) == 0 {
			return "", errors.New("env: want K=V")
		}
		for _, kv := range rest {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				return "", fmt.Errorf("env: %q is not K=V", kv)
			}
			switch {
			case v == "":
				delete(s.env, k)
				s.order = slices.DeleteFunc(s.order, func(o string) bool { return o == k })
			case k == "KB_HARNESS_NOW" && reRelTime.MatchString(v):
				nv, err := advance(s.env[k], v)
				if err != nil {
					return "", err
				}
				s.setenv(k, nv)
			default:
				s.setenv(k, v)
			}
		}
	case "write":
		if len(rest) != 1 && len(rest) != 2 {
			return "", errors.New("write: want DEST [SECTION]")
		}
		section := rest[0]
		if len(rest) == 2 {
			section = rest[1]
		}
		return "", s.writeSection(rest[0], section, s.cwd)
	case "rm":
		if len(rest) > 0 && rest[0] == "-r" {
			rest = rest[1:]
		}
		if len(rest) == 0 {
			return "", errors.New("rm: want at least one path")
		}
		for _, p := range rest {
			if err := os.RemoveAll(s.abs(p)); err != nil {
				return "", err
			}
		}
	case "mkdir":
		if err := need(1); err != nil {
			return "", err
		}
		return "", os.MkdirAll(s.abs(rest[0]), 0o755)
	case "cat":
		if err := need(1); err != nil {
			return "", err
		}
		b, err := os.ReadFile(s.abs(rest[0]))
		return string(b), err
	case "exists", "!":
		neg := name == "!"
		if neg {
			if len(rest) == 0 || rest[0] != "exists" {
				return "", errors.New("!: only '! exists GLOB' is supported")
			}
			rest = rest[1:]
		}
		if len(rest) != 1 {
			return "", errors.New("exists: want GLOB")
		}
		m, err := filepath.Glob(s.abs(rest[0]))
		if err != nil {
			return "", err
		}
		switch {
		case neg && len(m) > 0:
			return "", fmt.Errorf("exists: %s matches %d path(s)", rest[0], len(m))
		case !neg && len(m) == 0:
			return "", fmt.Errorf("exists: %s matches nothing", rest[0])
		}
	case "grep":
		if err := need(2); err != nil {
			return "", err
		}
		re, err := regexp.Compile(rest[0])
		if err != nil {
			return "", err
		}
		b, err := os.ReadFile(s.abs(rest[1]))
		if err != nil {
			return "", err
		}
		var hits []string
		for _, l := range strings.Split(string(b), "\n") {
			if re.MatchString(l) {
				hits = append(hits, l)
			}
		}
		if len(hits) == 0 {
			return "", fmt.Errorf("grep: no line of %s matches %s", rest[1], rest[0])
		}
		return strings.Join(hits, "\n") + "\n", nil
	case "mkfixture":
		if err := need(2); err != nil {
			return "", err
		}
		if s.r.Fixture == nil {
			return "", errors.New("mkfixture: no fixture loader")
		}
		dst := s.abs(rest[1])
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", err
		}
		return "", s.r.Fixture(rest[0], dst)
	default:
		return "", fmt.Errorf("%s: %w: not kb and not a builtin", name, errNotFound)
	}
	return "", nil
}

// advance adds a "+Nd|h|m" offset to an RFC3339 time.
func advance(cur, rel string) (string, error) {
	t, err := time.Parse(time.RFC3339, cur)
	if err != nil {
		return "", fmt.Errorf("env: KB_HARNESS_NOW %q is not RFC3339", cur)
	}
	m := reRelTime.FindStringSubmatch(rel)
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return "", fmt.Errorf("env: KB_HARNESS_NOW step %q: %w", rel, err)
	}
	unit := map[string]time.Duration{"d": 24 * time.Hour, "h": time.Hour, "m": time.Minute}[m[2]]
	return t.Add(time.Duration(n) * unit).UTC().Format(time.RFC3339), nil
}

// splitArgs splits a command line into words: whitespace separates, single and double quotes
// group (no escapes), and $WORK expands to the journey dir.
func splitArgs(line, work string) ([]string, error) {
	var args []string
	var cur strings.Builder
	in, quote := false, rune(0)
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ' || r == '\t':
			if in {
				args = append(args, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote")
	}
	if in {
		args = append(args, cur.String())
	}
	for i := range args {
		args[i] = strings.ReplaceAll(args[i], "$WORK", work)
	}
	return args, nil
}

// NoWeights fails when model weights appeared under work: a journey must never download a model.
func NoWeights(work string) error {
	var found []string
	walkErr := filepath.WalkDir(work, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && (strings.HasSuffix(p, ".safetensors") || strings.HasSuffix(p, ".onnx")) {
			found = append(found, p)
		}
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("checking %s for model weights: %w", work, walkErr)
	}
	if len(found) > 0 {
		return fmt.Errorf("model weights appeared under the journey dir (a journey must not download): %v", found)
	}
	return nil
}

// Updated returns a copy of tr whose expected blocks follow results (see Merge). It fails when an
// output line would read back as a command.
func Updated(tr *Transcript, results []CmdResult) (*Transcript, error) {
	out := &Transcript{Files: tr.Files}
	n := 0
	for _, b := range tr.Blocks {
		if b.IsCmd {
			if n >= len(results) {
				return nil, errors.New("fewer results than commands")
			}
			b.Out = Merge(b.Out, results[n].Actual)
			for _, l := range results[n].Actual {
				if !plainLine(l) {
					return nil, fmt.Errorf("output line %q of %q would read back as a command or an elision", l, b.Cmd)
				}
			}
			n++
		}
		out.Blocks = append(out.Blocks, b)
	}
	return out, nil
}

// plainLine reports whether an actual output line survives being written to a transcript: it is
// not "$ ...", "...", or a "~ RE" line.
func plainLine(l string) bool {
	return l != "..." && !strings.HasPrefix(l, cmdPrefix) && !strings.HasPrefix(l, "~ ") && !strings.HasPrefix(l, stderrPrefix+"~ ")
}
