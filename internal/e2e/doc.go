// Package e2e is the end-to-end journey runner (docs/TESTING.md §11, PLAN Phase 7).
//
// A journey is a cram-style transcript in testdata/journeys/<slug>.txt: prose, then commands
// ("  $ kb search ...") with their expected stdout, "[stderr] "-prefixed stderr lines and an
// "[exit N]" line for a non-zero exit. The runner (stdlib only) builds nothing itself: the
// journeys test (build tag e2e) builds kb once with -tags fts5,harnessfake and hands the
// binary to Runner. Everything here except journeys_test.go runs in a plain go test.
//
// Transcript format
//
//	prose: any unindented line
//	  $ cmd args            a command; first word kb, or a builtin
//	  output line           expected output, two-space indent
//	  [stderr] text         a stderr line (all stdout lines come first)
//	  [exit N]              present only when N != 0
//	  ...                   matches any number of lines (including none)
//	  ~ RE                  matches one line by regexp (whole line)
//	  [stderr] ~ RE         matches one stderr line whose text matches RE
//	-- path --              starts a file section (to the end of the file or the next section)
//
// Blank lines inside an output block are written as empty lines when more output follows and as
// two spaces when they end the block. File sections are extracted under $WORK before the run,
// except a name starting with "@", which only "write DEST @name" uses.
//
// Builtins: cd DIR, env K=V (K= unsets; KB_HARNESS_NOW=+200d moves the clock on), write DEST
// [SECTION], rm [-r] PATH, mkdir PATH, cat PATH, exists GLOB, ! exists GLOB, grep RE PATH,
// mkfixture ARTEFACT-vN PATH. Builtin failures print "[stderr]" text and exit 1, so a
// transcript shows them like any command.
package e2e
