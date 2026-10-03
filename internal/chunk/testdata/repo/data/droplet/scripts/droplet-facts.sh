#!/usr/bin/env bash
# Print the live values behind droplet/README.md, so the doc can be checked or refreshed.
set -euo pipefail

section() { printf '\n== %s ==\n' "$1"; }

section "Identity"
printf 'vendor:   %s\n' "$(cat /sys/class/dmi/id/sys_vendor 2>/dev/null || echo unknown)"
printf 'product:  %s\n' "$(cat /sys/class/dmi/id/product_name 2>/dev/null || echo unknown)"
printf 'hostname: %s\n' "$(hostname)"
printf 'os:       %s\n' "$(. /etc/os-release && echo "$PRETTY_NAME")"
printf 'kernel:   %s %s\n' "$(uname -r)" "$(uname -m)"
printf 'timezone: %s\n' "$(timedatectl show -p Timezone --value 2>/dev/null || cat /etc/timezone)"

section "Hardware"
printf 'vcpu:     %s (%s)\n' "$(nproc)" "$(awk -F': ' '/model name/{print $2; exit}' /proc/cpuinfo)"
free -h | awk 'NR<=3'
df -h / | awk 'NR==1 || NR==2'

section "Network"
ip -4 -o addr show | awk '{print $2, $4}'

section "Listening sockets"
ss -tlnp 2>/dev/null || echo "ss unavailable"

section "Running services"
systemctl list-units --type=service --state=running --no-pager --no-legend | awk '{print $1}'

section "Toolchain"
for probe in "psql --version" "sqlite3 --version" "node --version" "python3 --version" \
             "go version" "git --version" "docker --version"; do
  cmd=${probe%% *}
  if command -v "$cmd" >/dev/null 2>&1; then
    printf '%-8s %s\n' "$cmd" "$($probe 2>&1 | head -1)"
  else
    printf '%-8s not installed\n' "$cmd"
  fi
done

section "Recent OOM kills (no swap on this box)"
dmesg -T 2>/dev/null | grep -i 'killed process' | tail -5 || echo "none found (or dmesg needs root)"
