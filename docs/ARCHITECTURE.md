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

## The harness (v0.2)

`engine/harness` turns the runtime into an agent harness operated from a
web UI (`engine/web`, `intellectus serve`):

```
operator chat ──► intake (Claude) ──► task proposal card
                                         │ operator approves (Ed25519-signed:
                                         ▼ requirement, protected manifest + entrypoint,
                                           spec-consistency assumptions, task)
  Planner (Claude) ─► Coder (Claude) ─► submit ─► check_plan ─► bubblewrap sandbox
        ▲                 ▲                                          │ observations
        │ REPLAN          │ REPAIR / GATHER_CONTEXT (Tester)         ▼
        └──── route: deterministic rules ─► Jev (SHADOW/LIVE) ◄── core verdict FAIL
                                                                     │ PASS
                                  evaluate_context (Rust↔Mojo) ◄─────┘
                                         │ PASS
                  scheduler submits promote_local ─► admit ─► MISSING_APPROVAL
                                         │ operator approves the exact action digest
                                         ▼
                           admit ─► dispatch_begin ─► approved root advances ─► report
```

* Model output enters the core only through `submit` / `record_input`, with
  the full provider response kept as a protected blob. The runtime, never
  the model, binds the task id, the base root and every principal.
* Operator commands are signed only on an explicit UI action. The UI is
  token-authenticated (an HttpOnly SameSite=Strict cookie), mutations require
  a custom CSRF header and same-origin, and every page is served under a
  strict CSP. Model text is rendered with `textContent` only.
* The sandbox (`engine/sandbox`) runs each case in a fresh interpreter
  under bubblewrap: every namespace unshared, nested user namespaces
  disabled, an unprivileged host uid, a read-only root, a bounded tmpfs, a
  scrubbed environment, no inherited descriptors, and CPU / memory / process /
  file-size / wall-clock limits. The worker is used only if its 13-probe
  isolation self-test passes at startup, and only if the operator's policy
  trusts its implementation digest.
* Jev is consulted only when the core's `route` leaves a real choice. Its
  assessment (model, probabilities, confidence, latency, usage, fallback
  reason) is recorded in the core. In SHADOW mode it is never applied.

## Incompatibilities and deviations from the contract (explicit)

1. **Authorization.** The contract's CURRENT AUTHORIZATION section says
   "specification only". The project owner then explicitly asked for an
   implementation (v0.1) and later for a real harness connected to Claude
   and Jev (v0.2). Those requests are treated as the separate authorized
   tasks the contract anticipates. Paid Claude calls happen only when the
   operator supplies a key and chats. Nothing is deployed.
2. **Stack.** The contract proposes Python + SQLite; the owner required
   Rust / Go / Mojo. SQLite is kept. The processes are one core, one engine
   and a short-lived deductor subprocess on one host, which is not a
   distributed system. The cross-language boundary is pinned by
   `docs/PROTOCOL.md` and by end-to-end tests.
3. **Mojo toolchain maturity.** The deductor was verified with Mojo 1.1.0.
   `cross:` mode fails closed on any disagreement with the Rust reference.
4. **Isolation (M4)** is implemented with bubblewrap + prlimit, with no
   seccomp filter; the residual kernel attack surface is documented in
   `engine/sandbox/README.md`. When not running as root, the process limit
   is relative to the user's current process count, which is a weaker bound.
5. **Snapshots** are stored in full per event: simple and verifiable, but
   O(n·|state|) storage. Compaction is an M9 item.
6. **Jev.** The client follows the request shape of the MIT community
   harness `ismaelsoilet/jev-harness@37ab8c6`, because TypeSafe's own
   documentation was unreachable from the build environment. The live
   OpenRouter call was refused by that environment's egress policy, so the
   live path is unverified; the recorded fallback path is verified. Jev
   defaults to SHADOW until thresholds are calibrated on held-out examples,
   as the contract requires.
7. **Task scope of effects.** A successful `promote_local` completes its
   task. Exports therefore run as separate, operator-opened effect-only
   tasks, which complete when the export's postconditions pass.
8. **Task shape.** Chat-created tasks are single Python functions with
   JSON-encodable inputs and outputs. The formal context of such a task
   checks only that the approved tests never both accept and reject the
   same input.
9. **Comparison study (§7).** It is not run. The harness records the
   inputs it needs (usage, latency, routing decisions, outcomes), but a
   B0/B1/B2 study needs live Jev access and a budgeted task suite.
