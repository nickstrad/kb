package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/migrate"
)

func TestTOMLStepsShape(t *testing.T) {
	if err := configSteps.Check(ConfigArtefactName); err != nil {
		t.Error(err)
	}
	if err := locationSteps.Check(LocationArtefactName); err != nil {
		t.Error(err)
	}
	if configSteps.Target() != SchemaVersion || locationSteps.Target() != LocationSchemaVersion {
		t.Errorf("targets %d, %d; schema versions %d, %d", configSteps.Target(), locationSteps.Target(), SchemaVersion, LocationSchemaVersion)
	}
}

// v2Steps is a two-step list for the tests: v1 -> v2 renames [embed] model to name and adds a key.
// It stands in for a future real step, because SchemaVersion is 1 today.
var v2Steps = migrate.Steps[map[string]any]{
	configSteps[0],
	{
		Info: migrate.Info{From: 1, ID: "config/0002-test", Desc: "rename embed.model to embed.name", Effect: migrate.EffectReindex},
		Up: func(_ context.Context, m map[string]any) error {
			e := m["embed"].(map[string]any)
			e["name"] = e["model"]
			delete(e, "model")
			return nil
		},
	},
}

// testArt is a config artefact whose "current" kb has v2Steps. check accepts any text that parses
// as TOML with version 2, so Apply's result check is exercised too.
func testArt(path string) *tomlArtefact {
	return &tomlArtefact{name: "config", path: path, steps: v2Steps, check: func(data []byte, file string) error {
		var m map[string]any
		if _, err := toml.Decode(string(data), &m); err != nil {
			return err
		}
		if m["version"] != int64(2) {
			return errs.New(errs.Usage, "test", "%s: version is %v", file, m["version"])
		}
		return nil
	}}
}

func writeTOML(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestArtefactStatus is the status table: file state -> current, pending, error.
func TestArtefactStatus(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        string // "" with missing = no file
		missing     bool
		current     int
		pending     int
		newer       bool
		errContains string
	}{
		{name: "missing file is version 0 with nothing pending", missing: true},
		{name: "older", body: "version = 1\n[embed]\nmodel = \"none\"\n", current: 1, pending: 1},
		{name: "current", body: "version = 2\n", current: 2},
		{name: "newer", body: "version = 3\n", current: 3, newer: true},
		{name: "no version key reads as current", body: "[embed]\nmodel = \"none\"\n", current: 2},
		{name: "version 0", body: "version = 0\n", errContains: "positive integer"},
		{name: "negative", body: "version = -1\n", errContains: "positive integer"},
		{name: "a string", body: "version = \"1\"\n", errContains: "positive integer"},
		{name: "a float", body: "version = 1.5\n", errContains: "positive integer"},
		{name: "syntax error", body: "version = \n", errContains: "config.toml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if !tc.missing {
				path = writeTOML(t, tc.body)
			}
			st, err := testArt(path).Status(context.Background())
			if tc.errContains != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errContains) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.errContains)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if st.Current != tc.current || len(st.Pending) != tc.pending || st.Newer() != tc.newer || st.Target != 2 || st.Artefact != "config" || st.Path != path {
				t.Errorf("status %+v; want current %d, %d pending, newer %v, target 2", st, tc.current, tc.pending, tc.newer)
			}
		})
	}
}

func TestArtefactApplyV1ToV2(t *testing.T) {
	ctx := context.Background()
	path := writeTOML(t, "# my notes\nversion = 1\n\n[embed]\nmodel = \"none\"\n\n[search]\nk = 5\n")
	a := testArt(path)
	applied, err := a.Apply(ctx)
	if err != nil || len(applied) != 1 || applied[0].ID != "config/0002-test" {
		t.Fatalf("Apply = %v, %v", applied, err)
	}
	var got map[string]any
	data, _ := os.ReadFile(path)
	if _, err := toml.Decode(string(data), &got); err != nil {
		t.Fatal(err)
	}
	if got["version"] != int64(2) {
		t.Errorf("version = %v, want 2", got["version"])
	}
	embed := got["embed"].(map[string]any)
	if embed["name"] != "none" || embed["model"] != nil {
		t.Errorf("embed = %v, want name = none and no model", embed)
	}
	if got["search"].(map[string]any)["k"] != int64(5) {
		t.Errorf("an untouched key was lost: %v", got["search"])
	}
	if strings.Contains(string(data), "my notes") {
		t.Error("comments are expected to be dropped on rewrite")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640 kept", fi.Mode().Perm())
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".config.toml.migrate-*")); len(m) != 0 {
		t.Errorf("temp files left behind: %v", m)
	}
	if err := a.Verify(ctx); err != nil {
		t.Errorf("Verify after Apply: %v", err)
	}
	// A second Apply has nothing to do and leaves the bytes alone.
	if again, err := a.Apply(ctx); err != nil || again != nil {
		t.Errorf("second Apply = %v, %v; want nothing", again, err)
	}
}

// TestArtefactApplyRefusals: files Apply must not touch, and failures that leave the file as it was.
func TestArtefactApplyRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("newer file", func(t *testing.T) {
		path := writeTOML(t, "version = 3\n")
		_, err := testArt(path).Apply(ctx)
		if errs.KindOf(err) != errs.Conflict || !strings.Contains(err.Error(), "newer kb") {
			t.Fatalf("err = %v (kind %v)", err, errs.KindOf(err))
		}
		assertContent(t, path, "version = 3\n")
	})
	t.Run("missing file", func(t *testing.T) {
		_, err := testArt(filepath.Join(t.TempDir(), "x.toml")).Apply(ctx)
		if errs.KindOf(err) != errs.NotFound {
			t.Fatalf("err = %v (kind %v), want NotFound", err, errs.KindOf(err))
		}
	})
	t.Run("a step that fails", func(t *testing.T) {
		body := "version = 1\n[embed]\nmodel = \"none\"\n"
		path := writeTOML(t, body)
		a := testArt(path)
		a.steps = migrate.Steps[map[string]any]{configSteps[0], {Info: v2Steps[1].Info, Up: func(context.Context, map[string]any) error { return errs.New(errs.Internal, "t", "boom") }}}
		_, err := a.Apply(ctx)
		if err == nil || !strings.Contains(err.Error(), "config/0002-test") {
			t.Fatalf("err = %v, want the step ID", err)
		}
		assertContent(t, path, body)
	})
	t.Run("a result the parser rejects", func(t *testing.T) {
		body := "version = 1\n[embed]\nmodel = \"none\"\n"
		path := writeTOML(t, body)
		a := testArt(path)
		a.check = func([]byte, string) error { return errs.New(errs.Usage, "t", "nope") }
		_, err := a.Apply(ctx)
		if err == nil || !strings.Contains(err.Error(), "would not be valid") {
			t.Fatalf("err = %v", err)
		}
		assertContent(t, path, body)
	})
	t.Run("cancelled before the rename", func(t *testing.T) {
		body := "version = 1\n"
		path := writeTOML(t, body)
		cctx, cancel := context.WithCancel(ctx)
		a := testArt(path)
		a.steps = migrate.Steps[map[string]any]{configSteps[0], {Info: v2Steps[1].Info, Up: func(context.Context, map[string]any) error { cancel(); return nil }}}
		_, err := a.Apply(cctx)
		if errs.KindOf(err) != errs.Interrupted {
			t.Fatalf("err = %v (kind %v), want Interrupted", err, errs.KindOf(err))
		}
		assertContent(t, path, body)
	})
}

func assertContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Errorf("%s = %q, %v; want it unchanged (%q)", path, got, err, want)
	}
}

func TestArtefactBackup(t *testing.T) {
	ctx := context.Background()
	body := "# keep me\nversion = 1\n"
	path := writeTOML(t, body)
	dst := path + ".bak-v1-20261030-120000"
	a := testArt(path)
	if err := a.Backup(ctx, dst); err != nil {
		t.Fatal(err)
	}
	assertContent(t, dst, body)
	if err := a.Backup(ctx, dst); err == nil { // never overwrites
		t.Error("a second backup to the same name succeeded")
	}
	if err := testArt(filepath.Join(t.TempDir(), "none.toml")).Backup(ctx, dst+"x"); errs.KindOf(err) != errs.NotFound {
		t.Errorf("backup of a missing file: %v (kind %v), want NotFound", err, errs.KindOf(err))
	}
}

// TestArtefactVerify uses the real parsers: a valid current file passes, drift is a Conflict.
func TestArtefactVerify(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		art  func(string) migrate.Artefact
		body string
		ok   bool
	}{
		{"config ok", NewArtefact, "version = 1\n[embed]\nmodel = \"none\"\n", true},
		{"config unknown key", NewArtefact, "version = 1\nbogus = 1\n", false},
		{"config newer", NewArtefact, "version = 2\n", false},
		{"location ok", NewLocationArtefact, "version = 1\ndefault_store = \"default\"\n", true},
		{"location bad store name", NewLocationArtefact, "version = 1\ndefault_store = \"BAD NAME\"\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.art(writeTOML(t, tc.body)).(migrate.Verifier).Verify(ctx)
			if tc.ok != (err == nil) {
				t.Fatalf("Verify = %v, want ok %v", err, tc.ok)
			}
			if err != nil && errs.KindOf(err) != errs.Conflict {
				t.Errorf("kind %v, want Conflict", errs.KindOf(err))
			}
		})
	}
	if err := NewArtefact(filepath.Join(t.TempDir(), "none")).(migrate.Verifier).Verify(ctx); errs.KindOf(err) != errs.NotFound {
		t.Errorf("verify of a missing file: %v", err)
	}
}

// TestRealArtefactsAtCurrent: today's config and location files are current with nothing pending,
// and applying changes nothing.
func TestRealArtefactsAtCurrent(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		art  func(string) migrate.Artefact
		body string
	}{
		"config":   {NewArtefact, "version = 1\n"},
		"location": {NewLocationArtefact, "version = 1\ndefault_store = \"default\"\n"},
	} {
		t.Run(name, func(t *testing.T) {
			path := writeTOML(t, tc.body)
			a := tc.art(path)
			st, err := a.Status(ctx)
			if err != nil || st.Current != 1 || st.Target != 1 || len(st.Pending) != 0 || st.Artefact != name {
				t.Fatalf("Status = %+v, %v", st, err)
			}
			if applied, err := a.Apply(ctx); err != nil || applied != nil {
				t.Errorf("Apply = %v, %v", applied, err)
			}
			assertContent(t, path, tc.body)
		})
	}
}

// TestFileHelpersFail: the two writers report a Usage error and leave nothing behind.
func TestFileHelpersFail(t *testing.T) {
	dir := t.TempDir()
	gone := filepath.Join(dir, "no", "such", "dir", "f.toml")
	if err := replaceFile("t", gone, []byte("x"), 0o644); errs.KindOf(err) != errs.Usage {
		t.Errorf("replaceFile into a missing directory: %v (kind %v)", err, errs.KindOf(err))
	}
	if err := writeFileExcl("t", gone, []byte("x"), 0o644); errs.KindOf(err) != errs.Usage {
		t.Errorf("writeFileExcl into a missing directory: %v (kind %v)", err, errs.KindOf(err))
	}
	// Renaming a file over a directory fails after the temp file was written: it must be removed.
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := replaceFile("t", target, []byte("x"), 0o644); errs.KindOf(err) != errs.Usage {
		t.Errorf("replaceFile over a directory: %v", err)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, ".target.migrate-*")); len(m) != 0 {
		t.Errorf("temp file left behind: %v", m)
	}
}

// TestArtefactCancelled: a cancelled context is Interrupted from Status and Backup.
func TestArtefactCancelled(t *testing.T) {
	path := writeTOML(t, "version = 1\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := testArt(path).Status(ctx); errs.KindOf(err) != errs.Interrupted {
		t.Errorf("Status: %v", err)
	}
	if err := testArt(path).Backup(ctx, path+".bak"); errs.KindOf(err) != errs.Interrupted {
		t.Errorf("Backup: %v", err)
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Error("a cancelled Backup wrote the file")
	}
}

// TestArtefactVersionBounds: the version key's accepted range and the file-shape refusals, as
// Status reports them (Usage naming the file), plus the explicit lowest version.
func TestArtefactVersionBounds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		current int
		err     string // "" = ok
	}{
		{"explicit version 1", "version = 1\n", 1, ""},
		{"largest accepted (1<<31)", "version = 2147483648\n", 1 << 31, ""},
		{"one over the largest", "version = 2147483649\n", 0, "positive integer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTOML(t, tc.body)
			st, err := testArt(path).Status(context.Background())
			if tc.err != "" {
				if errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), tc.err) || !strings.Contains(err.Error(), path) {
					t.Fatalf("Status = %+v, %v; want Usage %q naming the file", st, err, tc.err)
				}
				return
			}
			if err != nil || st.Current != tc.current {
				t.Fatalf("Status = %+v, %v; want current %d", st, err, tc.current)
			}
		})
	}
	// The largest version is newer than this kb: Apply refuses it as a Conflict, never runs steps.
	big := writeTOML(t, "version = 2147483648\n")
	if _, err := testArt(big).Apply(context.Background()); errs.KindOf(err) != errs.Conflict {
		t.Errorf("Apply on version 1<<31: %v, want Conflict", err)
	}
	dir := filepath.Join(t.TempDir(), "config.toml")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := testArt(dir).Status(context.Background()); errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("a directory: %v, want Usage \"not a regular file\"", err)
	}
	huge := writeTOML(t, "version = 1\n# "+strings.Repeat("x", MaxFileBytes)+"\n")
	if _, err := testArt(huge).Status(context.Background()); errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), "at most") {
		t.Errorf("an oversized file: %v, want Usage \"at most\"", err)
	}
}

// TestMigrateKind: a step failure is Interrupted when the run was cancelled (by the error or the
// context) and Internal otherwise; it decides the exit code of an interrupted kb migrate up.
func TestMigrateKind(t *testing.T) {
	live := context.Background()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	other := errs.New(errs.Usage, "test", "step failed")
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		want errs.Kind
	}{
		{"canceled error, live ctx", live, context.Canceled, errs.Interrupted},
		{"wrapped canceled error", live, errs.Wrap(context.Canceled, errs.Internal, "t", "x"), errs.Interrupted},
		{"other error, cancelled ctx", cancelled, other, errs.Interrupted},
		{"other error, live ctx", live, other, errs.Internal},
	} {
		if got := migrateKind(tc.ctx, tc.err); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestArtefactBackupRefusesExisting: a backup never overwrites: an existing destination is a Usage
// error naming it, and its bytes are unchanged.
func TestArtefactBackupRefusesExisting(t *testing.T) {
	path := writeTOML(t, "version = 1\n")
	dst := path + ".bak-v1-20261030-120000"
	if err := os.WriteFile(dst, []byte("older backup"), 0o640); err != nil {
		t.Fatal(err)
	}
	err := testArt(path).Backup(context.Background(), dst)
	if errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), dst) {
		t.Fatalf("Backup over an existing file: %v, want Usage naming %s", err, dst)
	}
	assertContent(t, dst, "older backup")
}
