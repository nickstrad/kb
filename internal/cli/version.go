package cli

import (
	"runtime/debug"
	"strings"
)

// ResolveVersion is the version string `kb version` prints and the search log records. A value
// stamped at link time (-ldflags "-X main.version=v1.2.3", used by release builds) wins; otherwise
// it comes from the Go build info: the module version (v1.2.3 for `go install …@v1.2.3`,
// "(devel)" for a local build) plus the VCS revision when the build recorded one.
func ResolveVersion(stamped string) string {
	bi, ok := debug.ReadBuildInfo()
	return formatVersion(stamped, bi, ok)
}

func formatVersion(stamped string, bi *debug.BuildInfo, ok bool) string {
	if stamped != "" && stamped != "dev" {
		return stamped
	}
	if !ok || bi == nil {
		return "dev"
	}
	v := bi.Main.Version
	if v == "" {
		v = "(devel)"
	}
	var rev string
	var dirty bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	// A tagged module version already names the commit; the revision only adds information to a
	// development build.
	if rev == "" || !strings.HasPrefix(v, "(devel)") {
		return v
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "-dirty"
	}
	return v + " " + rev
}
