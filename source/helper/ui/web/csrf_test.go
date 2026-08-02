package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func req(method string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(method, "http://127.0.0.1:2202/api/x", nil)
	r.Host = "127.0.0.1:2202"
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestCrossSiteRequest(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{"same-origin fetch", map[string]string{"Sec-Fetch-Site": "same-origin"}, false},
		{"user navigation (none)", map[string]string{"Sec-Fetch-Site": "none"}, false},
		{"cross-site fetch", map[string]string{"Sec-Fetch-Site": "cross-site"}, true},
		{"same-site fetch", map[string]string{"Sec-Fetch-Site": "same-site"}, true},
		{"non-browser (no headers)", nil, false},
		{"origin fallback same host", map[string]string{"Origin": "http://127.0.0.1:2202"}, false},
		{"origin fallback other host", map[string]string{"Origin": "http://evil.com"}, true},
		{"sec-fetch wins over origin", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://evil.com"}, false},
	}
	for _, c := range cases {
		if got := crossSiteRequest(req(http.MethodPost, c.headers)); got != c.want {
			t.Errorf("%s: crossSiteRequest = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestGuardCSRF(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := guardCSRF(next)

	// Cross-site POST is blocked.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req(http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site"}))
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site POST: code %d, want 403", rec.Code)
	}

	// Cross-site GET passes (read-only, response unreadable cross-origin).
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req(http.MethodGet, map[string]string{"Sec-Fetch-Site": "cross-site"}))
	if rec.Code != http.StatusOK {
		t.Errorf("cross-site GET: code %d, want 200", rec.Code)
	}

	// Same-origin POST passes.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req(http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-origin"}))
	if rec.Code != http.StatusOK {
		t.Errorf("same-origin POST: code %d, want 200", rec.Code)
	}

	// Non-browser POST (CLI/tui, no headers) passes.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req(http.MethodPost, nil))
	if rec.Code != http.StatusOK {
		t.Errorf("non-browser POST: code %d, want 200", rec.Code)
	}
}
