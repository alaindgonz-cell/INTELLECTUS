# engine/ — The Parallel Attributes (Go concurrency engine)

The engine runs model modes, the protected test runner, tool-gateway
workers, reconciliation, the task scheduler and the optional Jev advisor.
It **never writes authoritative state**: it holds no database handle and
computes no digest the core relies on. Every change is a command to
`intellectus-core serve` over JSON Lines (docs/PROTOCOL.md). Go standard
library only; module `github.com/alaindgonz-cell/intellectus/engine`.

## Layout

| package | role |
|---|---|
| `coreclient` | spawns `intellectus-core serve`, JSON-Lines client (one write mutex, responses demultiplexed by id, fail-closed on EOF/desync), typed wrappers for every §4 command, `Operator` (Ed25519 signing of §5 envelopes) |
| `modes` | `Provider` interface, `RecordedProvider` (scripted, no network), Planner/Coder/Tester adapters that pass raw model text to `submit` unmodified, bounded parallel pool, call budget |
| `runner` | `Worker` interface, `FakeWorker` (scripted by SHA-256 of a target file; unknown content → `UNKNOWN`, incomplete), `SandboxWorker` (always refuses: M4), `ProtectedRunner` (`check_plan` → worker → `report_check`) |
| `gateway` | `Adapter` interface, `FakeTargetAdapter` (in-memory target keyed by idempotency key; crash/panic/hang/error injection), `Dispatcher` (`dispatch_begin` → execute → `record_outcome`, or `record_outcome_unknown` on any error/panic/cancel), `Reconciler` (read-only; never re-executes) |
| `advisor` | `DecisionAdvisor`, `SimulatedAdvisor`, `ShadowRunner` (route first; advisor only when no deterministic choice; always applies the core's `applied_choice`; fails closed if the core lets an advisor steer in SHADOW) |
| `scheduler` | task loop: plan → candidate(s) → checks → route → repair … → contexts → action → admit → (operator approval) → admit → optional dispatch; stops on `REPAIR_BUDGET_EXHAUSTED`, never requests COMPLETED, never restarts a cancelled task |
| `internal/fakecore` | in-process fake core for unit tests (io.Pipe, scripted handlers, optional reversed response order) |
| `cmd/intellectus-demo` | the page-size workflow end to end against the real core |

## Build and test

```sh
cd engine
go vet ./...
go test -race ./...
# optional end-to-end test against a built core:
INTELLECTUS_CORE=../core/target/debug/intellectus-core go test -race ./cmd/intellectus-demo/
```

## Run the demo

```sh
(cd core && cargo build)
cd engine
go run ./cmd/intellectus-demo --core ../core/target/debug/intellectus-core \
    [--deductor rust|mojo:<bin>|cross:<bin>] [--workdir intellectus-demo-work]
```

The workdir receives `operator.key` (hex seed, 0600), `policy.json`,
`root/`, `intellectus.db`, `wire.jsonl` (every request/response line),
`core.stderr.log` and `task_report.json`. Each run starts from a fresh
genesis in that workdir.

## Honest status

* **EXECUTED against the real core** (debug build of `core/`, deductors
  `rust` and `cross:deductor/build/deductor`): the full demo — signed
  operator setup, plan, A0 → core FAIL, route + SHADOW assessment (core
  applied REPAIR), A1 → core PASS, formal context PASS, MISSING_APPROVAL →
  approve → AUTHORIZED, A2 → ARTIFACT_BINDING_MISMATCH, reused key →
  IDEMPOTENCY_CONFLICT, external export with crash-after-effect →
  OUTCOME_UNKNOWN → reconciliation (effect observed, 1 execute), promotion
  COMPLETED, `replay_verify` and offline `replay` both match.
* **SIMULATED**: all model text (RecordedProvider), all test outcomes
  (FakeWorker — generated code is never executed), the advisor
  (SimulatedAdvisor, SHADOW) and the external target (FakeTargetAdapter).
* **DESIGNED, not enabled**: real code execution (`SandboxWorker` fails
  closed until a verified host sandbox exists, milestone M4) and any live
  model provider (no network code exists).
* Unit tests use a fake in-process core; they prove engine behaviour
  (serialized writes, demux, signing, fail-closed paths), not the core's.

## Protocol interpretations (aligned with core/ as of this writing)

* Operator body: op-specific fields sit flat beside `project_id`, `nonce`,
  `issued_at` (u64) and `op`; marshalled once, signed as
  `"intellectus/v1/operator\n" + body`, sent as the `body` string.
* Ids (`session_id`, `proposal_id`, …) are echoed back as the exact JSON
  token received (`coreclient.ID`); the core currently uses strings.
* `record_assessment` also carries `failure_codes` (same as given to
  `route`); `task_transition` carries an optional `reason`.
* Route `failure_codes` use the core's reason vocabulary: `CHECK_FAILED`
  (core FAIL), `CHECK_NOT_PASSED` (UNKNOWN/ERROR/TIMEOUT), or the reason
  codes of a refused report (e.g. `UNTRUSTED_ISSUER`) or rejected submit.
* SHADOW assessments record the advisor's own `mode` (`SIMULATED`) with
  `fallback_reason: "SHADOW_MODE"`; timeouts/errors/malformed/out-of-set
  advice are recorded with their own fallback reason and `choice: null`
  (an out-of-set but valid route option is forwarded so the core refuses it).
* Promotion completes the task, after which the core accepts no new
  proposals for it; the demo therefore runs the external `export_view`
  (on the pre-promotion root) before dispatching the promotion.
