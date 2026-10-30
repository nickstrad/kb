# Harness scenarios time signals with sleeps and cannot reach the vector paths
> The kb binary exposes no progress point to synchronise on and no deterministic embedder, so interrupt scenarios are probabilistic and hybrid/vec/fallback paths are unreachable black-box.

- **Smell:** shared fixture/order dependence · hard-wired I/O
- **Status:** needs work
- **Where:** `.agent/scripts/adversarial/harness/scenarios.go:scSigint` (`time.Sleep` 80/250/600 ms, then SIGINT), `harness/migrate_scenarios.go` (6 sleep delays before SIGINT on `migrate up`), `withNone` (91 call sites); the binary's embedder choice (`cli/setup.go:buildEmbedder`)
- **Symptoms in tests:**
  - H23: in W3 only the 80 ms SIGINT landed mid-run (6/6 runs). The others arrived after reindex had finished, and `scSigint` only notes "the interrupt path was not exercised" instead of failing.
  - The migrate interrupt scenario fills a 600k-row fixture so that its sleeps can land mid-run.
  - H2, H14, H18: the exit-2 fallback, hybrid, vec and upgraded-without-vectors paths need a real model, so the harness runs everything with model `none` and leaves those contracts to unit tests.
- **Why it happens:** the binary has no observable "I'm at step N" point, and its only embedders are a real model or none.
- **Remedy:** (1) A `--debug` progress line (`reindex: entry N/M`, `migrate: step <id>`) that the harness waits for before it signals. That gives a deterministic mid-run interrupt, and it is useful to users too. (2) A build-tagged harness embedder (`//go:build harness`, model id `fake:<dim>` wrapping `embed/fake`) so the harness binary reaches every vector path without a download. Release builds stay unchanged. (3) Fail, don't note, when an interrupt never lands mid-run. → **H23** / **H14** in the harness queue. The progress line belongs to whichever item next touches reindex output (**5.2**), or a small **new item**. Option (2) needs an owner yes, because it adds a test-only build of the binary.
- **Cost/benefit:** small for (1) and (3), medium for (2). Interrupt scenarios become deterministic, and H2, H14 and H18 become runnable.
- **Last reviewed:** 2026-10-30 · testability first pass
