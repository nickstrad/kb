---
title: Daemons started from tmux die when root's last login ends
summary: A daemon launched from a tmux pane stays in root's systemd user manager cgroup and is SIGTERMed when the last root session closes; run long-lived servers as system units instead.
tags: [droplet, systemd, tmux, logind, postgres]
updated: 2026-09-13
verified: 2026-09-13 — Ubuntu 24.04.4, systemd 255, tmux 3.4; cgroups read live, journal correlated with two PostgreSQL shutdowns
---

# Daemons started from tmux die when root's last login ends

tmux 3.4 on this droplet puts every pane in its own transient scope under root's systemd user
manager: `user.slice/user-0.slice/user@0.service/tmux-spawn-<uuid>.scope`. Forking or
daemonizing (for example `pg_ctl start`, `nohup`, `&`) does not leave that cgroup. Root has
`Linger=no`. When root's last login session goes away, logind stops `user@0.service` (about 10
seconds later), and systemd sends SIGTERM to everything in its scopes, including daemons that
outlived their pane. `KillUserProcesses=no` in `logind.conf` does not prevent this.

## Why it matters / what bites you

A server works for hours or days, then disappears without a crash. Its own log shows a clean,
externally requested shutdown with no clue about the sender. PostgreSQL logs
`received smart shutdown request`, then later clients get
`connection to server on socket "/tmp/.s.PGSQL.5440" failed: No such file or directory`.
It happens when the tmux server exits and no other root SSH session is open, so it looks random.

## How to do it

Find out where a process really lives:

```bash
cat /proc/<pid>/cgroup                       # user@0.service/tmux-spawn-*.scope = at risk
loginctl show-user root | grep Linger        # Linger=no on this droplet
```

Match a mystery shutdown to a session teardown in the journal (same second as the server's log line):

```bash
journalctl --since "<time-1min>" --until "<time+1min>" --no-pager -o short-iso \
  | grep -E 'Removed session|Stopping user@0.service|tmux-spawn'
```

Seen output pattern:

```text
systemd-logind[808]: Removed session 1309.
systemd[1]: Stopping user@0.service - User Manager for UID 0...
systemd[1443]: Stopping tmux-spawn-692785d4-...scope - tmux child pane 333062 launched by process 1750...
```

Fix: run the server as a system unit so it sits in `system.slice` and starts at boot. For a
PostgreSQL cluster, use `Type=notify` with `postgres -D <datadir>` directly, not `pg_ctl`.
Ubuntu's build links libsystemd. The working unit is `/etc/systemd/system/pglab.service`; see
[postgres-learner-lab.md](postgres-learner-lab.md).

## Edge cases

- `loginctl enable-linger root` keeps `user@0.service` alive and would also protect tmux-spawned
  jobs. It was not applied here. A system unit is the better fix for a server that must survive
  reboots. Unverified: behavior with linger enabled on this box.
- Starting the server again by hand from a tmux pane brings the problem back. Use
  `systemctl start <unit>`.
- Claude Code and Codex sessions in tmux panes live in the same kind of scope. They also die if
  the tmux server and every SSH session end, but that no longer affects system-unit services.

## Seen in

On 2026-09-12 the learner PostgreSQL lab stopped at 19:46:37 and 23:43:30 UTC. Both times were
exact matches for `Stopping user@0.service`. It had run for 8 days because some root session had
stayed open throughout.
