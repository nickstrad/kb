#!/usr/bin/env bash
# List .agent/state/ logs (agent, item, status, updated), flag stale ones and files owned twice.
set -euo pipefail

# Usage: state-status.sh [stale-minutes]   (default 30; macOS BSD date)
# A path marked "(append only)" in owns is shared: several logs may list it, and it is listed as
# info, not as a CONFLICT.
stale_min="${1:-30}"
dir="$(cd "$(dirname "$0")/../state" && pwd)"
now="$(date +%s)"

field() { # field <file> <key>: value of "key:" in the front matter
  awk -v k="$2" 'NR==1 && $0=="---" {fm=1; next} fm && $0=="---" {exit}
    fm && index($0, k ":")==1 {sub("^" k ":[ ]*", ""); print; exit}' "$1"
}

shopt -s nullglob
logs=()
for f in "$dir"/*.md; do
  case "$(basename "$f")" in README.md|TEMPLATE.md) continue ;; esac
  logs+=("$f")
done
if [ ${#logs[@]} -eq 0 ]; then
  echo "no state logs in $dir"
  exit 0
fi

printf '%-8s %-16s %-7s %-20s %-30s %s\n' FLAG UPDATED STATUS AGENT ITEM FILE
owned=""
shared=""
for f in "${logs[@]}"; do
  status="$(field "$f" status)"; updated="$(field "$f" updated)"
  flag=""
  if ts="$(date -j -f '%Y-%m-%d %H:%M' "$updated" +%s 2>/dev/null)"; then :; else
    ts="$(stat -f %m "$f")"; flag="no-date"
  fi
  if [ "$status" != "done" ] && [ $(( (now - ts) / 60 )) -ge "$stale_min" ]; then flag="STALE"; fi
  printf '%-8s %-16s %-7s %-20s %-30s %s\n' "${flag:--}" "${updated:-?}" "${status:-?}" \
    "$(field "$f" agent)" "$(field "$f" item | cut -c1-30)" "$(basename "$f")"
  if [ "$status" = "active" ] || [ "$status" = "blocked" ]; then
    paths="$(field "$f" owns | tr ',' '\n' | sed 's/^ *//; s/ *$//' | grep -v '^$' || true)"
    owned+="$(printf '%s\n' "$paths" | grep -v '(append only)$' | sed "s|\$|	$(basename "$f")|" || true)"$'\n'
    shared+="$(printf '%s\n' "$paths" | grep '(append only)$' | sed 's| *(append only)$||' \
      | sed "s|\$|	$(basename "$f")|" || true)"$'\n'
  fi
done

dups="$(printf '%s' "$owned" | cut -f1 | grep -v '^$' | sort | uniq -d || true)"
if [ -n "$dups" ]; then
  echo
  echo "CONFLICT: paths owned by more than one active/blocked log:"
  while IFS= read -r p; do
    printf '  %s: %s\n' "$p" "$(printf '%s' "$owned" | awk -F'\t' -v p="$p" '$1==p {print $2}' | paste -sd, -)"
  done <<< "$dups"
fi

shared_paths="$(printf '%s' "$shared" | cut -f1 | grep -v '^$' | sort -u || true)"
if [ -n "$shared_paths" ]; then
  echo
  echo "INFO: append-only paths (shared, not a conflict):"
  while IFS= read -r p; do
    printf '  %s: %s\n' "$p" "$(printf '%s' "$shared" | awk -F'\t' -v p="$p" '$1==p {print $2}' | paste -sd, -)"
  done <<< "$shared_paths"
fi
