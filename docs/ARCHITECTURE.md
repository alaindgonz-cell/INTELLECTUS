# THE INTELLECTUS — architecture (v0.1 implementation)

This implements the *Implementation contract and build sequence — v0.1*
with the language split the project owner requested:

| Contract concept | Metaphor | Language | Where |
|---|---|---|---|
| State Service, Evidence Store, AKG Query Layer, Verifier, Policy Evaluator, outbox + launch boundary | **The Substance** (immutable core) | Rust | `core/` |
| Modes (Planner/Coder/Tester), protected runner, Tool Gateway workers, Reconciler, scheduler, Jev DecisionAdvisor | **The Parallel Attributes** (concurrency engine) | Go | `engine/` |
| Bounded formal-context checker (finite ground Horn fragment, contradiction detection) | **The Absolute Mathematical Deductor** | Mojo | `deductor/` |

The operating principle is unchanged: *the LLM proposes; Jev optionally
recommends; the AKG tracks justification; trusted code checks and
authorizes; the gateway executes; recorded observations support later
evaluation.*

## Process topology

```
 operator ──(Ed25519-signed envelopes)──┐
                                        ▼
 ┌──────────────── Go engine process ─────────────────┐     ┌──── Rust core process ────┐
 │ Planner/Coder/Tester adapters (parallel goroutines) │     │ single serialized          │
 │ Provider (recorded responses only — no paid APIs)   │JSONL│ coordinator: reducer,      │
 │ protected runner  → FakeWorker (SandboxWorker off)  │◄───►│ SQLite event log +         │
 │ tool gateway → adapters; reconciler                 │stdio│ snapshots, AKG, policy,    │
 │ scheduler + DecisionAdvisor (Jev, SHADOW default)   │     │ verifier, outbox           │
 └─────────────────────────────────────────────────────┘     └────────────┬──────────────┘
                                                                          │ problem file
                                                                          ▼
                                                        Mojo deductor (subprocess), or
                                                        Rust reference, or cross-check both
```

* **One logical authority.** The Rust core is the only writer. The Go
  engine holds no database handle, never computes a digest the core relies
  on, and can only *request* changes through typed commands.
* **Parallelism lives in Go.** Model calls, candidate preparation, test
  runs and gateway I/O run concurrently. Authoritative commits are
  serialized by the core, which processes commands strictly in order.
* **The Mojo deductor is a checker, not an authority.** It computes a
  fixpoint over a problem the core compiled; the core decides what the
  result means. In `cross:` mode the Rust reference runs too and any
  disagreement fails closed (`DEDUCTOR_DISAGREEMENT` → `ERROR` receipt).

## Why these boundaries

* The contract requires that same-process code is *not* treated as a
  security boundary. Modes run in the Go process with the gateway, which
  is acceptable only because A3 treats role adapters as trusted code; the
  *model output* they carry is untrusted and reaches the core only through
  `submit`, whose envelope parser is strict (`deny_unknown_fields`) and
  cannot carry receipts, approvals or principals.
* Authentication: the engine principal is the parent process holding the
  core's stdio pipe; role principals are bound by the core from session
  ids it issued, never from JSON. Operator authority is an Ed25519
  signature over the exact body bytes, with nonce replay protection.
  The core holds only the operator's *public* key.
* Receipts: only a session whose principal *and* implementation digest
  are registered in protected policy for that check kind can cause a
  `VerificationIssued` event, and `PASS` is issued only for a complete,
  exact report of the protected manifest.

## State model

* `S[n+1] = reduce(S[n], E[n+1])`; `state::reduce_with_digest` is pure.
  Clock readings, ids and decisions are computed by the coordinator and
  recorded *in* events. Replay (`replay_store`) folds events, compares
  every stored snapshot digest, and halts on an unsupported reducer or
  schema version.
* One command = one SQLite `BEGIN IMMEDIATE` transaction: head check,
  events, per-event snapshots, blobs, action/outbox projection rows, head
  advance. Triggers make events, snapshots and blobs append-only within
  the application trust model (not against a host administrator).
* Ids derive from the event sequence (`prop:17`, `act:42`) — no randomness.
* Dependency fingerprint (conservative, whole-project): approved root,
  policy digest, every knowledge record's exact revision + lifecycle,
  protected test-manifest and environment digests. It deliberately
  *excludes* audit-only history (proposals, receipts, evaluations,
  assessments), so recording a check never invalidates the check.

## AKG

Nodes (`req`, `assumption`, `evidence`, `claim`, `rule`, `derivation`) are
immutable revisions addressed as `kind:id@rev`; a new revision supersedes
the old. Epistemic status (OBSERVED / ASSUMED / DERIVED_UNDER_PREMISES /
UNSUPPORTED / DISPUTED), lifecycle (CURRENT / STALE / SUPERSEDED / REVOKED)
and derivation status (PENDING / VALID / INVALID / STALE / INDETERMINATE)
are independent. Models can only propose `Derived` claims; they cannot
assert observations.

Revocation computes its invalidation boundary (reverse-dependency closure)
*inside the same event*, so affected derivations are unusable immediately,
before any re-evaluation. Alternative derivations are tracked separately.

A formal context is compiled to integer literals: current observed/assumed
claims and live assumptions are facts; each structurally valid derivation
(rule body = premise literals, rule head = conclusion literal, no self
support) becomes a ground Horn rule. A derivation is VALID only if all its
premises are grounded in the least fixpoint — circular support never
grounds. Conflicting literals make the context inconsistent; affected
derivations become INDETERMINATE and the `formal_context` check fails.
Absence yields UNKNOWN (open world); no negative literal is ever derived
from absence.

## Admission and dispatch

`admit` checks, collecting every reason: task open; tool registered (fail
closed); premise and requirement usability; for `promote_local`, that the
candidate's base is the current approved root; every policy-required check
has an applicable receipt (exact candidate root, base root, trusted issuer
+ implementation, current fingerprint, policy, manifest and environment);
formal contexts evaluated at the current fingerprint and consistent;
resource reservation free; a live, unconsumed approval bound to the exact
action digest (which binds the base→candidate transition). Only then are
applicability records and the intent + outbox + reservation committed
together.

`dispatch_begin` is the launch boundary: it re-checks task state, policy
digest, payload identity, dependency fingerprint, base root, approvals and
premises, then commits `DISPATCH_STARTED` *before* returning the payload.
Local promotion is an internal effect committed atomically with its
outcome and postcondition evaluation. External effects are executed by the
Go gateway; a crash after `DISPATCH_STARTED` becomes `OUTCOME_UNKNOWN` on
restart and is reconciled by an adapter read, never blindly re-executed.

## Incompatibilities and deviations from the contract (explicit)

1. **Authorization vs. request.** The contract's CURRENT AUTHORIZATION
   section says "specification only". The project owner then explicitly
   asked for this implementation; that request is treated as the separate
   authorized task the contract anticipates. No paid model API is called,
   nothing is deployed, and generated code is never executed.
2. **Stack.** The contract proposes Python + SQLite. The owner required
   Rust / Go / Mojo. SQLite is kept (via `rusqlite`, bundled). The
   contract's "no microservices" guidance is respected: there are two
   processes on one host joined by a pipe, plus a short-lived deductor
   subprocess — not a distributed system. The cross-language boundary is
   a real cost (a wire protocol to keep in sync); it is pinned by
   `docs/PROTOCOL.md` and by end-to-end tests.
3. **Mojo toolchain maturity.** The deductor was built and tested with
   Mojo 1.1.0; Mojo syntax still changes between releases. Because the Rust
   reference implements identical semantics and `cross:` mode fails closed
   on disagreement, a Mojo break cannot silently change a decision.
4. **Isolation (M4) is not implemented.** The protected runner has only a
   `FakeWorker` (scripted outcomes; candidate code is not executed) and a
   `SandboxWorker` that always refuses. Every receipt from the fake worker
   carries a `SIMULATED` applicability limit, and task reports say so.
   Contract T19 is therefore not satisfiable yet and is reported as such.
5. **Snapshots** are stored in full per event — simple and verifiable, but
   O(n·|state|) storage. Acceptable for the MVP; compaction is an M9 item.
6. **Jev.** Only a `SimulatedAdvisor` exists. No live Jev/TypeSafe client
   was written, because the provider API was not verified against primary
   documentation in this task. Assessments record mode SIMULATED/SHADOW and
   can never be mistaken for a live result.
