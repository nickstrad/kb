# Named stores: layout notes
> Why named stores sit in `.kb/stores/<name>/`, use `data-<name>/`, and why migrations need a file lock.

**Fact.**
1. The store config schema from 1.3 is unchanged under named stores. Only the paths move (workspace fields), so `config.Config` is untouched.
2. The entries layout reads `data/<dir>/README.md` as a directory entry. A named store's default entries folder therefore can't be `data/<name>`: the default store would index it. Named stores use `data-<name>/`.
3. SQLite migration already runs all pending steps in one `BEGIN IMMEDIATE`. `VACUUM INTO` (used for the backup) can't run inside that transaction, so the migrator takes an advisory `flock` around backup + apply.

**Why it matters.** Each of these looks like a free choice but isn't. Changing it reintroduces a bug.

**How to apply.** Read before touching store layout, default sources, or the migrate backup and lock sequence.

**Seen in:** docs/design/named-stores.md, docs/design/migrations.md · 2026-10-30
