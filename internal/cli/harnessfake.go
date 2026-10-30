//go:build harnessfake

// The harness-only fake embedder (DEFAULTS #189): a build with the harnessfake tag accepts the
// model id fake:<dim> (1 to 4096) and embeds deterministically with internal/embed/fake, so the
// adversarial harness can exercise vector paths without a model. Release builds and go install
// never set the tag, so the id stays unknown there.
package cli

import (
	"strconv"
	"strings"

	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/fake"
)

func init() { harnessModel = fakeModel }

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
