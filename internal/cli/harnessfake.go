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
	"github.com/nickstrad/kb/internal/store/sqlite"
)

func init() {
	harnessModel = fakeModel
	harnessNow = harnessClock(os.LookupEnv, os.Stderr)
	harnessIndexSteps = func() sqlite.Steps { return harnessProbe(os.LookupEnv) }
}

// harnessProbeEnv, set to 1, makes a harnessfake build one index release ahead of this one
// (sqlite.ProbeSteps), so a black-box run can drive an older store through the refusal, kb
// migrate up, the backup and the reindex it declares. Release builds never read it.
const harnessProbeEnv = "KB_HARNESS_INDEX_PROBE"

// harnessProbe is sqlite.ProbeSteps when KB_HARNESS_INDEX_PROBE=1, else nil (this build's steps).
func harnessProbe(lookup func(string) (string, bool)) sqlite.Steps {
	if v, _ := lookup(harnessProbeEnv); v == "1" {
		return sqlite.ProbeSteps()
	}
	return nil
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
