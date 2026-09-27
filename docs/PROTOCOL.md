# THE INTELLECTUS — cross-language protocol v1 (schema v2)

This document is the binding contract between the three components:

| Component | Language | Directory | Role |
|---|---|---|---|
| The Substance | Rust | `core/` | Only writer of authoritative state: event log, reducer, snapshots, AKG, policy, verifier, outbox |
| The Parallel Attributes | Go | `engine/` | Concurrency engine: parallel mode (Planner/Coder/Tester) invocations, protected runner, tool gateway workers, reconciliation, scheduler, optional Jev advisor |
| The Absolute Mathematical Deductor | Mojo | `deductor/` | Bounded Horn-fragment fixpoint + contradiction detection, cross-checked against the Rust reference |

The Go engine never writes state directly. It holds no database handle and
never computes a digest that the core relies on. Every authoritative change
is a command to the core, which validates it and commits an event.

## 1. Canonical encoding and digests (core only)

* Canonical JSON: UTF-8, object keys sorted by byte order, no insignificant
  whitespace, integers only (no floating point anywhere in digest-covered
  data; probabilities are basis points `0..=10000`).
* `digest(kind, value) = "sha256:" + hex(SHA-256("intellectus/v1/" + kind + "\n" + canonical_json(value)))`.
* Blob digest: `"sha256:" + hex(SHA-256("intellectus/v1/blob\n" + bytes))`.
* Tree manifest: canonical JSON object `{path: blob_digest}`; tree root =
  `digest("tree", manifest)`.

## 2. Transport

`intellectus-core serve --db <file> [--deductor rust|mojo:<bin>|cross:<bin>]`

JSON Lines on stdin/stdout, one JSON object per line. stderr is for logs.
The process that spawned `serve` is the authenticated engine principal (it
alone holds the pipe). Commands are processed strictly in arrival order —
the core is the single serialized coordinator; parallelism lives in Go.

Request:  `{"id": <u64>, "cmd": "<name>", "args": {...}}`
Response: `{"id": <u64>, "ok": true, "result": {...}}`
Error:    `{"id": <u64>, "ok": false, "error": {"code": "...", "message": "..."}}`

Transport errors (`ok:false`) mean the command was malformed or refused
before any decision (e.g. `UNKNOWN_SESSION`, `BAD_ARGS`, `SHUTDOWN`).
Decisions (including rejections) are `ok:true` with a `status` field.

### Structured rejection reason

```json
{"code": "ARTIFACT_BINDING_MISMATCH", "failed_check": "acceptance_tests",
 "subject_reference": "prop:12", "missing_dependency": null,
 "counterexample_reference": null,
 "permitted_next_steps": ["REPAIR", "ESCALATE"],
 "remaining_budget": {"repairs": 1}}
```

## 3. Other CLI subcommands

* `intellectus-core keygen --out <file>` → writes a 32-byte Ed25519 seed (hex); prints `{"public_key": "<hex>"}`.
* `intellectus-core sign --key <file> --body <file>` → prints an operator envelope (see §5).
* `intellectus-core init --db <file> --project <id> --operator-pubkey <hex> --policy <file> --root-dir <dir>` → genesis.
* `intellectus-core replay --db <file>` → folds all events from genesis, compares every snapshot digest, prints `{"events": n, "state_digest": "...", "matches": true}`; non-zero exit on any mismatch or unsupported reducer version.
* `intellectus-core deduce [--impl rust|mojo:<bin>] <problem-file>` → prints the deduction result (§7).

## 4. Engine commands

Roles: `planner`, `coder`, `tester` (model modes); `runner` (protected test
runner); `gateway` (tool gateway); `advisor` (Jev / DecisionAdvisor).

| cmd | args | result |
|---|---|---|
| `hello` | `{}` | `{protocol:1, project_id, head_sequence, reducer_version, deductor:{kind, implementation_digest}}` |
| `open_session` | `{role, label}` | `{session_id, principal}` (principal = `"<role>:<label>"`) |
| `view` | `{session_id, task_id}` | permission-filtered view, §4.1 |
| `submit` | `{session_id, raw}` | `{status:"RECORDED", input_id, proposal_id, kind, payload_digest, candidate_root?}` or `{status:"REJECTED", input_id, reasons:[...]}` |
| `check_plan` | `{proposal_id}` | `{checks:[CheckRequest], materialized:{path:content}}` (candidate proposals only) |
| `report_check` | `{session_id, check_id, result, implementation_digest, cases:[{name,status}], collected, completed, summary}` | `{verification_id, result}` (core may downgrade result) |
| `evaluate_context` | `{context_id}` | `{verification_id, result, consistent, conflicts:[atom], derivations:[{derivation_id, status}]}` |
| `admit` | `{proposal_id}` | `{status:"AUTHORIZED", action_id, action_digest}` / `{status:"EXISTING", action_id, action_digest, dispatch_state}` / `{status:"REJECTED", action_digest?, reasons}` |
| `operator` | `{envelope}` | `{status:"APPLIED", event_sequence, result}` or `{status:"REJECTED", reasons}` |
| `dispatch_begin` | `{action_id}` | `{status:"STARTED", attempt_id, tool_id, arguments, idempotency_key}` (external) / `{status:"COMPLETED", result}` (internal, e.g. promote) / `{status:"CANCELLED"|"EXPIRED"|"NOT_ELIGIBLE", reasons}` |
| `record_outcome` | `{session_id, action_id, attempt_id, outcome:"SUCCEEDED"|"FAILED", observation:{...}}` | `{status:"OUTCOME_OBSERVED", postconditions:"PASS"|"FAIL"|"UNKNOWN"}` |
| `record_outcome_unknown` | `{session_id, action_id, attempt_id, error}` | `{status:"OUTCOME_UNKNOWN"}` |
| `pending_reconciliation` | `{}` | `{actions:[{action_id, attempt_id, tool_id, arguments, idempotency_key, dispatch_state}]}` |
| `record_reconciliation` | `{session_id, action_id, finding:{effect_observed: true|false|null, details}}` | `{dispatch_state, requires_human_review}` |
| `route` | `{task_id, failure_codes:[...]}` | `{eligible:[...], deterministic_choice: "<opt>"|null, reason}` |
| `query` | `{session_id, ref, depth}` | permission-filtered dependency expansion `{status:"OK", nodes:[...]}` or `{status:"NOT_FOUND"}` (identical for missing and invisible records) |
| `record_assessment` | `{session_id, task_id, failure_codes, eligible, choice, probabilities_bp, confidence_bp, model_requested, model_returned, mode, latency_ms, usage, question_template_digest, input_manifest_digest, routing_policy_version, provider_response_ref, fallback_reason}` | `{assessment_id, applied_choice, used_advisor}` |
| `task_transition` | `{task_id, to:"STOPPED"|"ESCALATED", reason?}` | `{task_status}` (COMPLETED is never accepted from the engine) |
| `task_report` | `{task_id}` | final report, §4.3 |
| `replay_verify` | `{}` | `{events, state_digest, matches}` |

`failure_codes` use the core's own reason codes (e.g. `CHECK_FAILED`,
`MISSING_APPROVAL`, `REPAIR_BUDGET_EXHAUSTED`). Malformed request lines are
answered with `"id": null`; the engine treats that as a desync.

Route options: `GATHER_CONTEXT`, `REPAIR`, `REPLAN`, `ESCALATE`, `STOP`.
`record_assessment` validates `choice` against the core's own eligible set;
a choice outside it, `mode != LIVE`, or `confidence_bp` below the policy
threshold yields `used_advisor:false` and the deterministic fallback
(`applied_choice` = first eligible of REPAIR, ESCALATE, STOP).

### 4.1 View

```json
{"snapshot_sequence": 17, "state_digest": "sha256:...",
 "dependency_fingerprint": "sha256:...", "policy_version": "P1",
 "task": {"task_id": "task:page-size", "status": "OPEN",
          "requirement_refs": ["req:R1@1"], "repairs_used": 0, "max_repairs": 2,
          "candidates": [{"proposal_id": "prop:9", "candidate_root": "sha256:...", "acceptance": "FAIL"}],
          "last_failures": [{"proposal_id": "prop:9", "failed_cases": ["space_prefix"]}]},
 "requirements": [{"ref": "req:R1@1", "text": "..."}],
 "approved_root": {"digest": "sha256:...", "files": {"src/page.py": "..."}},
 "test_manifest": {"id": "T1", "digest": "sha256:...", "cases": [{"name": "...", "input": "...", "expect": "..."}]},
 "claims": [{"ref": "claim:c1@1", "proposition": "...", "usable": true}]}
```
Records above the role's clearance are omitted entirely (no counts, ids or paths).
A record's effective sensitivity is the maximum of its own label and that of
every record it is built from (claims inherit from their evidence,
derivations from their premises, rule and conclusion).

### 4.2 Model submission envelopes (`submit.raw`, strict: unknown fields rejected)

```json
{"kind":"plan","task_id":"task:x","summary":"...","steps":["..."],"premise_refs":["req:R1@1"]}
{"kind":"candidate","task_id":"task:x","base_root":"sha256:...","files":{"src/page.py":"..."},"deletions":[],"rationale":"..."}
{"kind":"claim","task_id":"task:x","claim_id":"c9","proposition":"...","context":"ctx:x","literal":{"atom":"a","positive":true},"derivation":{"premises":["claim:c1@1"],"rule_id":"rule:r1@1"}}
{"kind":"action","task_id":"task:x","tool_id":"promote_local","arguments":{"candidate_root":"sha256:..."},"premise_refs":["req:R1@1"],"idempotency_key":"k1","expected_postconditions":["approved_root == candidate_root"]}
```
A model cannot submit verification results, receipt ids, approvals or
principals; such fields make the envelope invalid (`SCHEMA_INVALID`).
`raw` may contain surrounding prose; the core extracts exactly one fenced
` ```json ` block or, failing that, parses the whole string.

### 4.3 CheckRequest

```json
{"check_id": "chk:21", "check_kind": "acceptance_tests",
 "subject": {"proposal_id": "prop:9", "base_root": "sha256:...", "candidate_root": "sha256:...", "payload_digest": "sha256:..."},
 "test_manifest": {"id": "T1", "digest": "sha256:...", "cases": [...]},
 "environment": {"id": "ENV1", "digest": "sha256:...", "worker_kind": "fake"},
 "validation_snapshot": 20, "dependency_fingerprint": "sha256:...", "policy_version": "P1"}
```

The runner reports per-case results. The core issues `PASS` only when the
reporting principal and `implementation_digest` are registered trusted
issuers for the check kind, `completed` is true, `collected` equals the
manifest case count, and every manifest case is reported `PASS`. Anything
else is recorded as `FAIL` (a case failed), `UNKNOWN`, `ERROR` or `TIMEOUT`.

## 5. Operator envelope

```json
{"body": "<exact JSON text>", "key_id": "<ed25519 public key hex>", "signature": "<hex>"}
```
`signature = Ed25519(key, "intellectus/v1/operator\n" + body_bytes)`. The core
parses `body` strictly. Every body has `project_id`, `nonce` (unique, replay
protected), `issued_at` (unix seconds) and `op`:

Operation fields sit flat beside `project_id`, `nonce`, `issued_at` and `op`
(e.g. `{"project_id":"p","nonce":"n1","issued_at":1800000000,"op":"approve","action_digest":"sha256:…","tool_id":"promote_local","expires_at":1800003600}`).
Node-creating ops return `{"ref":"kind:id@rev"}`; `register_test_manifest` and
`register_environment` return `{"digest"}` and are referenced by bare id
(`"T1"`, `"ENV1"`) in `open_task`. All ids and refs are strings.

`set_policy{policy}`, `add_requirement{requirement_id,text,sensitivity}`,
`add_assumption{assumption_id,text,context,literal?,sensitivity}`,
`add_evidence{evidence_id,content,source_locator,collection_method,sensitivity}`,
`add_claim{claim_id,proposition,context,literal?,basis:"observed"|"assumed",support_refs,sensitivity}`,
`add_rule{rule_id,body:[literal],head:literal}`,
`add_derivation{derivation_id,conclusion,premises,rule_id,context}`,
`register_test_manifest{manifest_id,cases}`, `register_environment{environment_id,description,worker_kind}`,
`open_task{task_id,requirement_refs,test_manifest,environment,contexts,max_repairs?}`,
`approve{action_digest,tool_id,expires_at}`, `revoke{ref,reason}`,
`cancel_task{task_id}`, `shutdown{}`.

## 6. Dispatch phases

```
PROPOSED -> CHECKED -> AUTHORIZED_INTENT -> DISPATCH_STARTED -> OUTCOME_OBSERVED -> POSTCONDITIONS_EVALUATED
branches: REJECTED, CANCELLED, EXPIRED, OUTCOME_UNKNOWN
```
`dispatch_begin` is the launch boundary: authorization, cancellation, policy,
dependency freshness and payload identity are re-checked and
`DISPATCH_STARTED` is committed before the payload is returned. On core
restart every `DISPATCH_STARTED` action without an outcome becomes
`OUTCOME_UNKNOWN`. The engine must never re-dispatch such an action; it
calls the adapter's reconcile read and reports `record_reconciliation`.

## 7. Deduction problem format (core ⇄ deductor)

Literals are non-zero integers: `+i` is atom `i` true, `-i` is atom `i`
false, atoms numbered `1..=N`. Line-oriented ASCII, tokens separated by
single spaces.

```
INTELLECTUS-DEDUCE 1
atoms <N>
facts <K>
<lit>                  (K lines)
rules <R>
<head> <n> <b1> ... <bn>   (R lines; rule index = 0-based line order; n may be 0)
end
```

Semantics (Jacobi rounds, identical in every implementation):

* Round 0: `D0` = the set of facts. Each fact's justification is `-1`, depth 0.
* Round k+1: for every rule whose body literals are all in `D_k`
  and whose head is not in `D_k`, add the head. Its justification is the
  **smallest rule index** among such rules for that head in this round;
  its depth is k+1. `D_{k+1} = D_k ∪ new heads`.
* Stop when a round adds nothing.
* Conflicts: atoms `i` with both `+i` and `-i` in the fixpoint.

Output:
```
INTELLECTUS-DEDUCED 1
derived <M>
<lit> <justification> <depth>    (M lines, sorted by |lit| asc, then positive before negative)
conflicts <C>
<atom>                            (C lines, ascending)
consistent <0|1>
end
```

Malformed input → exit code 2 and no `INTELLECTUS-DEDUCED` header. Any
disagreement between implementations in `cross` mode fails closed
(`DEDUCTOR_DISAGREEMENT`, result `ERROR`).

Limits: `N ≤ 1_000_000`, `R ≤ 1_000_000`, total body literals ≤ 10_000_000.
Violations are malformed input.

## 8. v0.2 additions (schema 2)

Databases created by v0.1 (schema 1) are refused with `UNSUPPORTED_SCHEMA`;
there is no migration (v0.1 databases were demo-only).

* Roles `intake` (may only `record_input`) and `scheduler` (may only
  `submit` envelopes of kind `action` — runtime-built actions such as
  promoting a candidate that passed its checks).
* `submit` and the new `record_input {session_id, raw, provider_record?}`
  accept `provider_record = {provider, model_requested, model_returned,
  response_id, raw_response, usage, latency_ms, attempts}`. `raw_response`
  (the full provider response) is stored as a protected blob; prompts are
  never stored. `usage` must be integer-only.
* `register_test_manifest` takes an optional `entrypoint = {language:
  "python", path, function}` covered by the manifest digest; `check_plan`
  and `view` return it. Case expectations used by the sandbox runner are
  `{"returns": <json>}` or `{"raises": "<ExceptionName>"}`.
* `open_task` takes an optional `title`; `test_manifest` and `environment`
  are optional — a task without them can never pass `acceptance_tests`
  (fails closed with `MISSING_TEST_MANIFEST`) and completes only when an
  external effect's postconditions pass (effect-only tasks such as export).
* Read-only commands for the UI: `list_tasks {}`, `read_tree {root?}`,
  `events_since {after, limit}` (with a one-line `summary` per event) and
  `status {}` (policy, environments, deductor, approved root).
