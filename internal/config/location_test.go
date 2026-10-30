package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/nickstrad/kb/internal/errs"
)

func TestParseLocation(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Location
		kind errs.Kind // errs.Internal (zero) means success
		msg  string
	}{
		{name: "empty file is the defaults", in: "", want: DefaultLocation("default")},
		{name: "full", in: "version = 1\ndefault_store = \"adr\"\n[aliases]\nnotes = \"~/knowledge\"\nadr = \"adr@~/src/proj\"\n",
			want: Location{Version: 1, DefaultStore: "adr", Aliases: map[string]string{"notes": "~/knowledge", "adr": "adr@~/src/proj"}}},
		{name: "default_store omitted", in: "version = 1\n", want: DefaultLocation("default")},
		{name: "unknown key", in: "colour = 1\n", kind: errs.Usage, msg: `unknown key "colour"`},
		{name: "two unknown keys", in: "a = 1\nb = 2\n", kind: errs.Usage, msg: "unknown keys"},
		{name: "unknown key in aliases is an alias, not an error", in: "[aliases]\nx = \"/p\"\n", want: Location{Version: 1, DefaultStore: "default", Aliases: map[string]string{"x": "/p"}}},
		{name: "bad type", in: "default_store = 3\n", kind: errs.Usage, msg: "default_store"},
		{name: "syntax error names the line", in: "version = 1\nxx\n", kind: errs.Usage, msg: ":2"},
		{name: "newer version", in: "version = 2\n", kind: errs.Usage, msg: "newer kb"},
		{name: "older version", in: "version = 0\n", kind: errs.Usage, msg: "unsupported location version 0"},
		{name: "invalid default_store", in: "default_store = \"../x\"\n", kind: errs.Usage, msg: "default_store"},
		{name: "reserved default_store", in: "default_store = \"global\"\n", kind: errs.Usage, msg: "reserved"},
		{name: "empty default_store", in: "default_store = \"\"\n", kind: errs.Usage, msg: "default_store"},
		{name: "invalid alias name", in: "[aliases]\n\"Bad\" = \"/p\"\n", kind: errs.Usage, msg: "aliases"},
		{name: "empty alias target", in: "[aliases]\nx = \"\"\n", kind: errs.Usage, msg: "aliases.x: the target is empty"},
		{name: "alias target at the limit", in: "[aliases]\nx = \"" + strings.Repeat("a", MaxAliasTargetLen) + "\"\n",
			want: Location{Version: 1, DefaultStore: "default", Aliases: map[string]string{"x": strings.Repeat("a", MaxAliasTargetLen)}}},
		{name: "alias target over the limit", in: "[aliases]\nx = \"" + strings.Repeat("a", MaxAliasTargetLen+1) + "\"\n", kind: errs.Usage, msg: "at most 4096"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseLocation([]byte(tc.in), "kb.toml")
			if tc.kind != errs.Internal {
				if err == nil || errs.KindOf(err) != tc.kind || !strings.Contains(err.Error(), tc.msg) {
					t.Fatalf("got %+v, %v; want %v error containing %q", got, err, tc.kind, tc.msg)
				}
				if !strings.Contains(err.Error(), "kb.toml") {
					t.Fatalf("error does not name the file: %v", err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestLocationOlderVersionIsConflict(t *testing.T) {
	// Only reachable once LocationSchemaVersion > 1; checkVersion is the shared rule.
	if err := checkVersion("kb.toml", 1, 2); errs.KindOf(err) != errs.Conflict {
		t.Fatalf("want Conflict, got %v", err)
	}
}

func TestLocationRoundTrip(t *testing.T) {
	for _, l := range []Location{
		DefaultLocation("default"),
		{Version: 1, DefaultStore: "adr", Aliases: map[string]string{"a": "/x y", "b-1": "adr@~/p"}},
	} {
		data, err := SaveLocation(l)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ParseLocation(data, "f")
		if err != nil || !reflect.DeepEqual(got, l) {
			t.Fatalf("round trip of %+v gave %+v, %v\n%s", l, got, err, data)
		}
		again, _ := SaveLocation(got)
		if string(again) != string(data) {
			t.Fatalf("Save(Parse(x)) is not a fixed point:\n%s\n%s", data, again)
		}
	}
}

func TestReadLocation(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.toml")
	os.WriteFile(good, []byte("default_store = \"x\"\n"), 0o644)
	l, err := ReadLocation(good)
	if err != nil || l.DefaultStore != "x" {
		t.Fatalf("good: %+v %v", l, err)
	}
	if _, err := ReadLocation(filepath.Join(dir, "missing.toml")); errs.KindOf(err) != errs.NotFound || !strings.Contains(err.Error(), "run kb init") {
		t.Fatalf("missing: %v", err)
	}
	if _, err := ReadLocation(dir); errs.KindOf(err) != errs.Usage {
		t.Fatalf("a directory: %v", err)
	}
	atCap := filepath.Join(dir, "cap.toml")
	os.WriteFile(atCap, []byte("# "+strings.Repeat("x", MaxFileBytes-3)+"\n"), 0o644)
	if _, err := ReadLocation(atCap); err != nil {
		t.Fatalf("a file at the cap (%d bytes) should read: %v", MaxFileBytes, err)
	}
	big := filepath.Join(dir, "big.toml")
	os.WriteFile(big, []byte("# "+strings.Repeat("x", MaxFileBytes-2)+"\n"), 0o644)
	if _, err := ReadLocation(big); errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("over the cap: %v", err)
	}
	if err := os.Chmod(good, 0); err == nil {
		if _, err := ReadLocation(good); errs.KindOf(err) != errs.Usage {
			t.Fatalf("unreadable: %v", err)
		}
		os.Chmod(good, 0o644)
	}
}

func FuzzParseLocation(f *testing.F) {
	for _, s := range []string{"", "version = 1\n", "default_store = \"a\"\n[aliases]\nb = \"/p\"\n", "[aliases]\n\"\" = \"x\"\n", "version = 9", "a = [", "default_store = \"" + strings.Repeat("z", 40) + "\""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		l, err := ParseLocation([]byte(in), "f")
		if err != nil {
			if k := errs.KindOf(err); k != errs.Usage && k != errs.Conflict {
				t.Fatalf("kind %v for %q: %v", k, in, err)
			}
			return
		}
		data, err := SaveLocation(l)
		if err != nil {
			t.Fatal(err)
		}
		again, err := ParseLocation(data, "f")
		if err != nil || !reflect.DeepEqual(again, l) {
			t.Fatalf("round trip of %+v: %+v %v", l, again, err)
		}
	})
}

// TestReadRefusesSpecialFiles: the config and location readers refuse anything that is not a
// regular file before opening it. A FIFO would block the open and a device (/dev/zero behind a
// symlink) never ends, so the size cap alone would not protect them. Each is a Usage error naming
// the file; a symlink to a regular file still reads.
func TestReadRefusesSpecialFiles(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo.toml")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	zero := filepath.Join(dir, "zero.toml")
	if err := os.Symlink("/dev/zero", zero); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(dir, "good.toml")
	if err := os.WriteFile(good, []byte("version = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.toml")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path, kind string // kind "" = reads fine
	}{
		{"a named pipe is refused", fifo, "a named pipe"},
		{"a symlink to /dev/zero is refused", zero, "a device"},
		{"a directory is refused", dir, "a directory"},
		{"a symlink to a regular file reads", link, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, rerr := Read(tc.path)
			_, lerr := ReadLocation(tc.path)
			for _, err := range []error{rerr, lerr} {
				if tc.kind == "" {
					if err != nil {
						t.Fatalf("want a read, got %v", err)
					}
					continue
				}
				if errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), tc.path) || !strings.Contains(err.Error(), tc.kind) {
					t.Fatalf("want a Usage error naming %s as %s, got %v", tc.path, tc.kind, err)
				}
			}
		})
	}
}
