//go:build harnessfake

package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/kb/internal/store/sqlite"
)

func TestFakeModel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		id  string
		dim int
	}{{"fake:16", 16}, {"fake:1", 1}, {"fake:4096", 4096}, {"fake:0", 0}, {"fake:4097", 0}, {"fake:016", 0}, {"fake:x", 0}, {"fake16", 0}, {"BAAI/bge-small-en-v1.5", 0}} {
		e, ok := fakeModel(tc.id)
		if ok != (tc.dim > 0) || (ok && (e.Dim() != tc.dim || e.Model() != tc.id)) {
			t.Errorf("fakeModel(%q) = %v, %v; want dim %d", tc.id, e, ok, tc.dim)
		}
	}
	if knownModel("fake:8") != nil {
		t.Error("knownModel refuses fake:8 in a harnessfake build")
	}
}

func TestHarnessClock(t *testing.T) {
	t.Parallel()
	pinned := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, val string
		set       bool
		pinned    bool
		warn      bool
	}{
		{"set", "2026-03-01T09:00:00Z", true, true, false},
		{"set with offset", "2026-03-01T10:00:00+01:00", true, true, false},
		{"unset", "", false, false, false},
		{"empty", "", true, false, false},
		{"garbage", "yesterday", true, false, true},
		{"date only", "2026-03-01", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var warn bytes.Buffer
			clock := harnessClock(func(k string) (string, bool) {
				if k != "KB_HARNESS_NOW" {
					t.Errorf("read %q", k)
				}
				return tc.val, tc.set
			}, &warn)
			if (clock != nil) != tc.pinned {
				t.Fatalf("clock set = %v, want %v", clock != nil, tc.pinned)
			}
			if tc.pinned && !clock().Equal(pinned) {
				t.Errorf("clock() = %v, want %v", clock(), pinned)
			}
			if got := strings.Contains(warn.String(), "not RFC3339"); got != tc.warn {
				t.Errorf("warning = %q, want warning %v", warn.String(), tc.warn)
			}
		})
	}
	d := productionDeps()
	d.now = harnessClock(func(string) (string, bool) { return "2026-03-01T09:00:00Z", true }, nil)
	if !d.clock().Equal(pinned) {
		t.Errorf("deps.clock() = %v, want the pinned time", d.clock())
	}
}

// TestHarnessProbe: KB_HARNESS_INDEX_PROBE=1, and only that value, makes the build one index
// release ahead; productionDeps carries the choice into openStore and the migrate adapter.
func TestHarnessProbe(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		val   string
		set   bool
		probe bool
	}{{"1", true, true}, {"", false, false}, {"", true, false}, {"0", true, false}, {"true", true, false}} {
		got := harnessProbe(func(string) (string, bool) { return tc.val, tc.set })
		want := sqlite.SchemaVersion()
		if tc.probe {
			want++
		}
		if (got != nil) != tc.probe || stepsTarget(got) != want {
			t.Errorf("KB_HARNESS_INDEX_PROBE=%q (set %v): target %d, want %d", tc.val, tc.set, stepsTarget(got), want)
		}
	}
}

func stepsTarget(s sqlite.Steps) int {
	if s == nil {
		return sqlite.SchemaVersion()
	}
	return s.Target()
}
