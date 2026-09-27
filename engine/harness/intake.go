package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/alaindgonz-cell/intellectus/engine/llm"
)

// proposal is a model-proposed task awaiting the operator's decision.
type proposal struct {
	ID          string
	MessageID   string
	InputID     string
	Title       string
	Requirement string
	Entry       EntrySpec
	Cases       []CaseView
	Status      string // pending|approved|discarded
	TaskID      string
}

var (
	identRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	caseNameRe = regexp.MustCompile(`^[a-z0-9_]{1,48}$`)
	excRe      = regexp.MustCompile(`^[A-Z][A-Za-z0-9_]{0,63}$`)
)

// ErrBusy is returned when a chat reply is already being produced.
var ErrBusy = errors.New("INTELLECTUS is still answering the previous message")

// SendChat records an operator message and produces the reply asynchronously.
func (h *Harness) SendChat(text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("empty message")
	}
	if len(text) > 20000 {
		return "", errors.New("message too long (max 20000 characters)")
	}
	h.mu.Lock()
	if h.chatBusy {
		h.mu.Unlock()
		return "", ErrBusy
	}
	h.chatBusy = true
	h.mu.Unlock()
	m := h.post("user", text, "", nil)
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer func() {
			h.mu.Lock()
			h.chatBusy = false
			h.mu.Unlock()
			h.publishStatus()
		}()
		h.reply(h.rootCtx)
	}()
	h.publishStatus()
	return m.ID, nil
}

func (h *Harness) reply(ctx context.Context) {
	if !h.opt.LLM.Describe().Configured {
		h.post("system", "Claude is not configured, so INTELLECTUS cannot answer. Set ANTHROPIC_API_KEY in the environment of `intellectus serve` and restart.", "", nil)
		return
	}
	act := h.act("", "model", "running", "Intake ("+h.model("intake")+") is reading your message", "", nil)
	msgs, err := h.intakeMessages(ctx)
	if err != nil {
		h.update(act, "fail", "Could not build the project context", err.Error(), nil)
		h.post("system", "Could not read the project state: "+err.Error(), "", nil)
		return
	}
	call := llm.Call{Role: "intake", System: intakeSystem, Messages: msgs, Tool: proposeTaskTool, MaxTokens: 32000}
	var (
		res     llm.Result
		prop    *proposal
		propErr error
	)
	for attempt := 0; attempt < 2; attempt++ {
		res, err = h.opt.LLM.Complete(ctx, call)
		if err != nil {
			break
		}
		h.meter(res)
		if res.ToolInput == nil {
			break
		}
		prop, propErr = parseProposal(res.ToolInput)
		if propErr == nil {
			break
		}
		// Bounded semantic repair: show the model why its proposal was invalid once.
		call.Messages = append(call.Messages,
			llm.Message{Role: "assistant", Text: "(proposed a task via propose_task)"},
			llm.Message{Role: "user", Text: "That proposal was rejected by INTELLECTUS: " + propErr.Error() + ". Please correct it and call propose_task again, or explain the problem to the operator."})
		prop = nil
	}
	if err != nil {
		h.update(act, "fail", "Intake failed", err.Error(), nil)
		h.post("system", "The model call failed: "+err.Error(), "", nil)
		return
	}
	// Record the model output (with provenance) as untrusted input.
	rec, rerr := h.opt.Core.RecordInput(ctx, h.sess.intake, recordText(res), providerRecord(res))
	inputID := ""
	if rerr == nil {
		inputID = string(rec.InputID)
	} else {
		h.opt.Logf("record_input: %v", rerr)
	}
	text := strings.TrimSpace(res.Text)
	if prop == nil {
		if propErr != nil && res.ToolInput != nil {
			text = strings.TrimSpace(text + "\n\n(INTELLECTUS rejected the proposed task: " + propErr.Error() + ")")
		}
		if text == "" {
			text = "(no reply)"
		}
		h.update(act, "ok", "Intake replied", "", map[string]any{"model": res.ModelReturned, "input_id": inputID})
		h.post("assistant", text, "", nil)
		return
	}
	h.mu.Lock()
	h.nextProp++
	prop.ID = fmt.Sprintf("p-%d", h.nextProp)
	prop.InputID = inputID
	prop.Status = "pending"
	h.proposals[prop.ID] = prop
	h.mu.Unlock()
	if text == "" {
		text = "I propose the following task. Review the requirement and the acceptance tests; nothing runs until you approve."
	}
	m := h.post("assistant", text, "", prop.card())
	h.mu.Lock()
	prop.MessageID = m.ID
	h.mu.Unlock()
	h.update(act, "ok", "Intake proposed a task: "+prop.Title, "", map[string]any{"proposal_id": prop.ID, "input_id": inputID})
}

// recordText is what gets recorded as the untrusted input for a response.
func recordText(res llm.Result) string {
	if res.ToolInput == nil {
		return res.Text
	}
	return res.Text + "\n```json\n" + string(res.ToolInput) + "\n```\n"
}

func (h *Harness) model(role string) string {
	if m := h.opt.LLM.Describe().Models[role]; m != "" {
		return m
	}
	return "model"
}

// intakeMessages renders the chat history plus current project context.
func (h *Harness) intakeMessages(ctx context.Context) ([]llm.Message, error) {
	history := h.chat.list()
	var msgs []llm.Message
	for _, m := range history {
		switch m.Role {
		case "user":
			msgs = append(msgs, llm.Message{Role: "user", Text: m.Text})
		case "assistant":
			t := m.Text
			if m.Card != nil && m.Card.Type == "proposal" {
				t += fmt.Sprintf("\n[proposed task %q (%s), status: %s]", m.Card.Title, m.Card.ProposalID, m.Card.Status)
			}
			msgs = append(msgs, llm.Message{Role: "assistant", Text: t})
		case "system":
			// System notices are shown to the operator, not replayed as turns.
		}
	}
	// Keep the most recent turns, starting with a user turn, alternating.
	if len(msgs) > 24 {
		msgs = msgs[len(msgs)-24:]
	}
	msgs = normalizeTurns(msgs)
	if len(msgs) == 0 || msgs[len(msgs)-1].Role != "user" {
		return nil, errors.New("no operator message to answer")
	}
	pc, err := h.projectContext(ctx)
	if err != nil {
		return nil, err
	}
	last := &msgs[len(msgs)-1]
	last.Text = "<project_context>\n" + pc + "</project_context>\n\n<operator_message>\n" + last.Text + "\n</operator_message>"
	return msgs, nil
}

// normalizeTurns merges consecutive same-role turns and drops a leading
// assistant turn so the conversation alternates starting with "user".
func normalizeTurns(in []llm.Message) []llm.Message {
	var out []llm.Message
	for _, m := range in {
		if len(out) == 0 && m.Role != "user" {
			continue
		}
		if len(out) > 0 && out[len(out)-1].Role == m.Role {
			out[len(out)-1].Text += "\n\n" + m.Text
			continue
		}
		out = append(out, m)
	}
	return out
}

const contextBudget = 60000

func (h *Harness) projectContext(ctx context.Context) (string, error) {
	tree, err := h.opt.Core.ReadTree(ctx, "")
	if err != nil {
		return "", err
	}
	tasks, err := h.opt.Core.ListTasks(ctx)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(renderFiles(tree.Files, contextBudget, nil))
	b.WriteString("\nTasks:\n")
	if len(tasks.Tasks) == 0 {
		b.WriteString("(none yet)\n")
	}
	for _, t := range tasks.Tasks {
		fmt.Fprintf(&b, "- %s %q: %s (candidates %d, repairs %d/%d)\n", t.TaskID, t.Title, t.Status, len(t.Candidates), t.RepairsUsed, t.MaxRepairs)
	}
	return b.String(), nil
}

// renderFiles lists files and includes contents within a character budget;
// files in `first` are included before the others.
func renderFiles(files map[string]string, budget int, first []string) string {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var b strings.Builder
	fmt.Fprintf(&b, "Approved project tree (%d files):\n", len(paths))
	for _, p := range paths {
		fmt.Fprintf(&b, "- %s (%d bytes)\n", p, len(files[p]))
	}
	ordered := append([]string{}, first...)
	for _, p := range paths {
		if !contains(ordered, p) {
			ordered = append(ordered, p)
		}
	}
	used := 0
	var omitted []string
	for _, p := range ordered {
		c, ok := files[p]
		if !ok {
			continue
		}
		if used+len(c) > budget {
			omitted = append(omitted, p)
			continue
		}
		used += len(c)
		fmt.Fprintf(&b, "\n<file path=%q>\n%s\n</file>\n", p, c)
	}
	if len(omitted) > 0 {
		fmt.Fprintf(&b, "\n(contents omitted for size: %s)\n", strings.Join(omitted, ", "))
	}
	return b.String()
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// parseProposal strictly decodes and validates a propose_task tool input.
func parseProposal(raw json.RawMessage) (*proposal, error) {
	var in struct {
		Title       string `json:"title"`
		Requirement string `json:"requirement"`
		Path        string `json:"path"`
		Function    string `json:"function"`
		Cases       []struct {
			Name      string `json:"name"`
			InputJSON string `json:"input_json"`
			Expect    string `json:"expect"`
			ValueJSON string `json:"value_json"`
			Exception string `json:"exception"`
		} `json:"cases"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf("malformed proposal: %v", err)
	}
	p := &proposal{Title: strings.TrimSpace(in.Title), Requirement: strings.TrimSpace(in.Requirement)}
	switch {
	case p.Title == "" || len(p.Title) > 120:
		return nil, errors.New("title must be 1-120 characters")
	case p.Requirement == "" || len(p.Requirement) > 8000:
		return nil, errors.New("requirement must be 1-8000 characters")
	case !validPyPath(in.Path):
		return nil, fmt.Errorf("path %q must be a relative .py path without '..' outside tests/acceptance/", in.Path)
	case !identRe.MatchString(in.Function):
		return nil, fmt.Errorf("function %q is not a Python identifier", in.Function)
	case len(in.Cases) < 3 || len(in.Cases) > 40:
		return nil, fmt.Errorf("need 3-40 test cases, got %d", len(in.Cases))
	}
	p.Entry = EntrySpec{Language: "python", Path: in.Path, Function: in.Function}
	seen := map[string]bool{}
	byInput := map[string]string{}
	for _, c := range in.Cases {
		if !caseNameRe.MatchString(c.Name) || seen[c.Name] {
			return nil, fmt.Errorf("case name %q must be unique snake_case", c.Name)
		}
		seen[c.Name] = true
		input, err := canonicalJSON(c.InputJSON)
		if err != nil {
			return nil, fmt.Errorf("case %s: input_json is not valid JSON: %v", c.Name, err)
		}
		var expect map[string]any
		switch c.Expect {
		case "returns":
			v, err := canonicalJSON(c.ValueJSON)
			if err != nil {
				return nil, fmt.Errorf("case %s: value_json is not valid JSON: %v", c.Name, err)
			}
			expect = map[string]any{"returns": json.RawMessage(v)}
		case "raises":
			if !excRe.MatchString(c.Exception) {
				return nil, fmt.Errorf("case %s: exception %q is not a Python exception class name", c.Name, c.Exception)
			}
			expect = map[string]any{"raises": c.Exception}
		default:
			return nil, fmt.Errorf("case %s: expect must be returns or raises", c.Name)
		}
		eb, _ := json.Marshal(expect)
		if prev, ok := byInput[string(input)]; ok && prev != string(eb) {
			return nil, fmt.Errorf("cases disagree about input %s", input)
		}
		byInput[string(input)] = string(eb)
		p.Cases = append(p.Cases, CaseView{Name: c.Name, Input: json.RawMessage(input), Expect: json.RawMessage(eb)})
	}
	return p, nil
}

func validPyPath(p string) bool {
	if p == "" || len(p) > 200 || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || !strings.HasSuffix(p, ".py") {
		return false
	}
	if strings.HasPrefix(p, "tests/acceptance/") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// canonicalJSON validates s as a single JSON value and re-encodes it
// compactly. Non-integer numbers are refused: manifests are digest-covered
// and the core's canonical encoding is integer-only.
func canonicalJSON(s string) ([]byte, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data")
	}
	if hasFloat(v) {
		return nil, errors.New("non-integer numbers are not supported in test data (the core's digests are integer-only); use strings or scaled integers")
	}
	return json.Marshal(v)
}

func hasFloat(v any) bool {
	switch t := v.(type) {
	case json.Number:
		_, err := t.Int64()
		return err != nil
	case []any:
		for _, x := range t {
			if hasFloat(x) {
				return true
			}
		}
	case map[string]any:
		for _, x := range t {
			if hasFloat(x) {
				return true
			}
		}
	}
	return false
}

func (p *proposal) card() *Card {
	c := &Card{
		Type: "proposal", ProposalID: p.ID, Title: p.Title, Requirement: p.Requirement,
		Entrypoint: &EntrySpec{Language: p.Entry.Language, Path: p.Entry.Path, Function: p.Entry.Function},
		Cases:      p.Cases, Status: p.Status,
	}
	if p.TaskID != "" {
		t := p.TaskID
		c.TaskID = &t
	}
	return c
}

func (h *Harness) refreshProposalCard(p *proposal) {
	m, ok := h.chat.get(p.MessageID)
	if !ok {
		return
	}
	m.Card = p.card()
	h.repost(m)
}

// DiscardProposal marks a pending proposal discarded.
func (h *Harness) DiscardProposal(id string) error {
	h.mu.Lock()
	p, ok := h.proposals[id]
	if !ok || p.Status != "pending" {
		h.mu.Unlock()
		return fmt.Errorf("proposal %s is not pending", id)
	}
	p.Status = "discarded"
	h.mu.Unlock()
	h.refreshProposalCard(p)
	h.act("", "approval", "info", "Operator discarded proposal "+id+": "+p.Title, "", nil)
	return nil
}

// ApproveProposal is the operator's authorization of a task: it signs the
// requirement, the protected acceptance manifest, the spec premises and the
// task, then starts work.
func (h *Harness) ApproveProposal(ctx context.Context, id string) (string, error) {
	h.mu.Lock()
	p, ok := h.proposals[id]
	if !ok || p.Status != "pending" {
		h.mu.Unlock()
		return "", fmt.Errorf("proposal %s is not pending", id)
	}
	p.Status = "approving"
	h.mu.Unlock()
	taskID, err := h.openTask(ctx, p)
	h.mu.Lock()
	if err != nil {
		p.Status = "pending"
	} else {
		p.Status, p.TaskID = "approved", taskID
	}
	h.mu.Unlock()
	if err != nil {
		return "", err
	}
	h.refreshProposalCard(p)
	h.startRun(taskID, p.Title, false)
	return taskID, nil
}

func (h *Harness) openTask(ctx context.Context, p *proposal) (string, error) {
	if h.opt.Worker == nil || h.env == "" {
		return "", errors.New("no verified, trusted sandbox is available: acceptance tests cannot run, so a task cannot be started")
	}
	tasks, err := h.opt.Core.ListTasks(ctx)
	if err != nil {
		return "", err
	}
	taskID := fmt.Sprintf("task-%d", len(tasks.Tasks)+1)
	act := h.act(taskID, "approval", "running", "Operator approved "+p.ID+": registering requirement, tests and task", "", nil)
	op := func(name string, params map[string]any) (json.RawMessage, error) {
		res, err := h.opt.Core.OperatorOp(ctx, h.opt.Operator, name, params)
		if err != nil {
			return nil, err
		}
		if res.Status != "APPLIED" {
			return nil, fmt.Errorf("%s rejected: %s", name, res.Reasons.Codes())
		}
		return res.Result, nil
	}
	fail := func(err error) (string, error) {
		h.update(act, "fail", "Could not register the task", err.Error(), nil)
		return "", err
	}
	approved, _ := json.Marshal(map[string]any{"proposal": p.ID, "title": p.Title, "requirement": p.Requirement, "entrypoint": p.Entry, "cases": p.Cases, "model_input": p.InputID})
	if _, err := op("add_evidence", map[string]any{
		"evidence_id": "E-" + taskID, "content": string(approved), "source_locator": "chat:" + p.MessageID,
		"collection_method": "operator-approved-proposal/v1",
	}); err != nil {
		return fail(err)
	}
	r, err := op("add_requirement", map[string]any{"requirement_id": "R-" + taskID, "text": p.Requirement})
	if err != nil {
		return fail(err)
	}
	var ref struct {
		Ref string `json:"ref"`
	}
	_ = json.Unmarshal(r, &ref)
	if _, err := op("register_test_manifest", map[string]any{
		"manifest_id": "T-" + taskID, "cases": p.Cases,
		"entrypoint": map[string]string{"language": "python", "path": p.Entry.Path, "function": p.Entry.Function},
	}); err != nil {
		return fail(err)
	}
	// Spec-consistency premises: each case stipulates that its input is
	// accepted (returns) or rejected (raises). The deductor checks that the
	// approved tests never both accept and reject the same input.
	ctxID := "ctx:" + taskID
	for _, c := range p.Cases {
		var ex map[string]any
		_ = json.Unmarshal(c.Expect, &ex)
		_, accepts := ex["returns"]
		sum := sha256.Sum256(c.Input)
		verb := "accepts"
		if !accepts {
			verb = "rejects"
		}
		if _, err := op("add_assumption", map[string]any{
			"assumption_id": "spec." + taskID + "." + c.Name, "context": ctxID,
			"text":    fmt.Sprintf("Approved test %s: %s(%s) %s", c.Name, p.Entry.Function, string(c.Input), verb),
			"literal": map[string]any{"atom": "accepts." + hex.EncodeToString(sum[:8]), "positive": accepts},
		}); err != nil {
			return fail(err)
		}
	}
	if _, err := op("open_task", map[string]any{
		"task_id": taskID, "title": p.Title, "requirement_refs": []string{ref.Ref},
		"test_manifest": "T-" + taskID, "environment": h.env, "contexts": []string{ctxID},
	}); err != nil {
		return fail(err)
	}
	h.update(act, "ok", "Task "+taskID+" opened: "+p.Title, fmt.Sprintf("requirement %s, %d protected acceptance tests, environment %s", ref.Ref, len(p.Cases), h.env), nil)
	h.bus.Publish("tasks", map[string]any{"changed": taskID})
	return taskID, nil
}

// restore reloads proposals from the chat log after a restart and notes
// interrupted work (in-memory waits cannot survive a restart).
func (h *Harness) restore(ctx context.Context) {
	for _, m := range h.chat.list() {
		if m.Card == nil {
			continue
		}
		switch m.Card.Type {
		case "proposal":
			p := &proposal{ID: m.Card.ProposalID, MessageID: m.ID, Title: m.Card.Title, Requirement: m.Card.Requirement, Cases: m.Card.Cases, Status: m.Card.Status}
			if m.Card.Entrypoint != nil {
				p.Entry = *m.Card.Entrypoint
			}
			if m.Card.TaskID != nil {
				p.TaskID = *m.Card.TaskID
			}
			if p.Status == "approving" {
				p.Status = "pending"
			}
			h.proposals[p.ID] = p
			var n int
			if _, err := fmt.Sscanf(p.ID, "p-%d", &n); err == nil && n > h.nextProp {
				h.nextProp = n
			}
		case "approval":
			var n int
			if _, err := fmt.Sscanf(m.Card.ApprovalID, "a-%d", &n); err == nil && n > h.nextAppr {
				h.nextAppr = n
			}
			if m.Card.Status == "pending" {
				// The waiting task loop is gone; the approval can no longer be acted on.
				m.Card.Status = "stale"
				h.repost(m)
			}
		}
	}
	tasks, err := h.opt.Core.ListTasks(ctx)
	if err != nil {
		return
	}
	for _, t := range tasks.Tasks {
		if t.Status == "OPEN" && t.TestManifest != nil {
			h.post("system", fmt.Sprintf("Task %s (%q) was interrupted by a restart and is still open. Use Resume in the Tasks tab to continue, or Cancel.", t.TaskID, t.Title), t.TaskID, nil)
		}
	}
}
