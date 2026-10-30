package entry

import (
	"errors"
	"strings"
	"testing"
)

// TestParseFrontMatterBoundaries covers the edges of the one-line rules and the fence: block-scalar
// indicators of length 1, 2 and 3, quoting of length 1 and 2, the closing fence with trailing
// space or an extra dash, and a continuation line under a key that is not title or summary.
func TestParseFrontMatterBoundaries(t *testing.T) {
	const ok, invalid, structural = "ok", "invalid", "structural"
	for _, tc := range []struct {
		name      string
		src       string
		want      string // ok | invalid (*ValidationError) | structural (other error)
		wantTitle string
	}{
		{"indicator len 1: rejected", "---\ntitle: |\nsummary: S\n---\n", invalid, ""},
		{"indicator len 2: rejected", "---\ntitle: |-\nsummary: S\n---\n", invalid, ""},
		{"indicator-like len 3: a literal title", "---\ntitle: |-2\nsummary: S\n---\n", ok, "|-2"},
		{"indicator with bad modifier: literal", "---\ntitle: |x\nsummary: S\n---\n", ok, "|x"},
		{"one quote char: literal", "---\ntitle: \"\nsummary: S\n---\n", ok, "\""},
		{"empty quotes: no title", "---\ntitle: \"\"\nsummary: S\n---\n", invalid, ""},
		{"mismatched quotes kept", "---\ntitle: \"a'\nsummary: S\n---\n", ok, "\"a'"},
		{"closing fence with trailing space", "---\ntitle: T\nsummary: S\n--- \t\nbody", ok, "T"},
		{"four dashes do not close", "---\ntitle: T\nsummary: S\n----\nbody", structural, ""},
		{"opening fence needs a newline", "---title: T\n---\n", ok, ""},
		{"continuation under tags is ignored", "---\ntitle: T\nsummary: S\nupdated: x\n  more\n---\n", ok, "T"},
		{"empty block", "---\n---\n", invalid, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fm, _, err := ParseFrontMatter([]byte(tc.src))
			var ve *ValidationError
			got := ok
			switch {
			case errors.As(err, &ve):
				got = invalid
			case err != nil:
				got = structural
			}
			if got != tc.want {
				t.Fatalf("outcome %s (err %v), want %s", got, err, tc.want)
			}
			if got == ok && fm.Title != tc.wantTitle {
				t.Fatalf("title = %q, want %q", fm.Title, tc.wantTitle)
			}
		})
	}
}

func TestValidationErrorMessage(t *testing.T) {
	if got := (&ValidationError{Reason: "r"}).Error(); got != "r" {
		t.Errorf("no path: %q", got)
	}
	if got := (&ValidationError{Path: "data/a.md", Reason: "r"}).Error(); got != "data/a.md: r" {
		t.Errorf("with path: %q", got)
	}
}

// TestTitleSummaryLimits is Q20: the title and summary caps at, below and above the limit
// (counted in runes, so the rows use a two-byte rune), and the character rules (control
// characters, invalid UTF-8) that make kb add and kb reindex refuse an entry.
func TestTitleSummaryLimits(t *testing.T) {
	fm := func(title, summary string) string {
		return "---\ntitle: " + title + "\nsummary: " + summary + "\n---\n\nbody\n"
	}
	rep := func(n int) string { return strings.Repeat("é", n) }
	for _, tc := range []struct {
		name, src string
		wantOK    bool
	}{
		{"title at MaxTitleRunes", fm(rep(MaxTitleRunes), "s"), true},
		{"title MaxTitleRunes-1", fm(rep(MaxTitleRunes-1), "s"), true},
		{"title MaxTitleRunes+1", fm(rep(MaxTitleRunes+1), "s"), false},
		{"summary at MaxSummaryRunes", fm("t", rep(MaxSummaryRunes)), true},
		{"summary MaxSummaryRunes-1", fm("t", rep(MaxSummaryRunes-1)), true},
		{"summary MaxSummaryRunes+1", fm("t", rep(MaxSummaryRunes+1)), false},
		{"title with NUL", fm("a\x00b", "s"), false},
		{"title with ESC", fm("a\x1b[31mred", "s"), false},
		{"title with DEL", fm("a\x7fb", "s"), false},
		{"title with C1 NEL", fm("a\u0085b", "s"), false},
		{"title with inner tab", fm("a\tb", "s"), false},
		{"summary with control byte", fm("t", "x\x01y"), false},
		{"title invalid UTF-8", fm("a\xffb", "s"), false},
		{"title with emoji and accents", fm("Café ☕ naïve 🚀", "s"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ParseFrontMatter([]byte(tc.src))
			if tc.wantOK {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want a *ValidationError", err)
			}
		})
	}
}
