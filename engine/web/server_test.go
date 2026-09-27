package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These tests exercise the authentication boundary only; they never reach a
// handler that needs the harness.

func newTestServer() http.Handler {
	return (&Server{Token: "secret-token"}).Handler()
}

func do(h http.Handler, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(`{"text":"hi"}`))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestLoginSetsStrictHttpOnlyCookie(t *testing.T) {
	h := newTestServer()
	rec := do(h, "GET", "/?token=secret-token", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d", rec.Code)
	}
	c := rec.Result().Cookies()
	if len(c) != 1 || c[0].Name != cookieName || !c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie %+v", c)
	}
	if rec := do(h, "GET", "/?token=wrong", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token accepted: %d", rec.Code)
	}
}

func TestUnauthenticatedRequestsAreRefused(t *testing.T) {
	h := newTestServer()
	for _, p := range []string{"/", "/app.js", "/api/status", "/api/chat", "/api/stream", "/api/tree"} {
		if rec := do(h, "GET", p, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without session: %d", p, rec.Code)
		}
	}
	if rec := do(h, "GET", "/api/status", map[string]string{"Cookie": cookieName + "=nope"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad cookie accepted: %d", rec.Code)
	}
}

func TestMutationsRequireCSRFHeaderAndSameOrigin(t *testing.T) {
	h := newTestServer()
	cookie := cookieName + "=secret-token"
	for _, p := range []string{"/api/chat", "/api/proposals/p-1/approve", "/api/approvals/a-1/approve", "/api/policy/jev-mode", "/api/export"} {
		if rec := do(h, "POST", p, map[string]string{"Cookie": cookie}); rec.Code != http.StatusForbidden {
			t.Errorf("POST %s without CSRF header: %d", p, rec.Code)
		}
		rec := do(h, "POST", p, map[string]string{"Cookie": cookie, csrfHeader: "1", "Origin": "https://evil.example"})
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s cross-origin: %d", p, rec.Code)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	rec := do(newTestServer(), "GET", "/", nil)
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "script-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q: %s", want, csp)
		}
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Error("missing security headers")
	}
}
