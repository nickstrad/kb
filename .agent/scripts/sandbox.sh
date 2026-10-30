#!/usr/bin/env bash
# Throwaway snapshots of the working tree under .agent/sandbox/ for tools that mutate or build code.
set -euo pipefail

usage() {
	cat >&2 <<'USAGE'
usage: bash .agent/scripts/sandbox.sh <command>
  new <name>                    snapshot the working tree (tracked + untracked, not ignored) into
                                .agent/sandbox/<YYYYMMDD-HHMM>-<name>[-N]/ and print its absolute path
  list                          list sandboxes: name, size, age in minutes
  rm <name|path>...             delete sandboxes (only directly under .agent/sandbox/)
  prune [--older-than <dur>]    delete sandboxes older than <dur> (Nm, Nh or Nd; default 24h)
USAGE
	exit 2
}

repo=$(git rev-parse --show-toplevel 2>/dev/null) || { echo "sandbox: not inside a git repository" >&2; exit 2; }
box="$repo/.agent/sandbox"
mkdir -p "$box"

# resolve maps a name or path to an absolute sandbox dir, refusing anything not directly in $box.
resolve() {
	local arg=$1 dir
	if [[ "$arg" == */* ]]; then
		dir=$(cd "$arg" 2>/dev/null && pwd -P) || { echo "sandbox: no such directory: $arg" >&2; return 1; }
	else
		dir="$box/$arg"
		[[ -d "$dir" ]] || { echo "sandbox: no such sandbox: $arg" >&2; return 1; }
		dir=$(cd "$dir" && pwd -P)
	fi
	local real_box
	real_box=$(cd "$box" && pwd -P)
	if [[ "$(dirname "$dir")" != "$real_box" ]]; then
		echo "sandbox: refusing $dir: not directly under $real_box" >&2
		return 1
	fi
	printf '%s\n' "$dir"
}

# age_minutes prints how many whole minutes ago dir was last modified (BSD stat).
age_minutes() {
	local mtime now
	mtime=$(stat -f %m "$1")
	now=$(date +%s)
	echo $(( (now - mtime) / 60 ))
}

cmd=${1:-}
[[ -n "$cmd" ]] || usage
shift

case "$cmd" in
new)
	[[ $# -eq 1 ]] || usage
	name=$1
	[[ "$name" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || { echo "sandbox: bad name $name (use [A-Za-z0-9._-])" >&2; exit 2; }
	base="$box/$(date +%Y%m%d-%H%M)-$name"
	dir=$base
	n=2
	# mkdir (not -p) is atomic, so two agents that pick the same name in the same minute both win
	# a distinct directory.
	until mkdir "$dir" 2>/dev/null; do
		dir="$base-$n"
		n=$((n + 1))
	done
	# Tracked and untracked-but-not-ignored files; tracked files deleted in the working tree are
	# skipped, and nothing under .agent/sandbox is ever copied into itself.
	(cd "$repo" && git ls-files -co --exclude-standard -z) |
		while IFS= read -r -d '' f; do
			[[ "$f" == .agent/sandbox/* ]] && continue
			[[ -e "$repo/$f" || -L "$repo/$f" ]] && printf '%s\0' "$f"
		done |
		rsync -a --from0 --files-from=- "$repo/" "$dir/"
	printf '%s\n' "$dir"
	;;
list)
	found=0
	for d in "$box"/*/; do
		[[ -d "$d" ]] || continue
		found=1
		d=${d%/}
		printf '%-48s %8s %6sm\n' "$(basename "$d")" "$(du -sh "$d" | cut -f1)" "$(age_minutes "$d")"
	done
	[[ $found -eq 1 ]] || echo "no sandboxes"
	;;
rm)
	[[ $# -ge 1 ]] || usage
	for a in "$@"; do
		dir=$(resolve "$a") || exit 1
		chmod -R u+w "$dir" 2>/dev/null || true
		rm -rf "$dir"
		echo "removed $dir"
	done
	;;
prune)
	limit=1440
	if [[ $# -gt 0 ]]; then
		[[ $# -eq 2 && "$1" == --older-than ]] || usage
		case "$2" in
		*m) limit=${2%m} ;;
		*h) limit=$(( ${2%h} * 60 )) ;;
		*d) limit=$(( ${2%d} * 1440 )) ;;
		*) usage ;;
		esac
	fi
	for d in "$box"/*/; do
		[[ -d "$d" ]] || continue
		d=${d%/}
		if (( $(age_minutes "$d") >= limit )); then
			chmod -R u+w "$d" 2>/dev/null || true
			rm -rf "$d"
			echo "removed $d"
		fi
	done
	;;
*)
	usage
	;;
esac
