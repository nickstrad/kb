package fake_test

import (
	"testing"

	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/embedtest"
	"github.com/nickstrad/kb/internal/embed/fake"
)

func TestContract(t *testing.T) {
	embedtest.Run(t, func(*testing.T) embed.Embedder { return fake.New("fake", 16) }, embedtest.Caps{Deterministic: true})
}
