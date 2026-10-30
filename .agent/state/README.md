# .agent/state: per-run agent logs

One log per agent run, so concurrent agents and the orchestrator can see who is doing what and
which files are taken. Logs are temporary and git-ignored (`.gitignore` here keeps only this
README, `TEMPLATE.md` and itself).

**Name:** `<YYYYMMDD-HHMM>-<agent>-<item>.md`, local time, lowercase kebab-case, for example
`20261029-2105-config-loader-1.3.md`. Names sort by start time.

**Start:** copy `TEMPLATE.md`, fill the front matter, and list every path you will edit in `owns`.
Before you edit, run `bash .agent/scripts/state-status.sh` and do not take a path that another
`active` or `blocked` log owns; ask the orchestrator instead.

**While working:** append timestamped entries to `## Log` (never rewrite earlier ones), add
durable facts to `## Insights`, and update `updated` and `status` (`active`, `blocked`, `done`)
at each step. A log not updated for 30 minutes is flagged as stale.

**Finish and clean up** (the orchestrator, or the agent if it runs alone):

1. Set `status: done` with a final `done:` entry (result, tests run, testing approach).
2. Move each durable insight into `.agent/knowledge/` (new entry or update) and its `index.md`.
3. Move each pending judgment call into `docs/DEFAULTS.md` (local only, never committed).
4. Delete the log.
