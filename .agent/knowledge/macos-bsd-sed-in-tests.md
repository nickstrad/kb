# macOS sed is BSD: no GNU `sed -i` in tests

> `sed -i 's/a/b/' f` fails on macOS (BSD sed takes the script as a backup suffix); tests use a generated POSIX-sh fake editor.

**Fact.** BSD `sed -i` requires a suffix argument (`sed -i '' …`), so GNU-style `EDITOR="sed -i
's/…/…/'"` broke the CLI edit tests on macOS. `fakeEditor` in internal/cli/add_test.go writes a
`#!/bin/sh` script that runs `sed 's/from/to/' "$1" > "$1.tmp" && mv "$1.tmp" "$1"`. The target
fix is an injected Go `editor` func in `deps` (CODING_STYLE §5, TESTING §7).

**Why it matters.** Development and CI are macOS-only; GNU-only shell in tests fails there.

**How to apply.** In tests and `.agent/scripts`, avoid `sed -i`, `date -d`, `readlink -f` and other
GNU flags; prefer Go, or POSIX forms (`date -j -f` is the BSD parser). Mismatch tests must
also check the file actually changed.

**Seen in:** internal/cli/add_test.go `useFakeEditor` (replaced the sh+sed `fakeEditor`, W3 pass); docs/TESTING.md §7; docs/DEFAULTS.md row 18 · 2026-10-29
