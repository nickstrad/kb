package cli

import (
	"os"
	"strings"
	"testing"
)

// TestStalePartialBackupIsReported: an unfinished backup a killed migration left beside the index
// (Q65) shows in `kb migrate status` (text and JSON) and as a doctor warning; the exit codes do
// not change (the file is not a pending migration).
func TestStalePartialBackupIsReported(t *testing.T) {
	t.Parallel()
	root := newTempRoot(t)
	useNoneModel(t, root)
	indexState(t, root, "current")
	partial := testDBPath(root) + ".bak-v3-20260101-120000.partial"
	if err := os.WriteFile(partial, []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, _, code := run(t, "migrate", "status")
	if code != 0 || !strings.Contains(stdout, "unfinished backups") || !strings.Contains(stdout, partial) {
		t.Errorf("status: exit %d, want 0 listing %s:\n%s", code, partial, stdout)
	}
	stdout, _, _ = run(t, "migrate", "status", "--json")
	if !strings.Contains(stdout, `"partials"`) || !strings.Contains(stdout, partial) {
		t.Errorf("status --json lacks partials:\n%s", stdout)
	}
	stdout, _, code = run(t, "doctor")
	if code != 0 || !strings.Contains(stdout, "warn migrate: 1 unfinished backup(s)") {
		t.Errorf("doctor: exit %d, want a migrate warning:\n%s", code, stdout)
	}

	if err := os.Remove(partial); err != nil {
		t.Fatal(err)
	}
	if stdout, _, _ = run(t, "migrate", "status"); strings.Contains(stdout, "unfinished") {
		t.Errorf("status after removal still reports:\n%s", stdout)
	}
}
