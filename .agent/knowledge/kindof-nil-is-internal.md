# errs.KindOf(nil) is Internal

> A test that checks `errs.KindOf(err) != errs.Internal` passes when no error came back; guard nil or use `wantKind`.

**Fact.** `errs.KindOf` returns `Internal` for any error that is not an `*errs.Error`, and nil is
one of them. So `if errs.KindOf(err) != errs.Internal { t.Fatal(...) }` accepts `err == nil`.
Other kinds are safe (nil never reads as Usage, NotFound, ...).

**Why it matters.** The W14 pass found 9 such checks across cli, search, stats, migrate,
workspace, sqlite and storetest. `TestRememberedGoldBadParams` was vacuous: with the error
check deleted from `rememberedGold` it still passed.

**How to apply.** Write `err == nil || errs.KindOf(err) != errs.Internal`, or pair it with
`errors.Is(err, <cause>)`, or use the cli helper `wantKind(t, err, errs.Internal)`, which fails
on nil. H61 proposes a check that flags the bare form.

**Seen in:** internal/errs/errs.go:KindOf; internal/search/memory_test.go · 2026-10-30
