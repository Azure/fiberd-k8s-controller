package issuer

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/Azure/fiberd/pkg/grant"
)

// newRouteHandler is a Handler that holds only what route needs.
func newRouteHandler(t *testing.T, limits RateLimits, log logr.Logger) *Handler {
	t.Helper()
	clk := clocktesting.NewFakePassiveClock(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC))
	return NewHandler(Config{Limits: limits, Clock: clk, Log: log})
}

// post is a POST of body from ipA.
func post(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, GrantPath, strings.NewReader(body))
	r.RemoteAddr = ipA + ":40000"
	return r
}

func TestRouteRefusals(t *testing.T) {
	cases := []struct {
		name string
		// request builds the request. Nil posts an empty object.
		request func() *http.Request
		// limits defaults to testLimits.
		limits *RateLimits
		// before takes tokens ahead of the request under test.
		before func(*Handler)
		// next is the route. Nil fails the test if it runs.
		next       func(*http.Request) (any, error)
		wantStatus int
		wantBody   string
		wantHeader map[string]string
	}{
		{
			name: "a GET is a JSON 405",
			request: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, GrantPath, nil)
			},
			wantStatus: http.StatusMethodNotAllowed,
			wantBody:   `{"error":"MethodNotAllowed"}`,
			wantHeader: map[string]string{"Allow": http.MethodPost},
		},
		{
			name: "a declared length over the cap never reaches the route",
			request: func() *http.Request {
				return post(strings.Repeat("a", maxBodyBytes+1))
			},
			wantStatus: http.StatusRequestEntityTooLarge,
			wantBody:   `{"error":"BodyTooLarge"}`,
		},
		{
			name: "a body with no declared length is capped when it is read",
			request: func() *http.Request {
				r := post(strings.Repeat("a", maxBodyBytes+1))
				r.ContentLength = -1
				return r
			},
			next: func(r *http.Request) (any, error) {
				var v struct{}
				return nil, readJSON(r, &v)
			},
			wantStatus: http.StatusRequestEntityTooLarge,
			wantBody:   `{"error":"BodyTooLarge"}`,
		},
		{
			name:    "a body that is not JSON",
			request: func() *http.Request { return post("not json") },
			next: func(r *http.Request) (any, error) {
				var v struct{}
				return nil, readJSON(r, &v)
			},
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error":"BadRequest"}`,
		},
		{
			name:       "an address over its limit is refused before the route",
			limits:     &RateLimits{PerIP: 0.5, Global: 1000, PodInterval: time.Second},
			before:     func(h *Handler) { h.limits.admit(ipA) },
			wantStatus: http.StatusTooManyRequests,
			wantBody:   `{"error":"RateLimited"}`,
			wantHeader: map[string]string{"Retry-After": "2"},
		},
		{
			name:       "the global limit is refused before the route",
			limits:     &RateLimits{PerIP: 1000, Global: 0.5, PodInterval: time.Second},
			before:     func(h *Handler) { h.limits.admit(ipB) },
			wantStatus: http.StatusTooManyRequests,
			wantBody:   `{"error":"RateLimited"}`,
			wantHeader: map[string]string{"Retry-After": "2"},
		},
		{
			name: "a refusal names its reason and hides its cause",
			next: func(*http.Request) (any, error) {
				return nil, refuse(http.StatusForbidden, reasonForbidden, errors.New("namespace tenant-a"))
			},
			wantStatus: http.StatusForbidden,
			wantBody:   `{"error":"Forbidden"}`,
		},
		{
			name: "an unavailable API server gives a Retry-After",
			next: func(*http.Request) (any, error) {
				return nil, unavailable(errors.New("connection refused"))
			},
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   `{"error":"Unavailable"}`,
			wantHeader: map[string]string{"Retry-After": "5"},
		},
		{
			name: "an error that is no refusal hides its text",
			next: func(*http.Request) (any, error) {
				return nil, errors.New("sign with key abc")
			},
			wantStatus: http.StatusInternalServerError,
			wantBody:   `{"error":"MintFailed"}`,
		},
		{
			name: "an answer is JSON",
			next: func(*http.Request) (any, error) {
				return grantAnswer{UID: podA}, nil
			},
			wantStatus: http.StatusOK,
			wantBody:   `{"token":"","uid":"` + podA + `","audience":"","issuedAt":"0001-01-01T00:00:00Z","expiresAt":"0001-01-01T00:00:00Z"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limits := testLimits
			if tc.limits != nil {
				limits = *tc.limits
			}
			h := newRouteHandler(t, limits, logr.Discard())
			if tc.before != nil {
				tc.before(h)
			}
			next := tc.next
			if next == nil {
				next = func(*http.Request) (any, error) {
					t.Fatal("the route ran")
					return nil, nil
				}
			}
			r := post("{}")
			if tc.request != nil {
				r = tc.request()
			}

			w := httptest.NewRecorder()
			h.route(next).ServeHTTP(w, r)

			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			if got := strings.TrimSpace(w.Body.String()); got != tc.wantBody {
				t.Errorf("body = %s, want %s", got, tc.wantBody)
			}
			if got := w.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			if got := w.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			for k, v := range tc.wantHeader {
				if got := w.Header().Get(k); got != v {
					t.Errorf("header %s = %q, want %q", k, got, v)
				}
			}
		})
	}
}

func TestRouteBoundsTheRequest(t *testing.T) {
	h := newRouteHandler(t, testLimits, logr.Discard())
	var left time.Duration
	var hasDeadline bool
	next := func(r *http.Request) (any, error) {
		var deadline time.Time
		deadline, hasDeadline = r.Context().Deadline()
		left = time.Until(deadline)
		return grantAnswer{}, nil
	}

	h.route(next).ServeHTTP(httptest.NewRecorder(), post("{}"))

	if !hasDeadline || left > requestTimeout {
		t.Fatalf("the route's deadline is %v away, has one %t, want one within %v", left, hasDeadline, requestTimeout)
	}
}

func TestRespondErrorLogLevels(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantLog bool
	}{
		{name: "a 401 stays below the default verbosity", err: refuse(http.StatusUnauthorized, reasonUnauthenticated, errors.New("denied"))},
		{name: "a 429 stays below the default verbosity", err: rateLimited(time.Second)},
		{name: "a 403 is logged", err: refuse(http.StatusForbidden, reasonForbidden, errors.New("lineage")), wantLog: true},
		{name: "a 503 is logged", err: unavailable(errors.New("down")), wantLog: true},
		{name: "an error that is no refusal is logged", err: errors.New("bug"), wantLog: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var lines []string
			log := funcr.New(func(prefix, args string) {
				mu.Lock()
				defer mu.Unlock()
				lines = append(lines, args)
			}, funcr.Options{Verbosity: 0})
			h := newRouteHandler(t, testLimits, log)

			h.respondError(httptest.NewRecorder(), post("{}"), tc.err)

			if got := len(lines) > 0; got != tc.wantLog {
				t.Fatalf("logged %q, want a line %t", lines, tc.wantLog)
			}
		})
	}
}

func TestRoutesServesCoreBesideTheGrantRoute(t *testing.T) {
	ring, err := grant.NewKeyRing(jose.EdDSA)
	if err != nil {
		t.Fatalf("NewKeyRing: %v", err)
	}
	clk := clocktesting.NewFakePassiveClock(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC))
	h := NewHandler(Config{
		Issuer: &grant.Issuer{Ring: ring, URL: testIssuerURL},
		Limits: testLimits,
		Clock:  clk,
		Log:    logr.Discard(),
	}).Routes()

	cases := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantError  string
	}{
		{name: "the key set", method: http.MethodGet, path: JWKSPath, wantStatus: http.StatusOK},
		{name: "discovery", method: http.MethodGet, path: DiscoveryPath, wantStatus: http.StatusOK},
		{
			name:       "a GET of the grant route hears the route's own 405",
			method:     http.MethodGet,
			path:       GrantPath,
			wantStatus: http.StatusMethodNotAllowed,
			wantError:  reasonMethodNotAllowed,
		},
		{name: "any other path", method: http.MethodGet, path: "/other", wantStatus: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			if tc.wantError == "" {
				return
			}
			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("the body %q is not JSON: %v", w.Body.String(), err)
			}
			if body.Error != tc.wantError {
				t.Fatalf("error = %q, want %q", body.Error, tc.wantError)
			}
		})
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	cases := []struct {
		wait time.Duration
		want string
	}{
		{wait: time.Nanosecond, want: "1"},
		{wait: 999 * time.Millisecond, want: "1"},
		{wait: time.Second, want: "1"},
		{wait: 1500 * time.Millisecond, want: "2"},
		{wait: 10 * time.Second, want: "10"},
	}
	for _, tc := range cases {
		t.Run(tc.wait.String(), func(t *testing.T) {
			if got := retryAfterSeconds(tc.wait); got != tc.want {
				t.Fatalf("retryAfterSeconds(%v) = %s, want %s", tc.wait, got, tc.want)
			}
		})
	}
}

func TestClientIP(t *testing.T) {
	cases := []struct {
		name   string
		remote string
		header string
		want   string
	}{
		{name: "an IPv4 peer", remote: "10.0.0.1:40000", want: "10.0.0.1"},
		{name: "an IPv6 peer", remote: "[fd00::1]:40000", want: "fd00::1"},
		{name: "a forwarded header is ignored", remote: "10.0.0.1:40000", header: "192.0.2.1", want: "10.0.0.1"},
		{name: "an address with no port is kept", remote: "10.0.0.1", want: "10.0.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := post("{}")
			r.RemoteAddr = tc.remote
			if tc.header != "" {
				r.Header.Set("X-Forwarded-For", tc.header)
			}
			if got := clientIP(r); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestServerLogLevels(t *testing.T) {
	cases := []struct {
		name string
		line string
		// logged says the line shows at the default verbosity.
		logged bool
	}{
		{name: "a refused handshake stays quiet", line: "http: TLS handshake error from 10.0.0.1:1: remote error\n"},
		{name: "any other line is reported", line: "http: panic serving 10.0.0.1:1: boom\n", logged: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var lines []string
			log := funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{Verbosity: 0})

			n, err := serverLog{log: log}.Write([]byte(tc.line))

			if err != nil || n != len(tc.line) {
				t.Fatalf("Write = %d, %v, want %d, nil", n, err, len(tc.line))
			}
			if got := len(lines) > 0; got != tc.logged {
				t.Fatalf("logged %q, want a line %t", lines, tc.logged)
			}
		})
	}
}
