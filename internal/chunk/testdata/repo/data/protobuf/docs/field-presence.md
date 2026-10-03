---
title: Proto3 field presence — why a zero disappears
summary: In proto3 a plain scalar set to its zero value encodes to zero bytes and is indistinguishable from absent; `optional` restores presence, but only for readers whose schema also says `optional`.
tags: [protobuf, protoc, proto3, presence, compatibility]
updated: 2026-09-11
verified: 2026-09-11 — libprotoc 36.1
---

# Proto3 field presence — why a zero disappears

A proto3 scalar without `optional` has **implicit presence**: setting it to its zero value is
encoded as *nothing at all*. "Set to 0" and "never set" are the same bytes, so the distinction
cannot survive serialization.

```proto
syntax = "proto3";
package v;
message Inner { int32 x = 1; }
message P { int32 plain = 1; optional int32 opt = 2; Inner msg = 3; }
```

```bash
echo ""            | protoc -I . --encode=v.P p.proto > absent.bin
echo "plain: 0"    | protoc -I . --encode=v.P p.proto > plain0.bin
echo "opt: 0"      | protoc -I . --encode=v.P p.proto > opt0.bin
echo "msg { }"     | protoc -I . --encode=v.P p.proto > msgempty.bin
echo "msg { x: 0 }"| protoc -I . --encode=v.P p.proto > msgzero.bin
wc -c absent.bin plain0.bin opt0.bin msgempty.bin msgzero.bin
cmp absent.bin plain0.bin && echo identical
```

```
0 absent.bin
0 plain0.bin
2 opt0.bin
2 msgempty.bin
2 msgzero.bin
identical
```

So:

- `plain: 0` → **0 bytes**, byte-identical to sending nothing.
- `opt: 0` → **2 bytes** (`10 00`): the tag is emitted precisely so the zero is observable.
- `msg { }` → **2 bytes** (`1a 00`), and identical to `msg { x: 0 }`. Message-typed fields always
  have presence; the scalars *inside* them do not, recursively.

## The consequence people actually hit

A plain scalar cannot express "the client explicitly sent 0". A partial-update API built on
implicit-presence scalars **cannot distinguish "leave this alone" from "set it to zero"** — the
classic symptom is a PATCH that silently refuses to zero out a counter or clear a price.

Use `optional` (or a wrapper message, or a separate field mask) whenever zero is a meaningful
value a client can choose.

## The trap: presence lives in the reader's schema too

`optional` only protects you if *both* sides declare it. Decode `opt0.bin` with a schema that is
identical except the `optional` keyword is missing, and re-encode:

```bash
protoc -I noopt --decode=v.P p.proto < opt0.bin \
  | protoc -I noopt --encode=v.P p.proto > roundtrip.bin
wc -c opt0.bin roundtrip.bin
```

```
2 opt0.bin
0 roundtrip.bin
```

**Two bytes in, zero bytes out.** A middle service running the older schema strips the presence
bit while cheerfully reporting success — and because the value was zero, nothing in its output
looks wrong. Adding or removing `optional` is therefore a semantic change to every hop in the
chain, not a local edit.

For non-zero values, `optional` and plain produce identical bytes, so the keyword costs nothing
on the wire except in exactly the case it exists for.

## See also

[Protobuf wire format](../README.md) — why none of this is detectable at decode time.
