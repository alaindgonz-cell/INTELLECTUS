package harness

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event is one server-sent event for the UI (docs/UI_API.md).
type Event struct {
	Name string
	Data any
}

// Bus fans events out to UI subscribers. Publishing never blocks: a
// subscriber that falls behind is disconnected (its channel is closed) and
// the UI reconnects and refetches.
type Bus struct {
	mu   sync.Mutex
	next int
	subs map[int]chan Event
}

// NewBus returns an empty bus.
func NewBus() *Bus { return &Bus{subs: map[int]chan Event{}} }

// Subscribe returns an event channel and a cancel function.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	ch := make(chan Event, 512)
	b.subs[id] = ch
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if c, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(c)
		}
	}
}

// Publish delivers ev to every subscriber that keeps up.
func (b *Bus) Publish(name string, data any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, ch := range b.subs {
		select {
		case ch <- Event{Name: name, Data: data}:
		default:
			delete(b.subs, id)
			close(ch)
		}
	}
}

// ActivityItem is one "what INTELLECTUS is doing" entry. An item published
// again with the same ID replaces the earlier one.
type ActivityItem struct {
	ID     string         `json:"id"`
	TS     int64          `json:"ts"`
	TaskID string         `json:"task_id,omitempty"`
	Kind   string         `json:"kind"`   // model|sandbox|core|jev|approval|gateway|system|error
	Status string         `json:"status"` // running|ok|fail|warn|info
	Title  string         `json:"title"`
	Detail string         `json:"detail,omitempty"`
	Data   map[string]any `json:"data,omitempty"`
}

// activityLog keeps the most recent items in memory (the authoritative
// history is the core's event log).
type activityLog struct {
	mu    sync.Mutex
	next  int
	order []string
	items map[string]ActivityItem
	max   int
}

func newActivityLog(max int) *activityLog {
	return &activityLog{items: map[string]ActivityItem{}, max: max}
}

func (a *activityLog) newID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.next++
	return fmt.Sprintf("act-%d", a.next)
}

func (a *activityLog) put(it ActivityItem) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.items[it.ID]; !ok {
		a.order = append(a.order, it.ID)
		if len(a.order) > a.max {
			drop := a.order[0]
			a.order = a.order[1:]
			delete(a.items, drop)
		}
	}
	a.items[it.ID] = it
}

func (a *activityLog) get(id string) (ActivityItem, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	it, ok := a.items[id]
	return it, ok
}

func (a *activityLog) list(limit int) []ActivityItem {
	a.mu.Lock()
	defer a.mu.Unlock()
	start := 0
	if limit > 0 && len(a.order) > limit {
		start = len(a.order) - limit
	}
	out := make([]ActivityItem, 0, len(a.order)-start)
	for _, id := range a.order[start:] {
		out = append(out, a.items[id])
	}
	return out
}

// Message is one chat message.
type Message struct {
	ID     string `json:"id"`
	Role   string `json:"role"` // user|assistant|system
	TS     int64  `json:"ts"`
	Text   string `json:"text"`
	TaskID string `json:"task_id,omitempty"`
	Card   *Card  `json:"card,omitempty"`
}

// Card is a structured chat attachment (docs/UI_API.md). Only the fields
// of its Type are set.
type Card struct {
	Type string `json:"type"` // proposal|approval|report
	// proposal
	ProposalID  string     `json:"proposal_id,omitempty"`
	Title       string     `json:"title,omitempty"`
	Requirement string     `json:"requirement,omitempty"`
	Entrypoint  *EntrySpec `json:"entrypoint,omitempty"`
	Cases       []CaseView `json:"cases,omitempty"`
	Status      string     `json:"status,omitempty"`
	TaskID      *string    `json:"task_id,omitempty"`
	// approval
	ApprovalID   string      `json:"approval_id,omitempty"`
	ToolID       string      `json:"tool_id,omitempty"`
	ActionDigest string      `json:"action_digest,omitempty"`
	FromRoot     string      `json:"from_root,omitempty"`
	ToRoot       string      `json:"to_root,omitempty"`
	Checks       []CheckView `json:"checks,omitempty"`
	Summary      string      `json:"summary,omitempty"`
	// report
	ApprovedRoot string   `json:"approved_root,omitempty"`
	Lines        []string `json:"lines,omitempty"`
	Limitations  []string `json:"limitations,omitempty"`
}

// EntrySpec is a manifest entrypoint.
type EntrySpec struct {
	Language string `json:"language"`
	Path     string `json:"path"`
	Function string `json:"function"`
}

// CaseView is an acceptance case as shown and as registered.
type CaseView struct {
	Name   string          `json:"name"`
	Input  json.RawMessage `json:"input"`
	Expect json.RawMessage `json:"expect"`
}

// CheckView summarizes one check on an approval card.
type CheckView struct {
	Kind   string `json:"kind"`
	Result string `json:"result"`
	Issuer string `json:"issuer"`
	Detail string `json:"detail"`
}

// chatStore persists chat messages as JSON lines (last write per id wins).
type chatStore struct {
	mu    sync.Mutex
	path  string
	next  int
	order []string
	msgs  map[string]Message
}

func openChatStore(path string) (*chatStore, error) {
	c := &chatStore{path: path, msgs: map[string]Message{}}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, os.MkdirAll(filepath.Dir(path), 0o700)
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var m Message
		if json.Unmarshal(sc.Bytes(), &m) != nil || m.ID == "" {
			continue // a torn final line after a crash is skipped
		}
		if _, ok := c.msgs[m.ID]; !ok {
			c.order = append(c.order, m.ID)
		}
		c.msgs[m.ID] = m
		var n int
		if _, err := fmt.Sscanf(m.ID, "m-%d", &n); err == nil && n > c.next {
			c.next = n
		}
	}
	return c, sc.Err()
}

func (c *chatStore) newID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	return fmt.Sprintf("m-%d", c.next)
}

func (c *chatStore) put(m Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.msgs[m.ID]; !ok {
		c.order = append(c.order, m.ID)
	}
	c.msgs[m.ID] = m
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(c.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

func (c *chatStore) get(id string) (Message, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.msgs[id]
	return m, ok
}

func (c *chatStore) list() []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Message, 0, len(c.order))
	for _, id := range c.order {
		out = append(out, c.msgs[id])
	}
	return out
}

func nowMS(t time.Time) int64 { return t.UnixMilli() }
