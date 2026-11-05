package issuer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// maxAuthorizationHeader caps the header before it is split. The
	// listener allows a 1 MiB header, and an oversized one must not reach
	// the API server.
	maxAuthorizationHeader = 8 << 10

	// tokenReviewTimeout bounds one TokenReview call, so a slow API server
	// cannot hold a grant request open for as long as the client waits.
	tokenReviewTimeout = 5 * time.Second

	// podNameExtra and podUIDExtra are the user extras the API server sets
	// only on a token bound to a Pod. They are spelled out here because
	// k8s.io/apiserver is not worth adding as a dependency for two strings.
	podNameExtra = "authentication.kubernetes.io/pod-name"
	podUIDExtra  = "authentication.kubernetes.io/pod-uid"

	serviceAccountUsernamePrefix = "system:serviceaccount:"
)

// ErrNotPodToken means a reviewed token is not an accepted Pod token. The
// API server answered, and the answer is a refusal.
var ErrNotPodToken = errors.New("issuer: not an accepted Pod token")

// bearerToken returns the one Bearer token of r's Authorization header.
// ok is false for no header, more than one, another scheme, a token count
// other than one, or a header over maxAuthorizationHeader.
func bearerToken(r *http.Request) (token string, ok bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 || len(values[0]) > maxAuthorizationHeader {
		return "", false
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	return parts[1], true
}

// TokenReviewer asks the API server who sent a token. An error means the
// API server could not answer. A refused token is a status with
// Authenticated false, not an error.
type TokenReviewer interface {
	Review(ctx context.Context, token string) (authenticationv1.TokenReviewStatus, error)
}

// APITokenReviewer reviews a token with a TokenReview that names IssuerURL
// as the one audience, so a token minted for anything else is refused by
// the API server itself.
type APITokenReviewer struct {
	Client    client.Client
	IssuerURL string
}

var _ TokenReviewer = (*APITokenReviewer)(nil)

// Review creates the TokenReview and returns its status.
func (r *APITokenReviewer) Review(ctx context.Context, token string) (authenticationv1.TokenReviewStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, tokenReviewTimeout)
	defer cancel()
	// client-go logs request and response bodies at -v=8 through the
	// context's logger, and this body holds the bearer. A discard logger
	// keeps the token out of the log at any verbosity.
	ctx = klog.NewContext(ctx, logr.Discard())

	review := &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{
			Token:     token,
			Audiences: []string{r.IssuerURL},
		},
	}
	if err := r.Client.Create(ctx, review); err != nil {
		return authenticationv1.TokenReviewStatus{}, fmt.Errorf("issuer: create TokenReview: %w", err)
	}
	return review.Status, nil
}

// BoundPod is the Pod a reviewed token was bound to.
type BoundPod struct {
	PodIdentity
	// Namespace is the Pod namespace. It comes from the service account
	// username, which the API server builds from the real namespace.
	Namespace string
}

// boundPod reads the Pod out of a review status. It accepts only an
// authenticated token for issuerURL, bound to a Pod, of the service
// account named serviceAccount. Every other status is ErrNotPodToken, with
// the failed condition and nothing from the status in the text.
func boundPod(status authenticationv1.TokenReviewStatus, issuerURL, serviceAccount string) (BoundPod, error) {
	if !status.Authenticated {
		return BoundPod{}, fmt.Errorf("%w: not authenticated", ErrNotPodToken)
	}
	// An empty list is a refusal. The API server returns the audiences the
	// token and the request share, so a match is never implied.
	if !slices.Contains(status.Audiences, issuerURL) {
		return BoundPod{}, fmt.Errorf("%w: audience", ErrNotPodToken)
	}
	podName, ok := extraValue(status.User.Extra, podNameExtra)
	if !ok {
		return BoundPod{}, fmt.Errorf("%w: pod name", ErrNotPodToken)
	}
	podUID, ok := extraValue(status.User.Extra, podUIDExtra)
	if !ok {
		return BoundPod{}, fmt.Errorf("%w: pod uid", ErrNotPodToken)
	}
	namespace, name, ok := splitUsername(status.User.Username)
	if !ok {
		return BoundPod{}, fmt.Errorf("%w: username", ErrNotPodToken)
	}
	if name != serviceAccount {
		return BoundPod{}, fmt.Errorf("%w: service account", ErrNotPodToken)
	}
	return BoundPod{
		PodIdentity: PodIdentity{UID: podUID, Name: podName},
		Namespace:   namespace,
	}, nil
}

// extraValue returns the value of key when it holds exactly one non-empty
// value. The API server sets each Pod extra once, so any other shape did
// not come from a Pod-bound token.
func extraValue(extra map[string]authenticationv1.ExtraValue, key string) (string, bool) {
	values := extra[key]
	if len(values) != 1 || values[0] == "" {
		return "", false
	}
	return values[0], true
}

// splitUsername reads the namespace and name out of a service account
// username. Both parts must be present and non-empty, and the namespace
// must be a DNS label, as the API server would have made it.
func splitUsername(username string) (namespace, name string, ok bool) {
	rest, ok := strings.CutPrefix(username, serviceAccountUsernamePrefix)
	if !ok {
		return "", "", false
	}
	namespace, name, ok = strings.Cut(rest, ":")
	if !ok || namespace == "" || name == "" || strings.Contains(name, ":") {
		return "", "", false
	}
	if len(validation.IsDNS1123Label(namespace)) > 0 {
		return "", "", false
	}
	return namespace, name, true
}
