---
name: leverage-check
description: This skill should be used when the user asks for a "leverage check", "leverage pass", to "validate the leverage folder", "check leverage points", "are the leverage entries still accurate", "what are our weakest points", "find new high-leverage risks", "should this leverage item be archived", or at every PLAN wave-end pass. Validates .agent/leverage/ against the current codebase (paths, symbols, tests, harness scenarios, measurements), finds entries whose area was refactored, redesigned or removed and should be archived, spots new candidates, and reports a table. Updates or archives entries only when asked or when run as part of a wave-end pass.
---

# Leverage check

**Source of truth:** `.agent/leverage/README.md` (entry format, the cap of about 10 entries,
retiring), `.agent/leverage/archive/README.md` (how to archive), and AGENTS.md "leverage/".
Cite them; don't restate them.

**Default mode is report only.** Edit entries, archive them or add new ones only if the user
asks, or when you run as part of a wave-end pass (docs/PLAN.md §6.4 rule 4).

## 1. Scope

- Every active entry in `.agent/leverage/index.md`.
- The code changed since each entry's **Last reviewed** date:
  `git log --since=<date> --name-only -- <paths in Where>`, plus the uncommitted
  `git diff --name-only`.
- For candidate discovery: the diff against main, or the path or package the user names.

## 2. Validate each active entry

A grep hit is a lead. Read the code before you conclude anything.

| # | Check | How |
|---|---|---|
| 1 | **Where still exists** | Every file, package and symbol in **Where** resolves (`ls`, `rg -n '<Symbol>'`). If something is missing, check whether it was renamed or moved (`git log --follow`, `rg`) or removed. |
| 2 | **Weakness still present** | Re-read the code behind **Why it matters**. Was it refactored, redesigned or stripped out so the weakness is gone (e.g. the race is now impossible by construction, the feature was deleted, the limit was removed)? |
| 3 | **Evidence still true** | Queue ids (Q/H) still open, or done? Knowledge entries still exist? Measurements still current, or has the code on that path changed since? |
| 4 | **Coverage claims hold** | Named tests exist and pass: `go test -tags fts5 -run '<Name>' ./<pkg>/`. Named harness scenarios exist in `.agent/scripts/adversarial/harness`. Run cheap ones; run the harness only in a sandbox (docs/ADVERSARIAL_TESTING.md). |
| 5 | **Next checks** | Are any already done (move them into Evidence or Coverage)? Are any now due because the wave touched the area (propose an H entry, a queue entry or a PLAN note)? |
| 6 | **Status is right** | Choose `watch`, `needs work` or `validated (<date>)` from what checks 1–5 found. |

## 3. Decide per entry

- **Keep**: accurate; at most bump **Last reviewed**.
- **Update**: fix paths and symbols, evidence, coverage, next checks and status.
- **Narrow**: part of the weakness is resolved. Shrink the entry to the rest.
- **Archive**: the area was refactored or redesigned so the weakness is gone, stripped out of the
  code, or outranked by something new. Follow `archive/README.md`: status `retired (<reason>,
  <date>)`, a **Resolution** line, move the file to `archive/`, and update both indexes.
- **Merge**: two entries now describe the same weakness.

## 4. Look for new candidates (sparingly)

Look for the patterns that made the current entries:
- a new shared resource or SQLite handle, cache file or lock;
- new goroutines or fan-out;
- new network, filesystem or process boundaries;
- loops whose cost grows with corpus size;
- new one-way state changes (migrations, deletes);
- new external dependencies.

Propose a candidate only if it outranks the weakest active entry, or the folder is under about
10 entries. Say which entry it would displace.

## 5. Verify (when editing)

`go run ./.agent/scripts/index-check` must print "indexes in sync". Every archived entry has a
Resolution line, and `.agent/leverage/index.md` lists only active entries plus the archive link.

## 6. Report

Start with one line: the number of entries checked, and how many to keep, update, narrow,
archive or merge, plus candidates. Then one table, with archives and **needs work** first:

| Entry | Status (claimed → actual) | Finding | Action | Evidence |
|---|---|---|---|---|
| sqlite-write-contention | needs work → validated | Q22 fixed; `concurrent-fresh-db` passes 20/20 | Update | internal/reindex/reindex.go:<line>, harness run |
| <name> | watch → retired | `<Symbol>` removed in item 1.2; weakness gone by design | Archive | git log -- <path> |

After the table, at most three lines:
- new candidates, each with the entry it would displace;
- "next checks" that are now due, and where you routed them (H, Q or PLAN);
- anything the owner should decide, such as a pivot or an architecture-level question (ask; don't decide).
