#!/usr/bin/env bash
# Run packages' tests N times with -race and -shuffle in a sandbox and report any test that ever failed.
set -euo pipefail

usage() {
	echo "usage: bash .agent/scripts/adversarial/flaky.sh [-n count] [-k] <pkg dir>..." >&2
	exit 2
}

count=20
keep=0
while getopts "n:k" o; do
	case $o in
	n) count=$OPTARG ;;
	k) keep=1 ;;
	*) usage ;;
	esac
done
shift $((OPTIND - 1))
[[ $# -ge 1 ]] || usage

repo=$(git rev-parse --show-toplevel)
box=$(bash "$repo/.agent/scripts/sandbox.sh" new flaky | tail -1)
cleanup() { [[ $keep -eq 1 ]] && echo "kept $box" >&2 || bash "$repo/.agent/scripts/sandbox.sh" rm "$box" >/dev/null; }
trap cleanup EXIT

pkgs=()
for p in "$@"; do
	p=${p#./}
	pkgs+=("./${p%/}/")
done
mkdir -p "$box/_adv"
log="$box/_adv/flaky.log"
echo "running ${pkgs[*]} x$count with -race -shuffle=on ..." >&2
set +e
(cd "$box" && GOPROXY=off GOWORK=off go test -tags fts5 -race -shuffle=on -count="$count" "${pkgs[@]}" >"$log" 2>&1)
rc=$?
set -e
fails=$(grep -E '^\s*--- FAIL: ' "$log" | sed -E 's/^[[:space:]]*--- FAIL: ([^ ]+).*/\1/' | sort | uniq -c | sort -rn || true)
races=$(grep -c 'WARNING: DATA RACE' "$log" || true)
if [[ $rc -eq 0 ]]; then
	echo "no failures in $count runs; data races: $races"
	exit 0
fi
echo "failures (count of $count runs):"
echo "${fails:-  (none parsed; see shuffle seed and output below)}"
echo "data races: $races"
grep -m3 -E '^-test.shuffle' "$log" | sed 's/^/  /' || true
grep -m20 -E 'FAIL|panic' "$log" | sed 's/^/  /'
exit 1
