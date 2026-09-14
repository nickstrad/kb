---
title: Protobuf `reserved` — a compile-time tripwire, not a runtime guard
summary: `reserved` makes reusing a retired field number a protoc error; without it, old bytes silently reappear as a live field of the wrong meaning.
tags: [protobuf, protoc, proto3, schema-evolution, compatibility]
updated: 2026-09-11
verified: 2026-09-11 — libprotoc 36.1
---

# Protobuf `reserved` — a compile-time tripwire, not a runtime guard

Deleting a field frees its number, and the next person to add a field will be offered that number
by their editor, their reviewer, or plain bad luck. Nothing on the wire records that the number
used to mean something else.

## What goes wrong without it

`v1` writes `age = 2`. Someone deletes `age` and later reuses number 2 for `account_tier`:

```bash
echo 'name: "ann" age: 34' | protoc -I v1 --encode=v.U u.proto > old.bin
od -An -tx1 old.bin                 #  0a 03 61 6e 6e 10 22
protoc -I v2 --decode=v.U u.proto < old.bin
```

```
name: "ann"
account_tier: 34
```

Exit 0. A stored age became a tier. No error, no warning, no way for the reader to know — this is
the corruption `reserved` exists to prevent.

## What `reserved` does

```proto
message U {
  string name = 1;
  reserved 2;              // was: age
  int32 account_tier = 3;
}
```

```
name: "ann"
2: 34
```

Still exit 0 — but the stale value lands in **unknown fields**, where it is inert, instead of
impersonating a live field.

## The error, and how to trigger it

Reusing a reserved number or name is a hard `protoc` failure, **exit 1**, on any ordinary
invocation — `--cpp_out`, `--go_out`, `-o`, even `--encode`. You do not need an exotic flag such
as `--descriptor_set_out` to surface it:

```
r.proto:4:12: Field "replacement" uses reserved number 2.
r.proto:4:12: Suggested field numbers for v.R: 3
```

```
r.proto:6:10: Field name "label" is reserved.
```

Two gotchas in that output:

- **The line:column for a reserved *number* points at the `reserved` declaration, not at the
  offending field** (`4:12` is the `reserved 2;` line). The reserved *name* error points at the
  field instead. Don't trust the caret to find your mistake.
- `protoc r.proto` with **no** output flag never gets far enough to check — it exits 1 with
  `Missing output directives.` A syntax-only check still needs an output flag.

Reserve the name as well as the number when the name carried meaning; reserved names must be
quoted string literals in proto3. Reservations are scoped per message, and accept ranges
(`reserved 2, 15, 9 to 11;`).

## The honest limitation

`reserved` buys nothing at runtime. Both the reuse case and the reserved case decode with exit 0;
the only difference is whether the stale bytes hit a live field name. It is a note to your future
colleague that the compiler happens to enforce — and it evaporates the moment someone deletes the
`reserved` line to make their build pass.

## See also

[Protobuf wire format](../README.md) — why a decoder can never detect this on its own.
