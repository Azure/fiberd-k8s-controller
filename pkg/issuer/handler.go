package issuer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/fiberd/pkg/grant"

	"github.com/Azure/fiberd-k8s-controller/pkg/workload"
)

// GrantPath is where a grant Pod fetches and renews its grant. fiberd core
// appends the same path to its -issuer flag.
const GrantPath = "/v1/grant"

const (
	// maxBodyBytes bounds a request body. A renewal carries one grant, which
	// core caps well below this.
	maxBodyBytes = 16 << 10

	// requestTimeout bounds one request, its body read included. It stays
	// below the timeout of core's fetcher, so the Pod hears the refusal
	// instead of giving up on the connection.
	requestTimeout = 8 * time.Second

	// unavailableRetry is the Retry-After of a 503. The API server gives no
	// better estimate.
	unavailableRetry = 5 * time.Second

	// refusedMessage is the one log message of every refusal, so the reason
	// field is what a query filters on.
	refusedMessage = "Refused a grant request"

	// quietVerbosity hides the refusals a client can cause at will, so a
	// flood of them does not become a flood of log lines.
	quietVerbosity = 4
)

// The reasons a refusal names. A client reads them from the error field of
// the body, and they are all it learns.
const (
	reasonBadRequest       = "BadRequest"
	reasonUnauthenticated  = "Unauthenticated"
	reasonForbidden        = "Forbidden"
	reasonMethodNotAllowed = "MethodNotAllowed"
	reasonPodTerminating   = "PodTerminating"
	reasonGrantDeleting    = "GrantDeleting"
	reasonSuperseded       = "Superseded"
	reasonUnboundGrant     = "UnboundGrant"
	reasonBodyTooLarge     = "BodyTooLarge"
	reasonInvalidSpec      = "InvalidSpec"
	reasonRateLimited      = "RateLimited"
	reasonMintFailed       = "MintFailed"
	reasonUnavailable      = "Unavailable"
)

// errBadBody stands in for a decoding error, whose text can quote the body,
// and the body can hold a grant.
var errBadBody = errors.New("the body is not the expected JSON")

// refusal is a request the handler answers with an error status.
type refusal struct {
	status int
	reason string
	// retryAfter is the Retry-After of the answer, when it is positive.
	retryAfter time.Duration
	// err is the cause, for the log. It never reaches the client.
	err error
}

func (r *refusal) Error() string { return fmt.Sprintf("%s: %v", r.reason, r.err) }

func (r *refusal) Unwrap() error { return r.err }

// refuse returns the refusal with status and reason, caused by err.
func refuse(status int, reason string, err error) error {
	return &refusal{status: status, reason: reason, err: err}
}

// unavailable is the refusal of a request the API server could not serve.
func unavailable(err error) error {
	return &refusal{status: http.StatusServiceUnavailable, reason: reasonUnavailable, retryAfter: unavailableRetry, err: err}
}

// grantAnswer is the body of a grant. Core's fetcher reads the same field
// names.
type grantAnswer struct {
	Token     string    `json:"token"`
	UID       string    `json:"uid"`
	Audience  string    `json:"audience"`
	IssuedAt  time.Time `json:"issuedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Config is what a Handler is built from.
type Config struct {
	// Issuer signs grants. Handler serves its discovery and key set too.
	Issuer *grant.Issuer

	// Reviewer asks the API server who sent a token.
	Reviewer TokenReviewer

	// Cache reads from the informers.
	Cache client.Reader

	// Live reads from the API server. Every replica serves while only the
	// leader reconciles, so a cached object can be older than the Pod that
	// depends on it.
	Live client.Reader

	// Workload traces a Pod back to its CapacityGrant.
	Workload workload.Workload

	// Limits bounds how often the routes answer.
	Limits RateLimits

	// ServiceAccount is the one service account a grant Pod may run as.
	ServiceAccount string

	// Lease is how long a first grant stays valid. A whole number of
	// seconds. A renewal keeps the length of the grant it renews.
	Lease time.Duration

	// Clock must be the clock of Issuer. GrantFromSpec requires its now to
	// be no later than the time the Issuer signs at.
	Clock clock.PassiveClock

	// Log takes every refusal. No value it receives holds a token.
	Log logr.Logger
}

// Handler serves the issuer's HTTPS routes.
type Handler struct {
	issuer         *grant.Issuer
	reviewer       TokenReviewer
	live           client.Reader
	fresh          client.Reader
	workload       workload.Workload
	limits         *limiter
	serviceAccount string
	lease          time.Duration
	clock          clock.PassiveClock
	log            logr.Logger
}

// NewHandler returns the Handler c describes.
func NewHandler(c Config) *Handler {
	return &Handler{
		issuer:         c.Issuer,
		reviewer:       c.Reviewer,
		live:           c.Live,
		fresh:          workload.CacheThenLive(c.Cache, c.Live),
		workload:       c.Workload,
		limits:         newLimiter(c.Limits, c.Clock),
		serviceAccount: c.ServiceAccount,
		lease:          c.Lease,
		clock:          c.Clock,
		log:            c.Log,
	}
}

// Routes serves the grant route beside core's discovery and key set. The
// route is registered without a method, so a wrong method hears a JSON 405
// from route and not the mux's text.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle(GrantPath, h.route(h.serveGrant))
	mux.Handle("/", h.issuer.Handler())
	return mux
}

// route runs what every grant route shares around next, which returns the
// JSON answer or an error. Nothing reads the body before the limits have
// passed, since HTTP/2 multiplexes streams and an unlimited read has no
// bound. A body that declares no length is capped when next reads it.
func (h *Handler) route(next func(r *http.Request) (any, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		r = r.WithContext(ctx)
		// The listener sets only a header timeout, so a slow body needs its
		// own bound. A ResponseWriter without deadlines keeps the context's.
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(requestTimeout))
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

		if err := h.admit(r); err != nil {
			h.respondError(w, r, err)
			return
		}
		answer, err := next(r)
		if err != nil {
			h.respondError(w, r, err)
			return
		}
		respondJSON(w, http.StatusOK, answer)
	})
}

// admit refuses what can be refused without reading the body.
func (h *Handler) admit(r *http.Request) error {
	if r.Method != http.MethodPost {
		return refuse(http.StatusMethodNotAllowed, reasonMethodNotAllowed, fmt.Errorf("method %s", r.Method))
	}
	if r.ContentLength > maxBodyBytes {
		return refuse(http.StatusRequestEntityTooLarge, reasonBodyTooLarge, fmt.Errorf("declared %d bytes", r.ContentLength))
	}
	if wait, ok := h.limits.admit(clientIP(r)); !ok {
		return rateLimited(wait)
	}
	return nil
}

// rateLimited is the refusal of a request a limit stopped.
func rateLimited(wait time.Duration) error {
	return &refusal{status: http.StatusTooManyRequests, reason: reasonRateLimited, retryAfter: wait, err: errors.New("over a rate limit")}
}

// clientIP is the address the connection came from. Forwarded headers are
// ignored, since any client can write them.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// readJSON reads r's body into v. route has capped it.
func readJSON(r *http.Request, v any) error {
	body, err := io.ReadAll(r.Body)
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return refuse(http.StatusRequestEntityTooLarge, reasonBodyTooLarge, err)
	case err != nil:
		return refuse(http.StatusBadRequest, reasonBadRequest, err)
	}
	if json.Unmarshal(body, v) != nil {
		return refuse(http.StatusBadRequest, reasonBadRequest, errBadBody)
	}
	return nil
}

// respondError answers with err's refusal and logs it. An error that is no
// refusal is a bug in a route, so it is the one answer that names nothing
// about the request.
func (h *Handler) respondError(w http.ResponseWriter, r *http.Request, err error) {
	var rf *refusal
	if !errors.As(err, &rf) {
		rf = &refusal{status: http.StatusInternalServerError, reason: reasonMintFailed, err: err}
	}
	log := h.log.WithValues("reason", rf.reason, "status", rf.status, "path", r.URL.Path, "client", clientIP(r))
	switch {
	case rf.status >= http.StatusInternalServerError:
		log.Error(rf.err, refusedMessage)
	case rf.status == http.StatusUnauthorized || rf.status == http.StatusTooManyRequests:
		log.V(quietVerbosity).Info(refusedMessage, "cause", rf.err)
	default:
		log.Info(refusedMessage, "cause", rf.err)
	}

	if rf.retryAfter > 0 {
		w.Header().Set("Retry-After", retryAfterSeconds(rf.retryAfter))
	}
	if rf.status == http.StatusMethodNotAllowed {
		w.Header().Set("Allow", http.MethodPost)
	}
	respondJSON(w, rf.status, struct {
		Error string `json:"error"`
	}{Error: rf.reason})
}

// retryAfterSeconds rounds up, since Retry-After holds whole seconds and a
// client that waits less is refused again.
func retryAfterSeconds(d time.Duration) string {
	return strconv.FormatInt(int64(math.Ceil(d.Seconds())), 10)
}

// respondJSON writes v as the answer. A grant is a credential, so no cache may
// keep an answer. A failed write is a client that went away, so there is
// nobody to tell.
func respondJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
