# Float sums: sort before summing for order independence
> Floating-point addition isn't associative, so code that must give bit-identical results for any input order sorts its terms first.

**Fact.** `internal/prior.Build` sorts kept events with a total-order comparator before summing weights. Without that, shuffling the same events changes the last bits of the result. Kahan summation reduces the error but still isn't bit-exact across orders.

**Why it matters.** Properties like "order-independent" (prior, cross-store merge) and golden files need bit-exact results.

**How to apply.** Any aggregation tested for order independence or pinned by a golden file: sort the inputs with a total order, then sum.

**Seen in:** internal/prior/prior.go · 2026-10-30
