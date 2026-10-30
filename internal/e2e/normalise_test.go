package e2e

import "testing"

func TestNormalise(t *testing.T) {
	const work = "/private/var/T/TestX/001"
	for _, tc := range []struct{ name, in, want string }{
		{"work dir", "created " + work + "/proj/.kb", "created $WORK/proj/.kb"},
		{"work dir, unresolved spelling", "at /var/T/TestX/001/x", "at $WORK/x"},
		{"version", "kb v0.0.0-20261030193210-73e9bc279655\n", "kb (version)\n"},
		{"version only after kb", "tool v1.2", "tool v1.2"},
		{"rfc3339", "search at 2026-10-30T19:23:02Z and 2026-10-30T19:23:02.5+02:00", "search at (time) and (time)"},
		{"date alone stays", "updated 2026-10-30", "updated 2026-10-30"},
		{"backup suffix", "kb.sqlite.bak-v5-20261030-193210 kb.sqlite.bak-v6-20261030-193210-2", "kb.sqlite.bak-v5-(time) kb.sqlite.bak-v6-(time)"},
		{"sizes", "88.0 KiB, 12 B, 1.5 MiB, 3 GiB", "(size), (size), (size), (size)"},
		{"size needs a unit word", "12 Bytes", "12 Bytes"},
		{"ms json", `"latency_ms": 12, "embed_ms":3.5`, `"latency_ms": (ms), "embed_ms":(ms)`},
		{"ms inline", "took 12ms and 3.5ms", "took (ms) and (ms)"},
		{"ms word stays", "5 items, 2msg", "5 items, 2msg"},
		{"incident id", "internal error (id 0a1b2c3d). Details", "internal error (id (id)). Details"},
		{"short id stays", "id 12", "id 12"},
		{"no change", "plain text\n", "plain text\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Normalise(tc.in, work, "/var/T/TestX/001"); got != tc.want {
				t.Errorf("Normalise(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
