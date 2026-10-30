# Config package: one loader, strict schema, precedence

> internal/config owns the schema; cli.loadConfig(cmd, ws) is the only place a config file is read; the model table is checked by the caller.

**Fact.** `config.Read(ws.ConfigPath)` decodes onto `Default()` (BurntSushi), rejects unknown keys and
bad types with Usage errors that name file and key, then `Validate`. A missing file is the defaults.
`CheckModel` takes the supported-model lookup as a function (config imports no embedder). Precedence
today: flag (`-k`, `--caller`) / `--set` > store config > defaults; there is no global-config layer.
KB_CALLER and the `--embedder`/`--embed-model` flags are gone; `$EDITOR` is read only in `runEditor`
(config `[edit] editor` wins). `reindex.preconditions` returns a Conflict for a recorded-but-different
embed_meta (assertion only for "never recorded"). The embed_meta model plays no part in choosing the model.
`Parse` reads `version` before the strict decode: newer than `SchemaVersion` is Usage "written by a newer kb", older is Conflict "run kb migrate up" (no migration exists yet), so a foreign-version file never reads as "unknown key". `Apply` strips the decoder's `toml: line N` prefix from type errors. `testdata/default.toml` is the golden of every default.
Reserved keys with an index-changing meaning (embed.dim, embed.int8, [[sources]]) are rejected until wired.

**Why it matters.** Wiring a reserved key means deleting its rejection in `Validate` and a test row.
`config.MinK/MaxK`, `DefaultModel` and the default open window repeat constants of search/local/store;
`TestConfigConstantsMatchServices` pins them.

**How to apply.** CLI tests write config with `writeConfig`/`useNoneModel`; the harness uses `kb config --path`.
New settings: add to the struct, `Default`, `Validate`, a boundary row, the doc.go wired/reserved list.

**Seen in:** internal/config, internal/cli/setup.go, internal/cli/config_cmd.go · 2026-10-30
