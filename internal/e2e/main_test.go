package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// builtDir is where the journeys test built kb; TestMain removes it.
var builtDir string

// editorEnv switches the test binary into the editor helper mode: kb edit runs EDITOR (this
// binary) on a path, and the helper appends the variable's text to that file.
const editorEnv = "E2E_EDITOR_APPEND"

// fakeKbName is the file name under which the test binary acts as a fake kb (runner_test.go).
const fakeKbName = "kb"

// fakeKbMain is the fake kb: each first argument picks a behaviour the runner tests check.
func fakeKbMain(args []string) int {
	cwd, _ := os.Getwd()
	arg := ""
	if len(args) > 0 {
		arg = args[0]
	}
	switch arg {
	case "fail":
		fmt.Println("partial")
		fmt.Fprintf(os.Stderr, "kb fail: broken at %s\n", cwd)
		return 1
	case "quiet":
	case "blank":
		fmt.Print("a\n\nb\n")
	case "env":
		fmt.Printf("HOME=%s NOW=%s PATH=%s EDITOR=%s PROXY=%s\n", os.Getenv("HOME"), os.Getenv("KB_HARNESS_NOW"), os.Getenv("PATH"), os.Getenv("EDITOR"), os.Getenv("HTTPS_PROXY"))
	case "sleep":
		time.Sleep(time.Minute)
	case "countzz":
		n := 0
		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, "ZZ=") {
				n++
			}
		}
		fmt.Println(n)
	case "version":
		fmt.Println("kb v0.0.0-20261030193210-abc")
	case "weights":
		dir := filepath.Join(os.Getenv("HOME"), "m")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := os.WriteFile(filepath.Join(dir, "model.onnx"), nil, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	default:
		fmt.Println("args: " + strings.Join(args, " "))
		fmt.Println(cwd)
	}
	return 0
}

func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == fakeKbName {
		os.Exit(fakeKbMain(os.Args[1:]))
	}
	if text := os.Getenv(editorEnv); text != "" && len(os.Args) == 2 {
		f, err := os.OpenFile(os.Args[1], os.O_APPEND|os.O_WRONLY, 0)
		if err == nil {
			_, err = fmt.Fprintln(f, text)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "e2e editor helper:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	code := m.Run()
	if builtDir != "" {
		os.RemoveAll(builtDir)
	}
	os.Exit(code)
}
