package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/errs"
)

// TestReadCapped (Q111): a regular file is read; a missing one is the os error; a directory is a
// Usage error naming the kind of file; an over-cap file is a Usage error.
func TestReadCapped(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "ok.txt")
	if err := os.WriteFile(ok, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(big, make([]byte, MaxFileBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path string
		want       string
		notExist   bool
		usage      string // substring of the Usage error
	}{
		{name: "ok", path: ok, want: "hello"},
		{name: "missing", path: filepath.Join(dir, "nope"), notExist: true},
		{name: "directory", path: dir, usage: "the-kind file must be a plain file"},
		{name: "over the cap", path: big, usage: "the-kind file may be at most"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadCapped("test.op", "the-kind", tc.path)
			switch {
			case tc.notExist:
				if !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("err = %v, want fs.ErrNotExist", err)
				}
			case tc.usage != "":
				if errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), tc.usage) {
					t.Errorf("err = %v, want a Usage error containing %q", err, tc.usage)
				}
			default:
				if err != nil || string(got) != tc.want {
					t.Errorf("got %q, %v; want %q", got, err, tc.want)
				}
			}
		})
	}
}
