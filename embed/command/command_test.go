package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/kb/embed"
)

// The fake embedding program is this test binary re-run with FAKE_EMBED_PROGRAM=1: TestMain hands
// control to fakeProgram, which speaks the package's stdin/stdout protocol. FAKE_EMBED_MODE picks
// a behaviour and FAKE_EMBED_LOG, when set, gets one line per run with the batch size.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_EMBED_PROGRAM") == "1" {
		os.Exit(fakeProgram())
	}
	os.Exit(m.Run())
}

func fakeProgram() int {
	var req request
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		fmt.Fprintln(os.Stderr, "bad request:", err)
		return 3
	}
	if p := os.Getenv("FAKE_EMBED_LOG"); p != "" {
		f, _ := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		fmt.Fprintf(f, "%d %s\n", len(req.Texts), req.Model)
		f.Close()
	}
	dim := req.Dim
	switch os.Getenv("FAKE_EMBED_MODE") {
	case "error":
		json.NewEncoder(os.Stdout).Encode(response{Error: "model " + req.Model + " is not installed"})
		return 0
	case "crash":
		fmt.Fprintln(os.Stderr, "segfault in model runtime")
		return 1
	case "garbage":
		fmt.Println("not json")
		return 0
	case "short":
		dim--
	case "few":
		req.Texts = req.Texts[1:]
	case "slow":
		time.Sleep(5 * time.Second)
	}
	out := response{Embeddings: make([][]float32, len(req.Texts))}
	for i, t := range req.Texts {
		v := make([]float32, dim)
		for j := range v {
			v[j] = float32(len(t)) + float32(j)/10
		}
		out.Embeddings[i] = v
	}
	json.NewEncoder(os.Stdout).Encode(out)
	return 0
}

// fakeArgv runs the fake program; mode and log are passed through the inherited environment.
func fakeArgv(t *testing.T, mode string) []string {
	t.Helper()
	t.Setenv("FAKE_EMBED_PROGRAM", "1")
	t.Setenv("FAKE_EMBED_MODE", mode)
	return []string{os.Args[0]}
}

func newClient(t *testing.T, mode string, dim int) *Client {
	t.Helper()
	c, err := New(Config{Argv: fakeArgv(t, mode), Model: "fake-model", Dim: dim})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEmbedReturnsVectorsInOrder(t *testing.T) {
	c := newClient(t, "", 3)
	got, err := c.Embed(context.Background(), []string{"a", "bbb", "cc"})
	if err != nil {
		t.Fatal(err)
	}
	for i, n := range []float32{1, 3, 2} {
		if len(got[i]) != 3 || got[i][0] != n {
			t.Fatalf("vector %d = %v", i, got[i])
		}
	}
	if vs, err := c.Embed(context.Background(), nil); err != nil || len(vs) != 0 {
		t.Fatalf("empty input: %v %v", vs, err)
	}
}

func TestEmbedSplitsIntoBatches(t *testing.T) {
	log := filepath.Join(t.TempDir(), "runs.log")
	t.Setenv("FAKE_EMBED_LOG", log)
	c, err := New(Config{Argv: fakeArgv(t, ""), Model: "m", Dim: 2, Batch: 2})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Embed(context.Background(), []string{"a", "b", "c", "d", "e"})
	if err != nil || len(got) != 5 {
		t.Fatalf("%d vectors, %v", len(got), err)
	}
	b, _ := os.ReadFile(log)
	if string(b) != "2 m\n2 m\n1 m\n" {
		t.Fatalf("runs:\n%s", b)
	}
}

func TestPing(t *testing.T) {
	if err := newClient(t, "", 4).Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := newClient(t, "short", 4).Ping(context.Background()); err == nil {
		t.Fatal("Ping should check the dimension")
	}
}

func TestErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		mode        string
		unavailable bool
		want        string
	}{
		{"error", false, "fake-model is not installed"},
		{"crash", true, "segfault in model runtime"},
		{"garbage", false, "not the expected JSON"},
		{"short", false, "dimension 2, want 3"},
		{"few", false, "expected 2 embeddings"},
	} {
		_, err := newClient(t, tc.mode, 3).Embed(context.Background(), []string{"x", "y"})
		if err == nil || !strings.Contains(err.Error(), tc.want) || errors.Is(err, embed.ErrUnavailable) != tc.unavailable {
			t.Errorf("%s: %v (unavailable=%v)", tc.mode, err, errors.Is(err, embed.ErrUnavailable))
		}
	}
}

func TestMissingProgramIsAConfigurationError(t *testing.T) {
	c, err := New(Config{Argv: []string{"kb-no-such-embedder-program"}, Model: "m", Dim: 2})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Embed(context.Background(), []string{"x"})
	if err == nil || errors.Is(err, embed.ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
}

func TestTimeouts(t *testing.T) {
	c := newClient(t, "slow", 2)
	c.BaseTimeout, c.PerTextTimeout = 200*time.Millisecond, 0
	if _, err := c.Embed(context.Background(), []string{"x"}); !errors.Is(err, embed.ErrUnavailable) {
		t.Fatalf("own deadline: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newClient(t, "slow", 2).Embed(ctx, []string{"x"}); !errors.Is(err, context.Canceled) || errors.Is(err, embed.ErrUnavailable) {
		t.Fatalf("caller cancel: %v", err)
	}
}

func TestNewValidates(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no command": {Model: "m", Dim: 2},
		"no model":   {Argv: []string{"x"}, Dim: 2},
		"no dim":     {Argv: []string{"x"}, Model: "m"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestProviderReadsOptions(t *testing.T) {
	argv := fakeArgv(t, "")
	r := embed.NewRegistry()
	r.MustRegister(Provider())
	env := embed.Env{Prefix: "KB_", Lookup: func(k string) (string, bool) {
		v, ok := map[string]string{
			"KB_EMBEDDER": "command", "KB_EMBED_MODEL": "my-onnx", "KB_EMBED_COMMAND": argv[0] + " --flag",
			"KB_EMBED_BATCH": "3", "KB_EMBED_TIMEOUT": "9", "KB_EMBED_QUERY_PREFIX": "query: ",
		}[k]
		return v, ok
	}}
	cfg, err := r.Resolve(embed.Config{Dim: 5}, env)
	if err != nil {
		t.Fatal(err)
	}
	e, err := r.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c := e.(*Client)
	if c.Model() != "my-onnx" || c.Dim() != 5 || c.cfg.Batch != 3 || c.BaseTimeout != 9*time.Second ||
		c.Command() != argv[0]+" --flag" || embed.QueryText(c, "q") != "query: q" || embed.DocText(c, "d") != "d" {
		t.Fatalf("got %+v", c)
	}
	for name, opts := range map[string]map[string]string{
		"no command": {},
		"bad batch":  {"command": "x", "batch": "many"},
	} {
		if _, err := r.Open(embed.Config{Provider: Name, Model: "m", Dim: 2, Options: opts}); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := r.Open(embed.Config{Provider: Name, Dim: 2, Options: map[string]string{"command": "x"}}); err == nil {
		t.Error("no model: want an error")
	}
}
