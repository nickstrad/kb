package cli

import (
	"runtime/debug"
	"testing"
)

func TestFormatVersion(t *testing.T) {
	t.Parallel()
	devel := func(settings ...debug.BuildSetting) *debug.BuildInfo {
		return &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: settings}
	}
	rev := debug.BuildSetting{Key: "vcs.revision", Value: "0123456789abcdef0123"}
	dirty := debug.BuildSetting{Key: "vcs.modified", Value: "true"}
	clean := debug.BuildSetting{Key: "vcs.modified", Value: "false"}

	tests := []struct {
		name    string
		stamped string
		bi      *debug.BuildInfo
		ok      bool
		want    string
	}{
		{"stamped wins", "v1.2.3", devel(rev), true, "v1.2.3"},
		{"no build info", "dev", nil, false, "dev"},
		{"tagged install", "dev", &debug.BuildInfo{Main: debug.Module{Version: "v0.4.0"}, Settings: []debug.BuildSetting{rev}}, true, "v0.4.0"},
		{"devel clean", "dev", devel(rev, clean), true, "(devel) 0123456789ab"},
		{"devel dirty", "", devel(rev, dirty), true, "(devel) 0123456789ab-dirty"},
		{"devel without vcs", "dev", devel(), true, "(devel)"},
		{"empty main version", "dev", &debug.BuildInfo{}, true, "(devel)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatVersion(tt.stamped, tt.bi, tt.ok); got != tt.want {
				t.Errorf("formatVersion = %q, want %q", got, tt.want)
			}
		})
	}
}
