# A size cap needs a regular-file check
> `os.Stat` + size check + `os.ReadFile` is not a cap: a symlink to /dev/zero stats as 0 bytes and never ends, and a FIFO blocks the open. Refuse non-regular files first, then read through `io.LimitReader(max+1)`.

**Fact:** Q49 capped `config.Read` with a stat size check before `os.ReadFile`. Character
devices report size 0 (so `/dev/zero` passes and `ReadFile` grows without bound), a FIFO's `open`
blocks until a writer appears, and a regular file can grow between the stat and the read. The
discovery walk only accepts a regular `kb.toml`, but `--root DIR` reads `DIR/.kb/kb.toml` directly,
so a cloned repo with `kb.toml -> /dev/zero` hung any command run with `--root`.

**Why it matters:** every small file kb owns (store `config.toml`, `.kb/kb.toml`, later the
migration and eval files) is attacker- or accident-controlled input from a checkout.

**How to apply:** use `config.readCapped(op, what, path)`: stat (following symlinks), refuse
`!Mode().IsRegular()` naming the kind (`fileKind`), refuse `Size() > MaxFileBytes`, then read with
`io.LimitReader(f, MaxFileBytes+1)` and refuse a result over the cap. Test with `syscall.Mkfifo`
and `os.Symlink("/dev/zero", …)` (stdlib only, no host tools): `TestReadRefusesSpecialFiles`.

**Seen in:** internal/config/config.go (`readCapped`), internal/config/location.go
(`ReadLocation`) · 2026-10-30 (W6 wave-end pass)
