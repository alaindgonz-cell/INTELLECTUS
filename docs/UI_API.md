# INTELLECTUS web UI — HTTP API (v0.2)

Served by `intellectus serve` (Go, `engine/web`). Binds `127.0.0.1:7777` by
default. The UI is a single page (`engine/web/static/`: `index.html`,
`app.js`, `style.css`) embedded in the binary. No build step, no external
network requests (no CDNs): the UI must work fully offline.

## Authentication

* The launch URL is `http://127.0.0.1:7777/?token=<random>`. `GET /?token=…`
  with the right token sets cookie `intellectus_session` (HttpOnly,
  SameSite=Strict, Path=/) and redirects to `/`.
* Every `/api/*` request needs that cookie. Every non-GET request must also
  send header `X-Intellectus-CSRF: 1` (a custom header forces a CORS
  preflight, which the server never grants). The server also checks
  `Origin` on non-GET requests.
* 401 → the UI shows "Session expired — reopen the URL printed by
  `intellectus serve`".

## Security rule for the UI (mandatory)

All model output (assistant chat text, proposals, plans, code, test output,
diagnoses) is **untrusted**. Render it with `textContent` / text nodes only
— never `innerHTML` with interpolated content. The only "formatting" allowed
is: splitting fenced ``` code blocks into `<pre><code>` elements whose text
is set with `textContent`, inline `code` spans, and line breaks.

## Objects

### Status — `GET /api/status`

```json
{
  "project_id": "myproj",
  "approved_root": "sha256:…",
  "head_sequence": 88,
  "core": {"reducer_version": 2, "deductor": "cross", "deductor_digest": "cross(rust-reference/…,mojo/sha256:…)"},
  "claude": {"configured": true, "provider": "anthropic", "models": {"intake": "claude-opus-5", "planner": "claude-opus-5", "coder": "claude-opus-5", "tester": "claude-opus-5"}, "detail": ""},
  "jev": {"configured": true, "policy_mode": "SHADOW", "model": "jev-1.13.0", "detail": ""},
  "sandbox": {"available": true, "verified": true, "trusted": true, "kind": "bwrap", "implementation_digest": "bwrap-python/v1:sha256:…",
              "probes": [{"name": "network_egress_blocked", "ok": true, "detail": "connect 1.1.1.1:443: OSError"}],
              "verified_at": 1790000000000, "detail": ""},
  "usage": {"model_calls": 12, "input_tokens": 51234, "output_tokens": 9876, "cache_read_input_tokens": 30000,
            "estimated_cost_usd": "0.4812", "jev_calls": 3, "jev_cost_usd": "0.0000"},
  "busy": true
}
```

### Chat message — `GET /api/chat` → `{"messages": [Message…]}`

```json
{"id": "m-12", "role": "user|assistant|system", "ts": 1790000000000, "text": "…",
 "task_id": "task:3", "card": null}
```

`card` is null or one of:

```json
{"type": "proposal", "proposal_id": "p-4", "status": "pending|approved|discarded",
 "title": "Strict page-size parsing",
 "requirement": "Accept only …",
 "entrypoint": {"language": "python", "path": "src/page_size.py", "function": "parse_page_size"},
 "cases": [{"name": "ok_1", "input": "1", "expect": {"returns": 1}},
           {"name": "plus", "input": "+1", "expect": {"raises": "ValueError"}}],
 "task_id": null}

{"type": "approval", "approval_id": "a-7", "status": "pending|approved|rejected|stale",
 "task_id": "task:3", "tool_id": "promote_local|export_view",
 "title": "Promote candidate to the approved tree",
 "action_digest": "sha256:…", "from_root": "sha256:…", "to_root": "sha256:…",
 "checks": [{"kind": "acceptance_tests", "result": "PASS", "issuer": "runner:sandbox", "detail": "11/11 cases passed in bwrap sandbox"},
            {"kind": "formal_context", "result": "PASS", "issuer": "core:deductor:cross", "detail": "spec consistent"}],
 "summary": "…"}

{"type": "report", "task_id": "task:3", "status": "COMPLETED|STOPPED|ESCALATED|CANCELLED|FAILED",
 "approved_root": "sha256:…", "lines": ["…"], "limitations": ["…"]}
```

`POST /api/chat` body `{"text": "…"}` → `{"message_id": "m-13"}`. The reply
arrives over SSE (`chat` events), possibly with a proposal card.

### Proposals and approvals

* `POST /api/proposals/{id}/approve` → `{"task_id": "task:3"}` (operator
  signs requirement + acceptance tests + task; work starts).
* `POST /api/proposals/{id}/discard` → `{}`.
* `POST /api/approvals/{id}/approve` → `{}` (operator signs an approval
  bound to the exact `action_digest`).
* `POST /api/approvals/{id}/reject` → `{}`.

### Activity — what INTELLECTUS is doing

`GET /api/activity?limit=200` → `{"items": [Item…]}` (oldest first).

```json
{"id": "act-41", "ts": 1790000000000, "task_id": "task:3",
 "kind": "model|sandbox|core|jev|approval|gateway|system|error",
 "status": "running|ok|fail|warn|info",
 "title": "Coder (claude-opus-5) is writing a candidate",
 "detail": "optional multi-line text (untrusted if it contains model output)",
 "data": {"optional": "structured fields"}}
```

An item with an existing `id` **replaces** the earlier one (e.g. a
`running` item later becomes `ok`).

### Tasks

* `GET /api/tasks` → `{"tasks": [{"task_id", "title", "status", "repairs_used", "max_repairs", "failed_candidates", "candidates": 3, "phase": "coding|testing|routing|awaiting_approval|done|…", "updated_ts"}]}`
* `GET /api/tasks/{id}` → task detail:
  `{"task": {…as above}, "requirement": "…", "entrypoint": {…}, "cases": […],
    "candidates": [{"proposal_id", "candidate_root", "acceptance": "PASS|FAIL|…", "details": [CaseDetail…]}],
    "assessments": [{"assessment_id", "choice", "confidence_bp", "applied_choice", "used_advisor", "fallback_reason", "mode"}],
    "actions": [{"action_id", "tool_id", "state", "postconditions"}],
    "report": {…core task_report…}}`
  where `CaseDetail = {"name", "status", "input", "expected", "observed", "stdout", "stderr", "duration_ms"}`.
* `POST /api/tasks/{id}/cancel` → `{}`.

### Files and diffs

* `GET /api/tree?root=<digest>` (default: approved root) → `{"root": "sha256:…", "files": {"path": "content"}}`.
* `GET /api/diff?from=<digest>&to=<digest>` → `{"from", "to", "files": [{"path", "status": "added|removed|modified", "old": "…", "new": "…"}]}`
  (unchanged files omitted). The UI renders a line diff client-side.

### Event log (authoritative core history)

`GET /api/events?after=<seq>&limit=100` → `{"events": [{"sequence", "type", "actor", "recorded_at", "summary"}]}`.

### Operator controls

* `POST /api/export` → `{"approval_id": "a-9"}` — proposes exporting the
  approved tree to the export directory (then needs its own approval).
* `POST /api/policy/jev-mode` body `{"mode": "SHADOW|LIVE|OFF"}` → `{}`.
* `POST /api/policy/trust-sandbox` → `{}` — trusts the current sandbox
  implementation digest (a policy change the operator signs).

## Server-sent events — `GET /api/stream`

`text/event-stream`; each event has `event:` and a JSON `data:` line:

| event | data |
|---|---|
| `activity` | an activity Item (upsert by id) |
| `chat` | a chat Message (upsert by id; cards change status this way) |
| `status` | a Status object |
| `tasks` | `{"changed": "task:3"}` — refetch `/api/tasks` (and detail if open) |
| `ping` | `{}` every 15 s |

The UI reconnects automatically (EventSource) and refetches `/api/chat`,
`/api/activity`, `/api/status` and `/api/tasks` after a reconnect.
