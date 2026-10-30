package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

// realTemp is a temp dir with symlinks resolved (macOS /var is a link to /private/var).
func realTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func mk(t *testing.T, path string, isDir bool) {
	t.Helper()
	if isDir {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}
