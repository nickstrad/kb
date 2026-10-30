//go:build harnessfake

// The harness-only fake embedder (DEFAULTS #189): a build with the harnessfake tag accepts the
// model id fake:<dim> (1 to 4096) and embeds deterministically with internal/embed/fake, so the
// adversarial harness can exercise vector paths without a model. Release builds and go install
// never set the tag, so the id stays unknown there.
package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/fake"
)

func init() {
	harnessModel = fakeModel
	harnessNow = harnessClock(os.LookupEnv, os.Stderr)
}

// harnessNowEnv pins the clock of a harnessfake build (RFC3339), so a black-box run can drive
// time-window behaviour: the open window, --since, kb stale. Release builds never read it (D4).
const harnessNowEnv = "KB_HARNESS_NOW"

// harnessClock is the clock KB_HARNESS_NOW names: every call returns that instant. When the
// variable is unset it returns nil (the wall clock); when it is not RFC3339 it warns on warn and
// returns nil.
func harnessClock(lookup func(string) (string, bool), warn io.Writer) func() time.Time {
	v, ok := lookup(harnessNowEnv)
	if !ok || v == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		fmt.Fprintf(warn, "kb: warning: %s=%q is not RFC3339; using the wall clock\n", harnessNowEnv, v)
		return nil
	}
	return func() time.Time { return t }
}

// fakeModel builds the fake embedder for "fake:<dim>"; ok is false for any other id.
func fakeModel(id string) (embed.Embedder, bool) {
	rest, ok := strings.CutPrefix(id, "fake:")
	if !ok {
		return nil, false
	}
	dim, err := strconv.Atoi(rest)
	if err != nil || dim < 1 || dim > 4096 || strconv.Itoa(dim) != rest {
		return nil, false
	}
	return fake.New(id, dim), true
}
