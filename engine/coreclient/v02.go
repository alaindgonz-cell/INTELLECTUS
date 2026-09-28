package coreclient

import (
	"context"
	"encoding/json"
)

// Roles added in protocol v0.2.
const (
	RoleIntake    = "intake"
	RoleScheduler = "scheduler"
)

// ProviderRecord is the provenance of a model response, attached by the
// (trusted) provider adapter to `submit` / `record_input`. RawResponse is
// stored by the core as a protected blob; prompts are never sent.
type ProviderRecord struct {
	Provider       string          `json:"provider"`
	ModelRequested string          `json:"model_requested"`
	ModelReturned  string          `json:"model_returned"`
	ResponseID     string          `json:"response_id,omitempty"`
	RawResponse    *string         `json:"raw_response,omitempty"`
	Usage          json.RawMessage `json:"usage,omitempty"`
	LatencyMS      int64           `json:"latency_ms,omitempty"`
	Attempts       int             `json:"attempts,omitempty"`
}

// SubmitWithProvider is `submit` with provider provenance.
func (c *Client) SubmitWithProvider(ctx context.Context, session ID, raw string, p *ProviderRecord) (*SubmitResult, error) {
	args := map[string]any{"session_id": session, "raw": raw}
	if p != nil {
		args["provider_record"] = p
	}
	return call[SubmitResult](ctx, c, "submit", args)
}

// RecordInputResult answers `record_input`.
type RecordInputResult struct {
	WithRaw
	Status  string `json:"status"`
	InputID ID     `json:"input_id"`
}

// RecordInput records model output that is not a proposal (chat replies,
// diagnoses): `record_input {session_id, raw, provider_record?}`.
func (c *Client) RecordInput(ctx context.Context, session ID, raw string, p *ProviderRecord) (*RecordInputResult, error) {
	args := map[string]any{"session_id": session, "raw": raw}
	if p != nil {
		args["provider_record"] = p
	}
	return call[RecordInputResult](ctx, c, "record_input", args)
}

// TaskSummary is one entry of `list_tasks`.
type TaskSummary struct {
	TaskID           string          `json:"task_id"`
	Title            string          `json:"title"`
	Status           string          `json:"status"`
	RequirementRefs  []string        `json:"requirement_refs"`
	Requirement      *string         `json:"requirement"`
	TestManifest     *string         `json:"test_manifest"`
	Environment      *string         `json:"environment"`
	Entrypoint       *Entrypoint     `json:"entrypoint"`
	Cases            []TestCase      `json:"cases"`
	Contexts         []string        `json:"contexts"`
	MaxRepairs       int64           `json:"max_repairs"`
	RepairsUsed      int64           `json:"repairs_used"`
	FailedCandidates int64           `json:"failed_candidates"`
	Candidates       []ViewCandidate `json:"candidates"`
	CompletedBy      *string         `json:"completed_by"`
}

// ListTasksResult answers `list_tasks`.
type ListTasksResult struct {
	WithRaw
	Tasks            []TaskSummary `json:"tasks"`
	SnapshotSequence int64         `json:"snapshot_sequence"`
}

// ListTasks: `list_tasks {}`.
func (c *Client) ListTasks(ctx context.Context) (*ListTasksResult, error) {
	return call[ListTasksResult](ctx, c, "list_tasks", struct{}{})
}

// ReadTreeResult answers `read_tree`.
type ReadTreeResult struct {
	WithRaw
	Root  string            `json:"root"`
	Files map[string]string `json:"files"`
}

// ReadTree materializes a tree (empty root = the approved root).
func (c *Client) ReadTree(ctx context.Context, root string) (*ReadTreeResult, error) {
	args := map[string]any{}
	if root != "" {
		args["root"] = root
	}
	return call[ReadTreeResult](ctx, c, "read_tree", args)
}

// EventSummary is one entry of `events_since`.
type EventSummary struct {
	Sequence   int64  `json:"sequence"`
	Type       string `json:"type"`
	Actor      string `json:"actor"`
	RecordedAt int64  `json:"recorded_at"`
	Summary    string `json:"summary"`
}

// EventsSinceResult answers `events_since`.
type EventsSinceResult struct {
	WithRaw
	Events       []EventSummary `json:"events"`
	HeadSequence int64          `json:"head_sequence"`
}

// EventsSince pages the authoritative event log.
func (c *Client) EventsSince(ctx context.Context, after, limit int64) (*EventsSinceResult, error) {
	return call[EventsSinceResult](ctx, c, "events_since", map[string]any{"after": after, "limit": limit})
}

// CoreStatus answers `status`.
type CoreStatus struct {
	WithRaw
	ProjectID      string          `json:"project_id"`
	HeadSequence   int64           `json:"head_sequence"`
	StateDigest    string          `json:"state_digest"`
	ApprovedRoot   string          `json:"approved_root"`
	ReducerVersion int64           `json:"reducer_version"`
	SchemaVersion  int64           `json:"schema_version"`
	PolicyVersion  string          `json:"policy_version"`
	PolicyDigest   string          `json:"policy_digest"`
	Policy         json.RawMessage `json:"policy"`
	Deductor       struct {
		Kind                 string `json:"kind"`
		ImplementationDigest string `json:"implementation_digest"`
	} `json:"deductor"`
	Shutdown     bool `json:"shutdown"`
	Tasks        int  `json:"tasks"`
	Environments []struct {
		ID          string `json:"id"`
		Digest      string `json:"digest"`
		WorkerKind  string `json:"worker_kind"`
		Description string `json:"description"`
	} `json:"environments"`
	TestManifests []string `json:"test_manifests"`
}

// Status: `status {}`.
func (c *Client) Status(ctx context.Context) (*CoreStatus, error) {
	return call[CoreStatus](ctx, c, "status", struct{}{})
}
