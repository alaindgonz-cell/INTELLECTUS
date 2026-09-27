// Package fakecore is an in-process stand-in for `intellectus-core serve`
// used only by unit tests. It speaks the same JSON-Lines framing over
// io.Pipe and answers from per-command handlers. It holds no real state
// machine; tests script exactly the responses they need.
package fakecore

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/alaindgonz-cell/intellectus/engine/coreclient"
)

// Handler answers one command. Returning a non-nil *CoreError produces an
// ok:false transport error.
type Handler func(args json.RawMessage) (any, *coreclient.CoreError)

// Req is one received request.
type Req struct {
	ID   uint64          `json:"id"`
	Cmd  string          `json:"cmd"`
	Args json.RawMessage `json:"args"`
}

// Server is the fake core.
type Server struct {
	mu          sync.Mutex
	handlers    map[string]Handler
	log         []Req
	parseErrors int

	// ReverseBatch > 1 makes the server hold up to that many responses and
	// emit them in reverse order (flushing after a short idle), to prove the
	// client demultiplexes by id rather than by order.
	ReverseBatch int
}

// New returns an empty fake core.
func New() *Server { return &Server{handlers: map[string]Handler{}} }

// Handle registers a handler for cmd.
func (s *Server) Handle(cmd string, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[cmd] = h
}

// Requests returns the received requests for cmd ("" = all).
func (s *Server) Requests(cmd string) []Req {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Req
	for _, r := range s.log {
		if cmd == "" || r.Cmd == cmd {
			out = append(out, r)
		}
	}
	return out
}

// Count returns how many cmd requests were received.
func (s *Server) Count(cmd string) int { return len(s.Requests(cmd)) }

// ParseErrors counts request lines that were not valid JSON requests
// (e.g. interleaved writes).
func (s *Server) ParseErrors() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.parseErrors
}

type response struct {
	ID     uint64                `json:"id"`
	OK     bool                  `json:"ok"`
	Result any                   `json:"result,omitempty"`
	Error  *coreclient.CoreError `json:"error,omitempty"`
}

// Start launches the server and returns a client connected to it.
func (s *Server) Start() *coreclient.Client {
	reqR, reqW := io.Pipe()
	respR, respW := io.Pipe()
	go s.serve(reqR, respW)
	return coreclient.NewClient(respR, reqW)
}

func (s *Server) serve(in *io.PipeReader, out *io.PipeWriter) {
	defer out.Close()
	lines := make(chan []byte)
	go func() {
		defer close(lines)
		br := bufio.NewReader(in)
		for {
			line, err := br.ReadBytes('\n')
			if t := bytes.TrimSpace(line); len(t) > 0 {
				lines <- append([]byte(nil), t...)
			}
			if err != nil {
				return
			}
		}
	}()

	var held [][]byte
	flush := func() {
		for i := len(held) - 1; i >= 0; i-- {
			if _, err := out.Write(held[i]); err != nil {
				return
			}
		}
		held = held[:0]
	}
	for {
		var line []byte
		var ok bool
		if len(held) > 0 {
			select {
			case line, ok = <-lines:
			case <-time.After(3 * time.Millisecond):
				flush()
				continue
			}
		} else {
			line, ok = <-lines
		}
		if !ok {
			flush()
			return
		}
		resp := s.handle(line)
		if resp == nil {
			continue
		}
		b, _ := json.Marshal(resp)
		b = append(b, '\n')
		if s.ReverseBatch > 1 {
			held = append(held, b)
			if len(held) >= s.ReverseBatch {
				flush()
			}
			continue
		}
		if _, err := out.Write(b); err != nil {
			return
		}
	}
}

func (s *Server) handle(line []byte) *response {
	var r Req
	if err := json.Unmarshal(line, &r); err != nil || r.Cmd == "" {
		s.mu.Lock()
		s.parseErrors++
		s.mu.Unlock()
		return nil
	}
	s.mu.Lock()
	s.log = append(s.log, r)
	h := s.handlers[r.Cmd]
	s.mu.Unlock()
	if h == nil {
		return &response{ID: r.ID, Error: &coreclient.CoreError{Code: "UNKNOWN_COMMAND", Message: r.Cmd}}
	}
	res, cerr := h(r.Args)
	if cerr != nil {
		return &response{ID: r.ID, Error: cerr}
	}
	if res == nil {
		res = map[string]any{}
	}
	return &response{ID: r.ID, OK: true, Result: res}
}

// Arg decodes one field of a request's args (test helper).
func Arg[T any](args json.RawMessage, field string) T {
	var m map[string]json.RawMessage
	var v T
	if json.Unmarshal(args, &m) == nil {
		_ = json.Unmarshal(m[field], &v)
	}
	return v
}
