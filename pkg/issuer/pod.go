package issuer

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/fiberd/pkg/grant"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload/podtemplate"
)

// grantRequest is the body of a grant request. Core's fetcher writes the
// same field.
type grantRequest struct {
	// Previous is the grant the Pod holds, sent on a renewal and after a
	// restart. Empty asks for a first grant.
	Previous string `json:"previous"`
}

// The causes of the refusals the Pod checks end in. A 401 or 403 names the
// failed condition and nothing from the request, since the cause is logged.
var (
	errNoBearer          = errors.New("no bearer token")
	errNoPod             = errors.New("the Pod does not exist")
	errPodUID            = errors.New("the Pod has another UID")
	errPodServiceAccount = errors.New("the Pod runs as another service account")
	errPodTerminating    = errors.New("the Pod is terminating")
	errGrantDeleting     = errors.New("the CapacityGrant is being deleted")
	errGrantGone         = errors.New("the CapacityGrant does not exist")
	errGrantRecreated    = errors.New("the CapacityGrant was recreated")
	errSuperseded        = errors.New("the Pod was built from an older spec")
	errUnboundGrant      = errors.New("the previous grant is bound to no caller")
)

// serveGrant issues a grant to the Pod whose token the request carries, or
// renews the one it holds. The token is reviewed before the body is read,
// so an unauthenticated client costs one TokenReview and nothing more, and
// the per-Pod limit is taken before the API server is asked anything else.
func (h *Handler) serveGrant(r *http.Request) (any, error) {
	ctx := r.Context()
	token, ok := bearerToken(r)
	if !ok {
		return nil, refuse(http.StatusUnauthorized, reasonUnauthenticated, errNoBearer)
	}
	status, err := h.reviewer.Review(ctx, token)
	if err != nil {
		return nil, unavailable(err)
	}
	bound, err := boundPod(status, h.issuer.URL, h.serviceAccount)
	if err != nil {
		return nil, refuse(http.StatusUnauthorized, reasonUnauthenticated, err)
	}
	if wait, ok := h.limits.admitPod(bound.UID); !ok {
		return nil, rateLimited(wait)
	}
	var req grantRequest
	if err := readJSON(r, &req); err != nil {
		return nil, err
	}

	pod, err := h.podOf(ctx, bound)
	if err != nil {
		return nil, err
	}
	cg, err := h.grantOf(ctx, pod)
	if err != nil {
		return nil, err
	}

	// GrantFromSpec requires now to be no later than the time the Issuer
	// signs at, and the Issuer runs on the same clock.
	now := h.clock.Now()
	answer, renewed := h.renew(req.Previous, pod, now)
	// A grant bound to no caller was issued to a Pod that serves plaintext,
	// built before grant Pods served mutual TLS only. The conflict stops its
	// renewals, so the Pod lapses within a lease.
	if renewed && !callerBound(answer.Token) {
		return nil, refuse(http.StatusConflict, reasonUnboundGrant, errUnboundGrant)
	}
	if !renewed {
		if answer, err = h.issue(ctx, cg, pod, now); err != nil {
			return nil, err
		}
	}
	h.log.V(2).Info("Issued a grant", "namespace", pod.Namespace, "pod", pod.Name, "uid", pod.UID, "renewed", renewed)
	return answer, nil
}

// podOf reads the Pod a token is bound to and checks that it is the Pod the
// token describes. A Pod that is gone, one with another UID and one that
// runs as another service account are refused alike, so a token for one
// Pod tells its holder nothing about another.
func (h *Handler) podOf(ctx context.Context, bound BoundPod) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	switch err := h.fresh.Get(ctx, client.ObjectKey{Namespace: bound.Namespace, Name: bound.Name}, pod); {
	case apierrors.IsNotFound(err):
		return nil, forbidden(errNoPod)
	case err != nil:
		return nil, unavailable(err)
	case string(pod.UID) != bound.UID:
		return nil, forbidden(errPodUID)
	case pod.Spec.ServiceAccountName != h.serviceAccount:
		return nil, forbidden(errPodServiceAccount)
	case pod.DeletionTimestamp != nil:
		// The API server honours a deleted Pod's token for a minute more,
		// so the controller has to notice the deletion itself.
		return nil, refuse(http.StatusConflict, reasonPodTerminating, errPodTerminating)
	}
	return pod, nil
}

// grantOf traces pod to its CapacityGrant. A chain that breaks is a Pod the
// controller did not build. A grant on its way out must not hand out a
// lease that outlives it.
func (h *Handler) grantOf(ctx context.Context, pod *corev1.Pod) (*v1alpha1.CapacityGrant, error) {
	cg, err := h.workload.GrantOf(ctx, h.fresh, pod)
	switch {
	case errors.Is(err, workload.ErrNotOwned):
		return nil, forbidden(err)
	case err != nil:
		return nil, unavailable(err)
	case cg.DeletionTimestamp != nil:
		return nil, refuse(http.StatusConflict, reasonGrantDeleting, errGrantDeleting)
	}
	return cg, nil
}

// renew answers with previous renewed, when previous is a grant of pod that
// this issuer signed. Anything else is no renewal, and the caller issues a
// first grant instead, so a Pod that lost or mangled its grant recovers on
// its own. No error leaves here, since any text about previous would be
// logged.
//
// The spec is not read. A superseded Pod keeps the lease it was built with
// until the rollout replaces it, and a bad spec edit does not take a
// running Pod down.
func (h *Handler) renew(previous string, pod *corev1.Pod, now time.Time) (grantAnswer, bool) {
	if previous == "" {
		return grantAnswer{}, false
	}
	// A grant of another Pod is not this Pod's to renew, whoever signed it.
	// Renew checks the signature behind the claim.
	if uid, err := grant.PeekUID(previous); err != nil || uid != string(pod.UID) {
		return grantAnswer{}, false
	}
	token, err := h.issuer.Renew(previous, now)
	if err != nil {
		return grantAnswer{}, false
	}
	expiresAt, err := grant.PeekExpiry(token)
	if err != nil {
		return grantAnswer{}, false
	}
	return newAnswer(token, pod, now, expiresAt), true
}

// callerBound reports whether token, which this issuer has just signed,
// carries a cnf jkt.
func callerBound(token string) bool {
	tok, err := jwt.ParseSigned(token, grant.AllowedAlgorithms)
	if err != nil {
		return false
	}
	var claims struct {
		Cnf struct {
			JKT string `json:"jkt"`
		} `json:"cnf"`
	}
	if err := tok.UnsafeClaimsWithoutVerification(&claims); err != nil {
		return false
	}
	return claims.Cnf.JKT != ""
}

// issue mints a first grant for pod from the spec it was built from.
func (h *Handler) issue(ctx context.Context, cg *v1alpha1.CapacityGrant, pod *corev1.Pod, now time.Time) (grantAnswer, error) {
	cg, err := h.current(ctx, cg, pod)
	if err != nil {
		return grantAnswer{}, err
	}
	g, err := GrantFromSpec(cg, PodIdentity{UID: string(pod.UID), Name: pod.Name}, now, h.lease)
	if err != nil {
		return grantAnswer{}, refuse(http.StatusUnprocessableEntity, reasonInvalidSpec, err)
	}
	token, err := h.issuer.Mint(g)
	if err != nil {
		return grantAnswer{}, refuse(http.StatusInternalServerError, reasonMintFailed, err)
	}
	return newAnswer(token, pod, now, g.LeaseExpiry), nil
}

// current returns the grant whose hash pod carries. A Pod built from an
// earlier spec hears 409 and stops asking while the rollout replaces it.
// Every replica serves this route while only the leader reconciles, so cg
// out of this replica's cache can be older than the Pod, and a 409 ends the
// fetcher's renewal for good. A hash that differs is therefore checked
// against the API server before it is refused.
func (h *Handler) current(ctx context.Context, cg *v1alpha1.CapacityGrant, pod *corev1.Pod) (*v1alpha1.CapacityGrant, error) {
	switch ok, err := builtFrom(pod, cg); {
	case err != nil:
		return nil, err
	case ok:
		return cg, nil
	}
	live := &v1alpha1.CapacityGrant{}
	switch err := h.live.Get(ctx, client.ObjectKeyFromObject(cg), live); {
	case apierrors.IsNotFound(err):
		return nil, forbidden(errGrantGone)
	case err != nil:
		return nil, unavailable(err)
	case live.UID != cg.UID:
		return nil, forbidden(errGrantRecreated)
	case live.DeletionTimestamp != nil:
		return nil, refuse(http.StatusConflict, reasonGrantDeleting, errGrantDeleting)
	}
	switch ok, err := builtFrom(pod, live); {
	case err != nil:
		return nil, err
	case ok:
		return live, nil
	}
	return nil, refuse(http.StatusConflict, reasonSuperseded, errSuperseded)
}

// builtFrom reports whether pod carries the hash of cg, which the reconciler
// stamps on every Pod it builds.
func builtFrom(pod *corev1.Pod, cg *v1alpha1.CapacityGrant) (bool, error) {
	hash, err := podtemplate.Hash(cg, podtemplate.ImageOf(cg))
	if err != nil {
		return false, refuse(http.StatusUnprocessableEntity, reasonInvalidSpec, err)
	}
	return pod.Annotations[podtemplate.AnnotationGrantHash] == hash, nil
}

// forbidden is the refusal of a Pod the token holder may not hold a grant
// for.
func forbidden(err error) error {
	return refuse(http.StatusForbidden, reasonForbidden, err)
}

// newAnswer is the body of a grant for pod. issuedAt is cut to whole
// seconds as the iat claim is, so the two agree.
func newAnswer(token string, pod *corev1.Pod, now, expiresAt time.Time) grantAnswer {
	return grantAnswer{
		Token:     token,
		UID:       string(pod.UID),
		Audience:  pod.Name,
		IssuedAt:  now.Truncate(time.Second),
		ExpiresAt: expiresAt,
	}
}
