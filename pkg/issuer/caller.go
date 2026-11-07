package issuer

import (
	"cmp"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/fiberd/pkg/tlsconf"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload/podtemplate"
)

// CallerGrantPath is where a tenant's caller fetches the grants it presents
// to the Pods of a CapacityGrant, over the client certificate whose key the
// CapacityGrant names.
const CallerGrantPath = "/v1/caller-grants"

// maxCallerGrants bounds the grants of one answer. A grant runs one Pod,
// and two during a rollout, so only a pile-up of stuck rollouts reaches it.
// The newest Pods are kept, since the oldest are the ones on their way out.
const maxCallerGrants = 8

// callerGrantRequest names the CapacityGrant a caller asks for. Audience,
// when set, names the one Pod whose grant the caller wants.
type callerGrantRequest struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Audience  string `json:"audience,omitempty"`
}

// callerGrant is the grant of one Pod and the address the caller dials it
// at.
type callerGrant struct {
	grantAnswer
	Address string `json:"address"`
}

// callerGrantsAnswer is the body of a caller's grants. ServerName is the
// name every Pod's certificate carries, which the caller verifies whatever
// address it dials. Grants is empty, never null, when no Pod runs.
type callerGrantsAnswer struct {
	ServerName string        `json:"serverName"`
	Grants     []callerGrant `json:"grants"`
}

// The causes of the caller route's refusals.
var (
	errNoClientCert   = errors.New("no client certificate")
	errNoCallerKey    = errors.New("the client certificate's key has no thumbprint")
	errNoClientCA     = errors.New("the issuer has no client CA")
	errNoGrantName    = errors.New("the request names no valid CapacityGrant")
	errCallerNotNamed = errors.New("the CapacityGrant names another caller")
)

// serveCallerGrants gives a caller one grant for each running Pod of the
// CapacityGrant that names its key. route has taken the per-address and
// global limits, so a stranger is stopped before its certificate costs a
// chain check, and they bound every refusal after it. The per-caller limit
// is taken once the grant names the caller, and counts each grant and
// client address on its own, since one key may be named by many grants
// and shared by many replicas. A grant that does not exist and
// one that names another key are refused alike, and both are read from the
// cache alone, so neither the answer nor its latency tells a caller about a
// grant that is not its own.
func (h *Handler) serveCallerGrants(r *http.Request) (any, error) {
	ctx := r.Context()
	caller, err := h.callerOf(r)
	if err != nil {
		return nil, refuse(http.StatusUnauthorized, reasonUnauthenticated, err)
	}
	var req callerGrantRequest
	if err := readJSON(r, &req); err != nil {
		return nil, err
	}
	if len(validation.IsDNS1123Label(req.Namespace)) > 0 || len(validation.IsDNS1123Subdomain(req.Name)) > 0 {
		return nil, refuse(http.StatusBadRequest, reasonBadRequest, errNoGrantName)
	}

	key := client.ObjectKey{Namespace: req.Namespace, Name: req.Name}
	cg := &v1alpha1.CapacityGrant{}
	switch err := h.cache.Get(ctx, key, cg); {
	case apierrors.IsNotFound(err):
		return nil, forbidden(errGrantGone)
	case err != nil:
		return nil, unavailable(err)
	case cg.Spec.CallerThumbprint != caller:
		return nil, forbidden(errCallerNotNamed)
	}
	if wait, ok := h.limits.admitCaller(caller, key.String(), clientIP(r)); !ok {
		return nil, rateLimited(wait)
	}
	if cg.DeletionTimestamp != nil {
		return nil, refuse(http.StatusConflict, reasonGrantDeleting, errGrantDeleting)
	}
	// GrantFromSpec requires now to be no later than the time the Issuer
	// signs at, and the Issuer runs on the same clock.
	now := h.clock.Now()
	// Checked before the Pods, so a grant with none still hears that its
	// spec cannot be served.
	if err := ValidateSpec(cg, now, h.lease); err != nil {
		return nil, refuse(http.StatusUnprocessableEntity, reasonInvalidSpec, err)
	}
	pods, err := h.runningPods(ctx, cg, req.Audience)
	if err != nil {
		return nil, err
	}
	exception, err := h.holdsException(ctx, cg)
	if err != nil {
		return nil, err
	}

	answer := callerGrantsAnswer{
		ServerName: podtemplate.ServiceHost(workload.ObjectName(cg), cg.Namespace),
		Grants:     make([]callerGrant, 0, len(pods)),
	}
	for _, pod := range pods {
		g, err := h.callerGrant(cg, pod, exception, now)
		if err != nil {
			return nil, err
		}
		answer.Grants = append(answer.Grants, g)
	}
	h.log.V(2).Info("Issued caller grants", "namespace", cg.Namespace, "capacityGrant", cg.Name, "grants", len(answer.Grants))
	return answer, nil
}

// callerOf verifies the client certificate of r against the fiberd CA and
// returns the RFC 7638 thumbprint of its key, as fiberd computes it on
// Clone. The listener only asks for the certificate, so the Pods' route,
// discovery and the key set serve a client without one. fiberd-ca signs
// leaves directly, so the leaf is verified against the CA alone and any
// other certificate the client sends is ignored. The check then costs the
// same whatever the client sends.
func (h *Handler) callerOf(r *http.Request) (string, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return "", errNoClientCert
	}
	// A nil pool would make Verify trust the system roots.
	if h.clientCAs == nil {
		return "", errNoClientCA
	}
	chains, err := r.TLS.PeerCertificates[0].Verify(x509.VerifyOptions{
		Roots:     h.clientCAs,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		return "", fmt.Errorf("the client certificate: %w", err)
	}
	c, ok := tlsconf.CallerOf(chains[0])
	if !ok {
		return "", errNoCallerKey
	}
	return c.Thumbprint, nil
}

// runningPods returns the Pods of cg a caller can Clone from, read from the
// cache, newest first and at most maxCallerGrants of them. A Pod of an
// older spec is one, since it serves until the rollout replaces it. A
// terminating Pod is not, and neither is a Pod that does not run yet or
// that cg does not own, checked by the same walk as a Pod's own request. A
// set audience keeps only the Pod of that name.
func (h *Handler) runningPods(ctx context.Context, cg *v1alpha1.CapacityGrant, audience string) ([]*corev1.Pod, error) {
	list := &corev1.PodList{}
	if err := h.cache.List(ctx, list, client.InNamespace(cg.Namespace), client.MatchingLabels(workload.SelectorLabels(cg))); err != nil {
		return nil, unavailable(err)
	}
	pods := make([]*corev1.Pod, 0, len(list.Items))
	for i := range list.Items {
		pod := &list.Items[i]
		if (audience != "" && pod.Name != audience) || pod.DeletionTimestamp != nil ||
			pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" ||
			pod.Spec.ServiceAccountName != h.serviceAccount {
			continue
		}
		pods = append(pods, pod)
	}
	slices.SortFunc(pods, func(a, b *corev1.Pod) int {
		if c := b.CreationTimestamp.Compare(a.CreationTimestamp.Time); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})

	owned := pods[:0]
	for _, pod := range pods {
		if len(owned) == maxCallerGrants {
			break
		}
		owner, err := h.workload.GrantOf(ctx, h.cache, pod)
		switch {
		case errors.Is(err, workload.ErrNotOwned):
			continue
		case err != nil:
			return nil, unavailable(err)
		case owner.UID != cg.UID:
			continue
		}
		owned = append(owned, pod)
	}
	return owned, nil
}

// callerGrant mints the grant of pod from the current spec of cg, with the
// same grant UID and audience as the Pod's own. fiberd keeps each Pod on
// the grant it was built from and runs a Clone under it, so a Pod of an
// older spec accepts this grant too, unless it names another caller. A
// callerThumbprint change therefore moves callers only to new Pods. It
// rolls the Deployment, and an old Pod refuses the grant minted here for
// the new key until the rollout replaces it. exception is what
// holdsException said of cg.
func (h *Handler) callerGrant(cg *v1alpha1.CapacityGrant, pod *corev1.Pod, exception bool, now time.Time) (callerGrant, error) {
	g, err := GrantFromSpec(cg, PodIdentity{UID: string(pod.UID), Name: pod.Name}, exception, now, h.lease)
	if err != nil {
		return callerGrant{}, refuse(http.StatusUnprocessableEntity, reasonInvalidSpec, err)
	}
	token, err := h.issuer.Mint(g)
	if err != nil {
		return callerGrant{}, refuse(http.StatusInternalServerError, reasonMintFailed, err)
	}
	return callerGrant{
		grantAnswer: newAnswer(token, pod, now, g.LeaseExpiry),
		Address:     net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(int(podtemplate.PortGRPC))),
	}, nil
}
