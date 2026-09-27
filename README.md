# THE INTELLECTUS

A model-independent agent runtime in which role-specific evaluators submit
structured proposals against one versioned project state, and an
**Axiomatic Knowledge Graph** records claims, assumptions, evidence,
inference rules, alternative derivations, verification results and
dependencies.

> The LLM proposes. Jev optionally recommends routing. The AKG tracks
> justification and applicability. Trusted code checks and authorizes. The
> tool gateway executes. Recorded observations support subsequent evaluation.

| Component | Language | Directory |
|---|---|---|
| **The Substance** — immutable core: single-writer event log, pure reducer, snapshots and replay, AKG, policy, verifier, admission, outbox | Rust | [`core/`](core/) |
| **The Parallel Attributes** — concurrency engine: parallel Planner/Coder/Tester modes, protected runner, tool gateway, reconciler, scheduler, Jev advisor | Go | [`engine/`](engine/) |
| **The Absolute Mathematical Deductor** — bounded ground-Horn fixpoint and contradiction detection | Mojo | [`deductor/`](deductor/) |

* [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) explains how the contract maps
  onto these three components and lists every deviation from the contract.
* [docs/PROTOCOL.md](docs/PROTOCOL.md) is the binding wire contract between
  the components.

## Quick start

```sh
./scripts/check.sh      # everything: Mojo build + conformance, Rust fmt/clippy/tests,
                        # Go vet + race tests, end-to-end demo
# or piecewise:
(cd core && cargo test)
(cd engine && go test -race ./...)
MOJO=/path/to/mojo deductor/build.sh && conformance/deduce/run.sh deductor/build/deductor
(cd core && cargo build) && (cd engine && go run ./cmd/intellectus-demo \
    --core ../core/target/debug/intellectus-core --deductor cross:../deductor/build/deductor)
```

Requirements: Rust (tested with 1.94), Go (tested with 1.24), and optionally
Mojo (tested with 1.1.0, installed via `pip install mojo`). Without Mojo the
core uses its Rust reference deductor, which has identical semantics.

## Status (v0.1)

These are separate statuses, as the contract requires. **EXECUTED** means the
code ran in this repository's test suite or demo. **VERIFIED** means an
automated test asserted the required result. Nothing here has been
independently audited.

| Deliverable | Designed | Implemented | Executed | Verified |
|---|---|---|---|---|
| Event log, pure reducer, snapshots, deterministic replay (T01, T26) | ✔ | ✔ | ✔ | ✔ automated tests |
| AKG: revisions, independent status dimensions, invalidation fences, alternative derivations (T05, T06, T22–T24) | ✔ | ✔ | ✔ | ✔ |
| Mojo deductor, Rust reference and cross mode that fails closed | ✔ | ✔ | ✔ | ✔ 32 hand-derived vectors; 300 random Rust↔Mojo differential cases |
| Receipts from trusted issuers only; artifact-bound approvals (T07, T10–T12) | ✔ | ✔ | ✔ | ✔ |
| Admission, outbox, launch boundary, idempotency (T02–T04, T09, T14, T15, T21, T25) | ✔ | ✔ | ✔ | ✔ |
| Crash recovery to OUTCOME_UNKNOWN and reconciliation without blind retry (T13) | ✔ | ✔ | ✔ | ✔ core and Go tests |
| Deterministic routing, then Jev in SHADOW with visible fallback (T16, T17) | ✔ | ✔ simulated advisor only | ✔ | ✔ |
| Permission-filtered retrieval with sensitivity inheritance (T20) | ✔ | ✔ | ✔ | ✔ |
| Budget, cancellation and shutdown (T18); injected instructions stay data (T08) | ✔ | ✔ | ✔ | ✔ |
| Parallel modes, serialized core client, gateway, reconciler (Go) | ✔ | ✔ | ✔ | ✔ `go test -race` |
| End-to-end page-size workflow (contract §4) | ✔ | ✔ | ✔ | ✔ replay matches |
| **Isolated execution of generated code (M4, T19)** | ✔ | ✘ `SandboxWorker` always refuses | ✘ | ✘ |
| **Live model providers or a live Jev client** | ✔ interface | ✘ | ✘ | ✘ |
| Comparison study (B0/B1/B2) and metrics (contract §7) | ✔ in the contract | ✘ | ✘ | ✘ |

**Remaining limitations.**
* All test outcomes come from a fake worker; candidate code is never run.
  Every receipt and report marks this as SIMULATED.
* All model text is recorded, not live.
* The consistency guarantee covers only the finite ground Horn fragment.
* Append-only storage is enforced within the application's trust model. It
  is not tamper-proof against someone who controls the host.
* Each event stores a full snapshot, which is simple but grows storage
  quadratically.
* See [docs/ARCHITECTURE.md § Incompatibilities](docs/ARCHITECTURE.md) for
  the full list of deviations.
