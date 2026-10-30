# reindex.All's phase order is reachable only through embedder side effects
> The Discover → index → list → re-discover → prune order has no seam, so the Q22 race test plays a second process from inside a fake `Embed` call.

- **Smell:** hidden coupling · decision+effect mixed
- **Status:** resolved (2026-10-30)
- **Where:** `internal/reindex/reindex.go:all` (calls `discover`, a plain wrapper of `entry.Discover`, twice), `internal/reindex/prune_test.go:concurrentAddEmbedder`
- **Symptoms in tests:**
  - `TestAllKeepsEntryAddedDuringRun` makes the Q22 interleaving deterministic by writing a file and calling `One` on a second store handle from inside the first `Embed` call. It depends on `All` embedding something after its first Discover. With `embed.None`, a warm embedding cache (3.3b) or a body-hash skip, the hook never fires. Today the test would then fail, not pass vacuously, but the trigger is unrelated to what the test is about.
  - The prune decision (rows not on disk, minus the entries a second Discover sees) is pure, but it can only be tested through a real filesystem and store.
- **Why it happens:** `all` takes no discovery function and computes the prune set inline.
- **Remedy:** (1) `Options.Discover func(root, sources) ([]entry.Entry, []entry.Skipped, error)` (nil = `entry.Discover`), so the race test swaps the second call directly. (2) A pure `pruneSet(rows, firstSeen, nowSeen) []string` with a table test. → **5.2** (W10 owns reindex for edges), or earlier if 1.5's in-place sources change discovery again.
- **Cost/benefit:** small. It makes the race test say what it means, and prune rules (sources removed, globs changed in 1.5) become table rows.
- **Resolution:** both remedies landed in PLAN 5.2 (W10): `Options.Discover` (nil = `entry.Discover`) and a pure `pruneSet(stored, first, now)`. `TestAllDiscoversTwiceBeforePruning` plays the concurrent add from the first Discover call (no embedder hook; `concurrentAddEmbedder` is gone), `TestPruneSet` is the prune decision table and `TestAllSecondDiscovery` covers when the second Discover runs and its failure.
- **Last reviewed:** 2026-10-30 · W10 wave-end pass
