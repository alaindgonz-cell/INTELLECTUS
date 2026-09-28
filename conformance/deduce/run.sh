#!/usr/bin/env bash
# Conformance runner for the deduction problem format (docs/PROTOCOL.md §7).
#
# Usage: conformance/deduce/run.sh "<deductor command>"
#   The command may include arguments; the problem file path is appended, e.g.
#     run.sh deductor/build/deductor
#     run.sh "target/release/intellectus-core deduce --impl rust"
#
# For every NN-name.in with a matching NN-name.out: the command must exit 0 and
# its stdout must equal the .out file byte for byte.
# For every NN-bad-*.in (no .out): the command must exit 2 and its stdout must
# not contain an INTELLECTUS-DEDUCED header.
set -u

if [ $# -ne 1 ] || [ -z "$1" ]; then
    echo "usage: $0 \"<deductor command>\"" >&2
    exit 64
fi
read -r -a CMD <<< "$1"

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
for input in "$DIR"/*.in; do
    name="$(basename "$input" .in)"
    expected="$DIR/$name.out"
    "${CMD[@]}" "$input" > "$TMP/stdout" 2> "$TMP/stderr"
    code=$?
    if [ -f "$expected" ]; then
        if [ "$code" -ne 0 ]; then
            echo "FAIL $name: exit code $code (expected 0)"
            sed 's/^/    stderr: /' "$TMP/stderr"
            fail=$((fail + 1))
        elif ! cmp -s "$expected" "$TMP/stdout"; then
            echo "FAIL $name: output differs"
            diff -u "$expected" "$TMP/stdout" | sed 's/^/    /'
            fail=$((fail + 1))
        else
            echo "ok   $name"
            pass=$((pass + 1))
        fi
    elif [[ "$name" == *-bad-* ]]; then
        if [ "$code" -ne 2 ]; then
            echo "FAIL $name: exit code $code (expected 2)"
            fail=$((fail + 1))
        elif grep -q '^INTELLECTUS-DEDUCED' "$TMP/stdout"; then
            echo "FAIL $name: printed an INTELLECTUS-DEDUCED header"
            fail=$((fail + 1))
        else
            echo "ok   $name (exit 2: $(head -n 1 "$TMP/stderr"))"
            pass=$((pass + 1))
        fi
    else
        echo "FAIL $name: no $name.out and name lacks -bad-"
        fail=$((fail + 1))
    fi
done

echo "passed $pass, failed $fail"
[ "$fail" -eq 0 ]
