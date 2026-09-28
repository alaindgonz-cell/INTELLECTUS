# The Absolute Mathematical Deductor (Mojo)

A standalone CLI that computes the bounded Horn-fragment fixpoint and detects
contradictions for the deduction problem format in
[`docs/PROTOCOL.md` §7](../docs/PROTOCOL.md). The core runs it through
`--deductor mojo:<bin>` or `cross:<bin>`. In `cross` mode the core compares
its output byte for byte with the Rust reference.

## Build and run

```sh
MOJO=/path/to/mojo deductor/build.sh    # or put `mojo` on PATH
deductor/build/deductor problem.txt     # result on stdout
conformance/deduce/run.sh deductor/build/deductor
```

`build.sh` runs `mojo build deductor/deductor.mojo -o deductor/build/deductor`.
The `build/` directory is git-ignored.

Exit codes:

| code | meaning | stdout |
|---|---|---|
| 0 | success | the §7 result |
| 1 | usage error (argument count) or the problem file cannot be read | empty |
| 2 | malformed input, including limit violations | empty (never an `INTELLECTUS-DEDUCED` header) |

The error message goes to stderr, for example
`deductor: malformed input: line 5: literal |3| exceeds atoms 2`.

## Accepted grammar (strict)

§7 says "line-oriented ASCII, tokens separated by single spaces". The
deductor reads that strictly:

* Every line ends with `\n`. The final `end` line may end with one `\n` or
  at end of file. Nothing may follow: no blank line, no second `\n`, no `\r`.
* Counts (`atoms`, `facts`, `rules`, a rule's `n`) are canonical unsigned
  decimals: `0` or `[1-9][0-9]*`. Leading zeros, a `+` sign or `-` are
  rejected.
* A literal is `-?(0|[1-9][0-9]*)`. `+5` is rejected. The literal must not
  be 0 (this also rejects `-0`), and its absolute value must be at most N.
* Tokens are separated by exactly one space. There are no leading or trailing
  spaces and no empty lines.
* A rule line is `<head> <n>` followed by exactly `n` body literals. A body
  may list the same literal more than once, and the same fact may appear more
  than once (both are sets).
* Limits: `N ≤ 1_000_000`, `R ≤ 1_000_000`, total body literals
  `≤ 10_000_000`. Anything over a limit is malformed. `K` (the number of
  facts) has no separate limit.

The Rust reference must accept exactly the same language. Otherwise `cross`
mode fails closed with `DEDUCTOR_DISAGREEMENT` on inputs that are only
borderline malformed. Vectors `60`–`68` in `conformance/deduce/` pin down
these choices.

## Semantics (Jacobi rounds)

* `D0` is the set of facts. Each fact has justification `-1` and depth `0`.
* Round `k+1` adds the head of every rule whose body is contained in `D_k`
  and whose head is not in `D_k`. The head's justification is the smallest
  index among the rules that fire for that head in this round, and its depth
  is `k+1`.
* The process stops when a round adds nothing. A conflict is an atom `i`
  with both `+i` and `-i` in the fixpoint.

### Implementation

The implementation is linear in the input size. It performs no per-round
scan over all rules.

* A literal is stored at index `+i → 2(i-1)` and `-i → 2(i-1)+1`. Ascending
  index order is the required output order (`|lit|` ascending, positive
  first), so the output needs no sort.
* `depth[idx]` (`Int32`, `-1` = not derived) serves as both the membership
  set and the depth. `just[idx]` holds the justification. With at most
  2·10⁶ literals these arrays take 16 MB. A separate bitset would add a
  second structure to keep in sync without saving any work.
* Each rule has a counter of body literal *occurrences* not yet derived.
  There is also a CSR occurrence list mapping each literal to the rules whose
  body contains it. A duplicated body literal appears twice in the list and
  is counted twice, so it is handled correctly.
* Frontier processing follows the rounds. The literals derived in round `k`
  decrement the counters of their rules. When a counter reaches 0 during
  round `k`, the rule's body is in `D_k` for the first time. That is exactly
  when the rule fires in round `k+1` (if its body were already in `D_{k-1}`,
  its head would already be in `D_k`). For each such rule:
  * If the head is not derived, it gets depth `k+1` and justification `r`,
    and joins the next frontier.
  * If the head was set to depth `k+1` earlier in this same round, its
    justification becomes `min(just, r)`.
  * If the head has a smaller depth, the rule is skipped.

  Rules with an empty body are complete in round 0, so they fire in round 1
  unless their head is a fact.

## Status (honest)

Everything below was actually compiled and run with **Mojo 1.1.0
(8189361e)** on Linux x86_64 (4 vCPUs):

* `deductor/build.sh` builds with no warnings. The only message is Mojo's
  harmless "Failed to initialize Crashpad" notice on stderr.
* `conformance/deduce/run.sh deductor/build/deductor` passes 32 of 32 vectors:
  13 valid vectors whose outputs were derived by hand from §7, and 19
  malformed vectors (all exit 2 with no header).
* Randomized cross-checks against an independent naive Python transcription
  of §7 (not committed):
  * 3,000 random small problems: identical output.
  * 4,000 byte-mutated problems, checked against a strict Python parser of
    the grammar above: acceptance and rejection agreed on every case, and
    every accepted case produced identical output.
  * The large random problems below also matched the Python reference.
* Performance with `mojo build` defaults, wall clock including process start
  and file I/O:

  | problem | atoms | rules | body lits | derived | max depth | time |
  |---|---|---|---|---|---|---|
  | random, dense derivation | 200,000 | 500,000 | 899,650 | 173,098 | 121 | ~0.17 s |
  | random, sparse derivation | 200,000 | 500,000 | 1,099,912 | 4,233 | 8 | ~0.14 s |
  | single chain | 200,001 | 200,000 | 200,000 | 200,001 | 200,000 | ~0.06 s |
  | at all limits | 1,000,000 | 1,000,000 | 10,000,000 | 15 | 2 | ~1.1 s |

Mojo version caveat: Mojo's syntax and standard library change between
releases. The code uses only `def` functions, `struct`, `List`, `String`,
`comptime` constants, `std.sys.argv`/`exit`/`stderr`, `open(...).read_bytes()`
and `std.io.FileDescriptor.write_bytes`. It uses no raw pointers, because
`alloc` and pointer indexing are already deprecated in 1.1.0. Other Mojo
versions may need small edits. Older releases, for example, use `alias`
instead of `comptime` and import from `sys` instead of `std.sys`.
