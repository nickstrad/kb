package e2e

import (
	"regexp"
	"strings"
)

// The fixed normaliser list (docs/TESTING.md §11.3). Extending it is a reviewed change.
var (
	reVersion  = regexp.MustCompile(`\bkb v[0-9][^\s]*`)
	reTime     = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})`)
	reBackup   = regexp.MustCompile(`(\.bak-v\d+)-\d{8}-\d{6}(?:-\d+)?`)
	reSize     = regexp.MustCompile(`\b\d+(?:\.\d+)? (?:B|KiB|MiB|GiB)\b`)
	reMsField  = regexp.MustCompile(`("[A-Za-z0-9_]*_ms": ?)-?\d+(?:\.\d+)?`)
	reMsInline = regexp.MustCompile(`\b\d+(?:\.\d+)?ms\b`)
	reID       = regexp.MustCompile(`\bid [0-9a-f]{8,}\b`)
)

// Normalise rewrites volatile text in out so it can be compared with a transcript. work is the
// journey's temp dir; alt are other spellings of it (for example the unresolved symlink path).
func Normalise(out, work string, alt ...string) string {
	for _, w := range append([]string{work}, alt...) {
		if w != "" {
			out = strings.ReplaceAll(out, w, "$WORK")
		}
	}
	out = reVersion.ReplaceAllString(out, "kb (version)")
	out = reBackup.ReplaceAllString(out, "$1-(time)")
	out = reTime.ReplaceAllString(out, "(time)")
	out = reMsField.ReplaceAllString(out, "${1}(ms)")
	out = reMsInline.ReplaceAllString(out, "(ms)")
	out = reSize.ReplaceAllString(out, "(size)")
	out = reID.ReplaceAllString(out, "id (id)")
	return out
}
