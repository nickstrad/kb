---
title: Protobuf wire format — field numbers are the contract
summary: On the wire a protobuf message is only field numbers and wire types; names live in the schema, so renaming is free and re-typing fails silently.
tags: [protobuf, protoc, serialization, wire-format, compatibility]
updated: 2026-09-11
verified: 2026-09-11 — every command below run against libprotoc 36.1, proto3 only
---

# Protobuf wire format — field numbers are the contract

An encoded protobuf message contains **no field names**. Each field is a varint *tag* packing a
field number and a wire type, followed by the payload. The `.proto` file is the only thing that
maps number 2 back to `label`.

Everything downstream follows from that one fact: renaming a field is free, changing its number
is catastrophic, and the decoder cannot tell you when it is wrong.

## The bytes

```bash
cat > m.proto <<'P'
syntax = "proto3";
package v;
message M { int32 count = 1; string label = 2; repeated int32 rep = 3; }
P
echo 'count: 150 label: "hi" rep: 0' | protoc -I . --encode=v.M m.proto | od -An -tx1
```

```
 08 96 01 12 02 68 69 1a 01 00
```

Read it as tag/payload pairs, where **tag = (field_number << 3) | wire_type**:

| Bytes | Tag math | Meaning |
| --- | --- | --- |
| `08` | `8 >> 3 = 1`, `8 & 7 = 0` | field 1, wire type 0 (varint) |
| `96 01` | `0x16 + (1 << 7)` | `= 150` |
| `12` | `18 >> 3 = 2`, `18 & 7 = 2` | field 2, wire type 2 (length-delimited) |
| `02 68 69` | length 2 | `"hi"` |
| `1a` | `26 >> 3 = 3`, `26 & 7 = 2` | field 3, **wire type 2** |
| `01 00` | length 1 | the single value `0` |

Note field 3: a `repeated int32` is **packed by default in proto3**, so it arrives as one
length-delimited blob, not as wire type 0. That surprises people reading hex by hand.

The tag is itself a varint, which is why field numbers 1–15 cost one byte and 16+ cost two —
verified: field 15 → `78`, field 16 → `80 01`, field 2000 → `80 7d`. **Spend 1–15 on your
hot fields.**

## Renaming is a no-op

A schema with entirely different names but the same numbers and types decodes the same bytes:

```bash
# message M { int32 TOTALLY_DIFFERENT = 1; string other_name = 2; repeated int32 rep = 3; }
protoc -I renamed --decode=v.M m.proto < msg.bin
```

```
TOTALLY_DIFFERENT: 150
other_name: "hi"
rep: 0
```

Exit 0. So a field rename is a source-level change only — but see
[grpc-reflection-and-grpcurl.md](../grpc-reflection-and-grpcurl.md), because **JSON-based tooling
keys on names**, and that is where a rename does break things.

## Changing a type does not raise an error

This is the part worth internalizing. `protoc --decode` returned **exit 0 with empty stderr in
every incompatible case tested.** There are two distinct failure shapes:

**1. Wire types disagree → the field is silently moved to unknown fields.** Decoding the bytes
above with `count` re-typed from `int32` (wire type 0) to `string` (wire type 2):

```
label: "hi"
rep: 0
1: 150          <- the old field, demoted to an unknown field, printed by number
```

Your data is not lost — it is printed last, by number, as an unknown field — but `count`
reads as unset. Nothing warns you.

**2. Wire types agree → a genuine silent misread.** `string label = 2` re-typed to a nested
message is still wire type 2, so the decoder happily parses the string's bytes as a submessage:

```
label {
  13: 105
}
```

`"hi"` is `68 69`; `0x68 = 104`, and `104 >> 3 = 13`, `104 & 7 = 0` — so the decoder sees "field
13, varint" and eats `0x69 = 105` as its value. Exit 0. **A protobuf decoder cannot tell you your
schema is wrong; it can only tell you your framing is wrong.**

The one safe widening confirmed here: `int32 → int64` is byte-identical, negatives included.
(`int32 -1` sign-extends to a 10-byte varint payload — the same bytes an `int64 -1` produces,
which is exactly why the widening is free and why storing negative numbers in `int32` costs
maximum size.)

## `--decode_raw` guesses, and gets it wrong

`protoc --decode_raw` reads bytes with no schema at all — the right first move on an opaque blob:

```bash
protoc --decode_raw < msg.bin
```

```
1: 150
2 {
  13: 105
}
3: "\000"
```

It recovers field numbers and wire types, never names. And for wire type 2 it **heuristically
guesses** whether the payload is a nested message or a string — above, it misidentified the
string `"hi"` as a message. It errs in both directions: a real empty nested message prints as
`2: ""`. Treat wire-type-2 output as a suggestion, not a reading.

## Files in this directory

| Path | What it is |
| --- | --- |
| `scripts/wire-demo.sh` | Reproduces every experiment on this page from scratch in a temp dir; run it to re-verify against a newer protoc. |
| `docs/field-presence.md` | Why a field set to zero can vanish, and what `optional` actually buys. |
| `docs/reserved-fields.md` | What `reserved` prevents, and the silent corruption it exists to stop. |

## Scope

proto3 only, via the `protoc` CLI. **Unverified:** no language runtime was tested, so
unknown-field *retention* across a decode/re-encode in Go/Java/Python is not established here —
only what the CLI prints. proto2 and editions were not tested.
