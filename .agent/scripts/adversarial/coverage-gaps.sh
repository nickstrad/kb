#!/usr/bin/env bash
# List functions not fully covered by a package's own tests (seeds included), for docs/TESTING.md §2.1.
set -euo pipefail

usage() {
	echo "usage: bash .agent/scripts/adversarial/coverage-gaps.sh [-t threshold%] [-k] <pkg dir>..." >&2
	echo "  prints every function below threshold (default 100) per package; -k keeps the sandbox" >&2
	exit 2
}

threshold=100
keep=0
while getopts "t:k" o; do
	case $o in
	t) threshold=$OPTARG ;;
	k) keep=1 ;;
	*) usage ;;
	esac
done
shift $((OPTIND - 1))
[[ $# -ge 1 ]] || usage

repo=$(git rev-parse --show-toplevel)
box=$(bash "$repo/.agent/scripts/sandbox.sh" new coverage | tail -1)
cleanup() { [[ $keep -eq 1 ]] && echo "kept $box" >&2 || bash "$repo/.agent/scripts/sandbox.sh" rm "$box" >/dev/null; }
trap cleanup EXIT

status=0
for pkg in "$@"; do
	pkg=${pkg#./}
	pkg=${pkg%/}
	prof="$box/_adv/cover-$(echo "$pkg" | tr / -).out"
	mkdir -p "$box/_adv"
	echo "== $pkg (functions below ${threshold}%; seeds and crashers replay as tests)"
	if ! (cd "$box" && GOPROXY=off GOWORK=off go test -tags fts5 -count=1 -covermode=set -coverprofile="$prof" "./$pkg/" >/dev/null 2>&1); then
		echo "   tests fail; run go test -tags fts5 ./$pkg/ for details"
		status=1
		continue
	fi
	(cd "$box" && go tool cover -func="$prof") |
		awk -v t="$threshold" '$1 != "total:" { p = $NF; sub("%", "", p); if (p + 0 < t + 0) printf "   %-6s %-40s %s\n", $NF, $2, $1 } $1 == "total:" { total = $NF } END { print "   total " total }'
done
exit $status
