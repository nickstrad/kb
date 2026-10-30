//go:build e2e

package e2e

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

var (
	buildOnce sync.Once
	kbPath    string
	buildErr  error
)

// kbBinary is the binary under test: -kb PATH, or one built once per run with
// -tags fts5,harnessfake (GOWORK=off, GOPROXY=off so a build never reaches for the network).
func kbBinary(t *testing.T) string {
	t.Helper()
	if *kbFlag != "" {
		p, err := filepath.Abs(*kbFlag)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "kb-e2e-bin-")
		if err != nil {
			buildErr = err
			return
		}
		builtDir = dir
		kbPath = filepath.Join(dir, "kb")
		cmd := exec.Command("go", "build", "-tags", "fts5,harnessfake", "-o", kbPath, "./cmd/kb")
		cmd.Dir = "../.."
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("building kb: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return kbPath
}

var reFixture = regexp.MustCompile(`^([a-z]+)-v[0-9]+$`)

// loadFixture implements "mkfixture <artefact>-v<N> PATH": it runs the frozen SQL of
// internal/store/sqlite/testdata/migrations/<artefact>-v<N>.sql on a new database at dst.
func loadFixture(name, dst string) error {
	if !reFixture.MatchString(name) {
		return fmt.Errorf("mkfixture: %q is not <artefact>-v<N>", name)
	}
	script, err := os.ReadFile(filepath.Join("..", "store", "sqlite", "testdata", "migrations", name+".sql"))
	if err != nil {
		return fmt.Errorf("mkfixture: %w", err)
	}
	db, err := sql.Open("sqlite3", dst)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(string(script)); err != nil {
		return fmt.Errorf("mkfixture %s: %w", name, err)
	}
	return nil
}

func TestJourneys(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "journeys", "*.txt"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no journeys found: %v", err)
	}
	kb := kbBinary(t)
	editor, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		name := filepath.Base(file)
		name = name[:len(name)-len(".txt")]
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			src, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			tr, err := Parse(string(src))
			if err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			r := &Runner{Kb: kb, Editor: editor, Fixture: loadFixture}
			res, err := r.Run(t.TempDir(), tr)
			if err != nil {
				t.Error(err)
			}
			if *update {
				up, err := Updated(tr, res)
				if err != nil {
					t.Fatal(err)
				}
				if got := Format(up); got != string(src) {
					if err := os.WriteFile(file, []byte(got), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				return
			}
			for _, c := range res {
				if !c.OK {
					t.Error(c.Report(name))
				}
			}
		})
	}
}
