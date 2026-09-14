#!/usr/bin/env bash
# Reproduce every experiment in protobuf/README.md and docs/ from scratch, so the
# claims there can be re-checked against whatever protoc is installed today.
# Writes only to a temp dir, which it removes on exit.
set -euo pipefail

command -v protoc >/dev/null || { echo "protoc not on PATH" >&2; exit 1; }
protoc --version

work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
cd "$work"; mkdir -p a renamed str nested opt noopt v1 v2 v3 res out

hr() { printf '\n--- %s ---\n' "$1"; }
# Run a command expected to fail; print its output and confirm it really failed.
expect_fail() {
  local label=$1; shift
  if out=$("$@" 2>&1); then
    printf 'UNEXPECTED SUCCESS (%s):\n%s\n' "$label" "$out" >&2; return 1
  fi
  printf '%s\n(exit %d, as expected)\n' "$out" 1
}

cat > a/m.proto <<'P'
syntax = "proto3";
package v;
message M { int32 count = 1; string label = 2; repeated int32 rep = 3; }
P
sed 's/count/TOTALLY_DIFFERENT/; s/label/other_name/' a/m.proto > renamed/m.proto
sed 's/int32 count/string count/' a/m.proto > str/m.proto
cat > nested/m.proto <<'P'
syntax = "proto3";
package v;
message Inner { int32 x = 1; }
message M { int32 count = 1; Inner label = 2; }
P

hr "encode: count:150 label:\"hi\" rep:0"
echo 'count: 150 label: "hi" rep: 0' | protoc -I a --encode=v.M m.proto > msg.bin
od -An -tx1 msg.bin

hr "same numbers, different names -> decodes fine"
protoc -I renamed --decode=v.M m.proto < msg.bin

hr "no schema at all (--decode_raw); note field 2 misread as a message"
protoc --decode_raw < msg.bin

hr "int32 -> string: wire types disagree, field demoted to unknown (exit 0)"
protoc -I str --decode=v.M m.proto < msg.bin

hr "string -> message: wire types agree, SILENT MISREAD (exit 0)"
protoc -I nested --decode=v.M m.proto < msg.bin

cat > opt/p.proto <<'P'
syntax = "proto3";
package v;
message Inner { int32 x = 1; }
message P { int32 plain = 1; optional int32 opt = 2; Inner msg = 3; }
P
sed 's/optional int32 opt/int32 opt/' opt/p.proto > noopt/p.proto

hr "presence: bytes emitted for each zero"
echo ""             | protoc -I opt --encode=v.P p.proto > absent.bin
echo "plain: 0"     | protoc -I opt --encode=v.P p.proto > plain0.bin
echo "opt: 0"       | protoc -I opt --encode=v.P p.proto > opt0.bin
echo "msg { }"      | protoc -I opt --encode=v.P p.proto > msgempty.bin
echo "msg { x: 0 }" | protoc -I opt --encode=v.P p.proto > msgzero.bin
wc -c absent.bin plain0.bin opt0.bin msgempty.bin msgzero.bin
cmp absent.bin plain0.bin && echo 'absent == plain:0'
cmp msgempty.bin msgzero.bin && echo 'msg{} == msg{x:0}'

hr "a reader without 'optional' destroys the presence bit"
protoc -I noopt --decode=v.P p.proto < opt0.bin \
  | protoc -I noopt --encode=v.P p.proto > roundtrip.bin
wc -c opt0.bin roundtrip.bin

cat > v1/u.proto <<'P'
syntax = "proto3";
package v;
message U { string name = 1; int32 age = 2; }
P
cat > v2/u.proto <<'P'
syntax = "proto3";
package v;
message U { string name = 1; int32 account_tier = 2; }
P
cat > v3/u.proto <<'P'
syntax = "proto3";
package v;
message U { string name = 1; reserved 2; int32 account_tier = 3; }
P
cat > res/r.proto <<'P'
syntax = "proto3";
package v;
message R {
  reserved 2;
  int32 id = 1;
  string replacement = 2;
}
P

hr "reusing a number without 'reserved': an age becomes a tier"
echo 'name: "ann" age: 34' | protoc -I v1 --encode=v.U u.proto > old.bin
od -An -tx1 old.bin
protoc -I v2 --decode=v.U u.proto < old.bin

hr "with 'reserved', the stale value is inert in unknown fields"
protoc -I v3 --decode=v.U u.proto < old.bin

hr "reusing a reserved number is a compile error on an ordinary invocation"
expect_fail "reserved number" protoc -I res --cpp_out=out r.proto

hr "no output flag never reaches the check"
expect_fail "no output flag" protoc -I res r.proto

printf '\nAll experiments completed.\n'
