// Package coreclient is the engine's only channel to the Rust core
// (intellectus-core serve). It speaks PROTOCOL.md §2 JSON Lines over the
// core process's stdin/stdout.
//
// Invariants:
//   - All writes to the pipe are serialized by one mutex, so concurrent
//     goroutines never interleave bytes of two requests.
//   - Responses are demultiplexed by id. The core answers in arrival order,
//     but the client never relies on that.
//   - A cancelled context prevents a command from being SENT. Once a command
//     has been written, the client waits for the core's answer regardless of
//     the context: the core will act on it, so its result must not be lost.
//   - Any framing error, unknown response id or closed pipe fails every
//     pending call closed (ErrClosed / the underlying error).
//
// The engine holds no database handle and computes no digest the core
// relies on; every authoritative change goes through Call.
package coreclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// ErrClosed is returned for calls on a closed or failed connection.
var ErrClosed = errors.New("coreclient: connection closed")

// CoreError is a transport-level refusal (`ok:false`): the command was
// malformed or refused before any decision was taken.
type CoreError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *CoreError) Error() string { return fmt.Sprintf("core error %s: %s", e.Code, e.Message) }

// IsCoreError reports whether err is a transport refusal with the given code.
func IsCoreError(err error, code string) bool {
	var ce *CoreError
	return errors.As(err, &ce) && ce.Code == code
}

type request struct {
	ID   uint64 `json:"id"`
	Cmd  string `json:"cmd"`
	Args any    `json:"args"`
}

type response struct {
	ID     *uint64         `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *CoreError      `json:"error"`
}

// TraceFunc observes every line on the wire ("->" sent, "<-" received).
type TraceFunc func(direction string, line []byte)

// Client is a JSON-Lines client for the core. It is safe for concurrent use.
type Client struct {
	w      io.Writer
	closer io.Closer

	wmu    sync.Mutex // serializes writes to the pipe
	nextID atomic.Uint64

	pmu      sync.Mutex
	pending  map[uint64]chan response
	closed   bool
	closeErr error
	done     chan struct{}

	trace atomic.Pointer[TraceFunc]
}

// NewClient starts a client reading responses from r and writing requests
// to w. w is closed by Close (closing the core's stdin asks it to exit).
func NewClient(r io.Reader, w io.WriteCloser) *Client {
	c := &Client{
		w:       w,
		closer:  w,
		pending: make(map[uint64]chan response),
		done:    make(chan struct{}),
	}
	go c.readLoop(r)
	return c
}

// SetTrace installs (or with nil removes) a wire tracer.
func (c *Client) SetTrace(f TraceFunc) {
	if f == nil {
		c.trace.Store(nil)
		return
	}
	c.trace.Store(&f)
}

func (c *Client) traceLine(dir string, line []byte) {
	if p := c.trace.Load(); p != nil {
		(*p)(dir, bytes.TrimRight(line, "\n"))
	}
}

// Done is closed when the connection has terminated.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err returns the reason the connection terminated (nil while open).
func (c *Client) Err() error {
	c.pmu.Lock()
	defer c.pmu.Unlock()
	return c.closeErr
}

func (c *Client) readLoop(r io.Reader) {
	br := bufio.NewReaderSize(r, 1<<16)
	for {
		line, err := br.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			c.traceLine("<-", trimmed)
			var resp response
			if uerr := json.Unmarshal(trimmed, &resp); uerr != nil {
				c.fail(fmt.Errorf("coreclient: malformed response line: %w", uerr))
				return
			}
			if resp.ID == nil {
				c.fail(fmt.Errorf("coreclient: response without id: %s", truncate(trimmed)))
				return
			}
			c.pmu.Lock()
			ch, ok := c.pending[*resp.ID]
			delete(c.pending, *resp.ID)
			c.pmu.Unlock()
			if !ok {
				// Desynchronised stream: fail closed rather than guess.
				c.fail(fmt.Errorf("coreclient: response for unknown id %d", *resp.ID))
				return
			}
			ch <- resp // buffered(1), never blocks
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				c.fail(ErrClosed)
			} else {
				c.fail(fmt.Errorf("coreclient: read: %w", err))
			}
			return
		}
	}
}

// fail terminates the connection and wakes every pending caller.
func (c *Client) fail(err error) {
	c.pmu.Lock()
	if c.closed {
		c.pmu.Unlock()
		return
	}
	c.closed = true
	c.closeErr = err
	pending := c.pending
	c.pending = map[uint64]chan response{}
	c.pmu.Unlock()
	for _, ch := range pending {
		close(ch)
	}
	close(c.done)
}

// Close closes the write side (the core sees EOF on stdin) and fails any
// pending calls once the read side terminates.
func (c *Client) Close() error {
	c.wmu.Lock()
	err := c.closer.Close()
	c.wmu.Unlock()
	return err
}

// CallRaw sends one command and returns its raw result object.
func (c *Client) CallRaw(ctx context.Context, cmd string, args any) (json.RawMessage, error) {
	if args == nil {
		args = struct{}{}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id := c.nextID.Add(1)
	line, err := marshalCompact(request{ID: id, Cmd: cmd, Args: args})
	if err != nil {
		return nil, fmt.Errorf("coreclient: encode %s: %w", cmd, err)
	}
	line = append(line, '\n')

	ch := make(chan response, 1)
	c.pmu.Lock()
	if c.closed {
		cerr := c.closeErr
		c.pmu.Unlock()
		return nil, cerr
	}
	c.pending[id] = ch
	c.pmu.Unlock()

	c.wmu.Lock()
	// Re-check under the write lock: a cancellation observed here stops the
	// command from ever reaching the core.
	if err := ctx.Err(); err != nil {
		c.wmu.Unlock()
		c.forget(id)
		return nil, err
	}
	c.traceLine("->", line)
	_, werr := c.w.Write(line)
	c.wmu.Unlock()
	if werr != nil {
		c.forget(id)
		c.fail(fmt.Errorf("coreclient: write: %w", werr))
		return nil, fmt.Errorf("coreclient: write %s: %w", cmd, werr)
	}

	// Sent: the core will act on it. Wait for the answer (or connection loss)
	// even if ctx is cancelled meanwhile, so an effect is never unaccounted.
	resp, ok := <-ch
	if !ok {
		if e := c.Err(); e != nil {
			return nil, fmt.Errorf("%s: %w", cmd, e)
		}
		return nil, ErrClosed
	}
	if !resp.OK {
		if resp.Error == nil {
			return nil, &CoreError{Code: "UNKNOWN", Message: "ok:false without error object"}
		}
		return nil, resp.Error
	}
	if len(resp.Result) == 0 {
		return json.RawMessage("null"), nil
	}
	return resp.Result, nil
}

func (c *Client) forget(id uint64) {
	c.pmu.Lock()
	delete(c.pending, id)
	c.pmu.Unlock()
}

// Call sends a command and decodes its result into out (if non-nil).
func (c *Client) Call(ctx context.Context, cmd string, args any, out any) error {
	raw, err := c.CallRaw(ctx, cmd, args)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("coreclient: decode %s result: %w", cmd, err)
	}
	return nil
}

func truncate(b []byte) string {
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}

// marshalCompact encodes v as compact JSON without HTML escaping (so model
// text and operator bodies travel byte-for-byte as written).
func marshalCompact(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
