package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/alaindgonz-cell/intellectus/engine/harness"
)

const (
	cookieName = "intellectus_session"
	csrfHeader = "X-Intellectus-CSRF"
)

// Server is the operator UI and its HTTP API (docs/UI_API.md).
type Server struct {
	H     *harness.Harness
	Token string
	Logf  func(format string, args ...any)
}

// NewToken returns a random launch token.
func NewToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Handler builds the HTTP handler.
func (s *Server) Handler() http.Handler {
	if s.Logf == nil {
		s.Logf = func(string, ...any) {}
	}
	mux := http.NewServeMux()
	static, _ := fs.Sub(Static, "static")
	files := http.FileServer(http.FS(static))

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if tok := r.URL.Query().Get("token"); tok != "" {
			if !s.tokenOK(tok) {
				http.Error(w, "invalid token", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.Token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		if !s.authed(r) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, "INTELLECTUS: open the URL printed by `intellectus serve` (it carries the session token).\n")
			return
		}
		files.ServeHTTP(w, r)
	})
	for _, name := range []string{"app.js", "style.css", "favicon.svg"} {
		mux.Handle("GET /"+name, s.requireAuth(files))
	}

	api := func(pattern string, fn func(w http.ResponseWriter, r *http.Request) (any, error)) {
		mux.Handle(pattern, s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && !s.csrfOK(r) {
				writeErr(w, http.StatusForbidden, errors.New("missing CSRF header or cross-origin request"))
				return
			}
			v, err := fn(w, r)
			if err != nil {
				code := http.StatusBadRequest
				if errors.Is(err, harness.ErrBusy) {
					code = http.StatusConflict
				}
				writeErr(w, code, err)
				return
			}
			writeJSON(w, v)
		})))
	}
	ctx := func(r *http.Request) context.Context { return r.Context() }

	api("GET /api/status", func(w http.ResponseWriter, r *http.Request) (any, error) { return s.H.Status(ctx(r)) })
	api("GET /api/chat", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return map[string]any{"messages": nonNil(s.H.Chat())}, nil
	})
	api("POST /api/chat", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct {
			Text string `json:"text"`
		}
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		id, err := s.H.SendChat(in.Text)
		return map[string]any{"message_id": id}, err
	})
	api("POST /api/proposals/{id}/approve", func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := s.H.ApproveProposal(ctx(r), r.PathValue("id"))
		return map[string]any{"task_id": id}, err
	})
	api("POST /api/proposals/{id}/discard", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return map[string]any{}, s.H.DiscardProposal(r.PathValue("id"))
	})
	api("POST /api/approvals/{id}/approve", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return map[string]any{}, s.H.ApproveAction(ctx(r), r.PathValue("id"))
	})
	api("POST /api/approvals/{id}/reject", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return map[string]any{}, s.H.RejectAction(r.PathValue("id"))
	})
	api("GET /api/activity", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return map[string]any{"items": nonNil(s.H.Activity(intParam(r, "limit", 300)))}, nil
	})
	api("GET /api/tasks", func(w http.ResponseWriter, r *http.Request) (any, error) {
		ts, err := s.H.Tasks(ctx(r))
		return map[string]any{"tasks": nonNil(ts)}, err
	})
	api("GET /api/tasks/{id}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return s.H.TaskDetail(ctx(r), r.PathValue("id"))
	})
	api("POST /api/tasks/{id}/cancel", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return map[string]any{}, s.H.CancelTask(ctx(r), r.PathValue("id"))
	})
	api("POST /api/tasks/{id}/resume", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return map[string]any{}, s.H.ResumeTask(ctx(r), r.PathValue("id"))
	})
	api("GET /api/tree", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return s.H.Tree(ctx(r), r.URL.Query().Get("root"))
	})
	api("GET /api/diff", func(w http.ResponseWriter, r *http.Request) (any, error) {
		q := r.URL.Query()
		return s.H.Diff(ctx(r), q.Get("from"), q.Get("to"))
	})
	api("GET /api/events", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return s.H.Events(ctx(r), int64(intParam(r, "after", 0)), int64(intParam(r, "limit", 100)))
	})
	api("POST /api/export", func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := s.H.ProposeExport(ctx(r))
		return map[string]any{"approval_id": id}, err
	})
	api("POST /api/policy/jev-mode", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct {
			Mode string `json:"mode"`
		}
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		return map[string]any{}, s.H.SetJevMode(ctx(r), in.Mode)
	})
	api("POST /api/policy/trust-sandbox", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return map[string]any{}, s.H.TrustSandbox(ctx(r))
	})
	mux.Handle("GET /api/stream", s.requireAuth(http.HandlerFunc(s.stream)))
	return securityHeaders(mux)
}

func (s *Server) tokenOK(t string) bool {
	return subtle.ConstantTimeCompare([]byte(t), []byte(s.Token)) == 1
}

func (s *Server) authed(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	return err == nil && s.tokenOK(c.Value)
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authed(r) {
			writeErr(w, http.StatusUnauthorized, errors.New("session expired: reopen the URL printed by `intellectus serve`"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// csrfOK requires the custom header (forces a CORS preflight that is never
// granted) and, when present, a same-origin Origin header.
func (s *Server) csrfOK(r *http.Request) bool {
	if r.Header.Get(csrfHeader) != "1" {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || u.Host != r.Host {
			return false
		}
	}
	return true
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")
	ch, cancel := s.H.Bus().Subscribe()
	defer cancel()
	send := func(name string, data any) bool {
		b, err := json.Marshal(data)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if st, err := s.H.Status(r.Context()); err == nil && !send("status", st) {
		return
	}
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return // fell behind: the client reconnects and refetches
			}
			if !send(ev.Name, ev.Data) {
				return
			}
		case <-ping.C:
			if !send("ping", map[string]any{}) {
				return
			}
		}
	}
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("bad request body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
}

func intParam(r *http.Request, name string, def int) int {
	v, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get(name)))
	if err != nil || v < 0 {
		return def
	}
	return v
}

func nonNil[T any](xs []T) []T {
	if xs == nil {
		return []T{}
	}
	return xs
}
