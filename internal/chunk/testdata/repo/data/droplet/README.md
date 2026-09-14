---
title: This droplet at a glance
summary: Hardware, OS, network shape, and installed toolchain of the DigitalOcean droplet everything here runs on.
tags: [droplet, digitalocean, ubuntu, hardware, networking]
updated: 2026-09-11
verified: 2026-09-11 — every value below read live from the box
---

# This droplet at a glance

The machine all of this knowledge is about. Re-run `scripts/droplet-facts.sh` to refresh these
numbers; they drift as packages update and volumes get resized.

## Identity and hardware

| | |
| --- | --- |
| Provider | DigitalOcean (`/sys/class/dmi/id/sys_vendor` reports `DigitalOcean`, product `Droplet`) |
| Hostname | `ubuntu-PostgreSQL-practice` |
| OS | Ubuntu 24.04.4 LTS (noble), kernel 6.8.0, `x86_64` |
| CPU | 4 vCPU, reported as `DO-Regular` — a shared-CPU droplet, not dedicated |
| Memory | 7.8 GiB, **no swap configured** |
| Root disk | 24 GB on `/dev/vda1` |
| Timezone | `Etc/UTC`. All logs and timestamps are UTC; never assume local time. |

**No swap is the one to remember.** A process that overruns RAM is killed by the OOM killer
instead of slowing down. Large builds, `pg_restore`, and parallel test runs fail abruptly rather
than degrading. Check `dmesg -T | grep -i oom` before blaming the program.

Confirm the provider rather than guessing — DigitalOcean-specific facts only apply on a box where:

```bash
cat /sys/class/dmi/id/sys_vendor   # -> DigitalOcean
ls /etc/cloud/digitalocean.info
```

## Network shape

Three real interfaces, which matters when a service binds an address:

- `eth0` — carries **both** the public IPv4 and a 10.10.0.0/16 address.
- `eth1` — DigitalOcean VPC private networking, 10.136.0.0/16.
- `docker0` — 172.17.0.0/16, the default Docker bridge.

So `0.0.0.0` exposes a service to the public internet. Bind `127.0.0.1` for anything local-only,
and the `eth1` address for droplet-to-droplet traffic inside the VPC.

```bash
ip -4 -o addr show | awk '{print $2, $4}'   # current addresses
ss -tlnp                                    # what is listening, and on which address
```

## DigitalOcean agents that are always running

`do-agent.service` (metrics for the DO control panel) and `droplet-agent.service` (the browser
console / DO SSH key delivery) run by default. They are not something you installed, and killing
them only breaks DO panel features — leave them alone when auditing `systemctl` output.

`unattended-upgrades.service` is also on, so package versions move on their own. That is why
entries in this repo carry a `verified` date.

## Installed toolchain

Versions as of the `verified` date above:

| Tool | Version | Note |
| --- | --- | --- |
| PostgreSQL | 16.15 (Ubuntu build) | server + `psql`; see the Postgres entries |
| SQLite | 3.53.4 | `sqlite3` CLI on `PATH` |
| Node.js | 22.23.2 | |
| Python | 3.12.3 | system Python; no venv active by default |
| Go | 1.26.8 | |
| Git | 2.43.0 | |
| Docker | running (`docker.service` + `containerd.service`) | |

## Where things live

| Path | What it is |
| --- | --- |
| `/root/Raw/knowledge` | this knowledge store |
| `/root/Software/skills-tools` | agent skills and the tutor/curriculum tooling |
| `/root/Research` | per-topic research working directories |
| `/root/README.md` | the user's own scratch cheat-sheet, predates this repo |

## Scripts

- `scripts/droplet-facts.sh` — prints every value in this doc, read live. Run it before trusting
  anything above.
