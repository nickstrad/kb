---
agent: <short-name, matches the file name>
model: <model id, e.g. claude-opus-5-5>
item: <docs/PLAN.md ref and title, e.g. PLAN 1.3 config loader>
status: active
started: <YYYY-MM-DD HH:MM>
updated: <YYYY-MM-DD HH:MM>
owns: <comma-separated repo paths or globs this run edits, e.g. internal/config/*.go, docs/PLAN.md>
---

# <item title>

Goal: <one line: what this run delivers>

## Log

Append only. Newest last. One entry per step, decision, blocker or handoff.

- <YYYY-MM-DD HH:MM> started: <plan in one or two lines>
- <YYYY-MM-DD HH:MM> decision: <what, default picked, alternative> -> docs/DEFAULTS.md candidate
- <YYYY-MM-DD HH:MM> blocked: <on what or whom>
- <YYYY-MM-DD HH:MM> done: <result; tests run; testing approach per docs/TESTING.md>

## Insights

Durable facts a future agent should know (become `.agent/knowledge/` entries at cleanup).

- 
