package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/nickstrad/kb/internal/embed/local"
)

// testDeps are the deps every CLI test runs with (through run, runKB and execute). TestMain keeps
// them off the user's model cache, the network and the user cache directory: the model cache is an
// empty temp directory, a download always fails, and incidents with no store go to a temp
// directory. A test that replaces a field restores it with t.Cleanup; the CLI tests are not
// parallel.
var testDeps = productionDeps()

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "kb-cli-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testDeps.modelCacheDir = func() (string, error) { return tmp + "/models", nil }
	testDeps.pullModel = func(_ context.Context, spec local.Spec, _ string) error {
		return errors.New("download model " + spec.ID + ": downloads are disabled in tests")
	}
	testDeps.userCacheDir = func() (string, error) { return tmp + "/cache", nil }
	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}
