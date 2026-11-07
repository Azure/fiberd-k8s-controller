package issuer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/fiberd/pkg/core"
	"github.com/Azure/fiberd/pkg/grant"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload/deployment"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload/podtemplate"
)

const (
	// podNamespace and podName are the Pod acceptedStatus binds its token
	// to, with podA as its UID.
	podNamespace = "tenant-a"
	podName      = "fiberd-web-1-7d9f"
	grantUID     = "5b6c7d8e-9f0a-4b1c-8d2e-3f4a5b6c7d8e"
	grantImage   = "ghcr.io/azure/fiberd:v0.4.0"

	// grantLease is the Handler's lease for a first grant.
	grantLease = 10 * time.Minute

	// previousFiberMax and previousLease are the claims of every previous
	// grant the tests sign. Both differ from the spec and from grantLease,
	// so a renewal is told from a first issuance by what the answer carries.
	previousFiberMax = 2
	previousLease    = 5 * time.Minute

	// staleHash is a grant hash no spec in these tests has.
	staleHash = "0123456789abcdef"

	// reviewError is what the API server says about a token it denies. It
	// must never reach a log line.
	reviewError = "token has expired"
)

var (
	grantDigest     = "sha256:" + strings.Repeat("ab", 32)
	grantThumbprint = strings.Repeat("Q", 43)
	errAPIServer    = errors.New("connection refused")

	// testStart is the fake clock's time. Whole seconds survive the claims.
	testStart = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
)

// fakeReviewer answers every review alike. The bearer is not read, since
// the route must do the same for any.
type fakeReviewer struct {
	status authenticationv1.TokenReviewStatus
	err    error
}

func (f fakeReviewer) Review(context.Context, string) (authenticationv1.TokenReviewStatus, error) {
	return f.status, f.err
}

// lineage is one grant Pod, the objects above it, and the Namespace and
// GrantPolicy that decide its trust. A nil object is left out of the API.
type lineage struct {
	grant      *v1alpha1.CapacityGrant
	deployment *appsv1.Deployment
	replicaSet *appsv1.ReplicaSet
	pod        *corev1.Pod
	namespace  *corev1.Namespace
	policy     *v1alpha1.GrantPolicy
}

// exceptionFor is the Namespace and the GrantPolicy that give podNamespace
// the exception for rt.
func exceptionFor(rt v1alpha1.Runtime) (*corev1.Namespace, *v1alpha1.GrantPolicy) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   podNamespace,
		Labels: map[string]string{v1alpha1.RuntimeLabel(rt): v1alpha1.LabelRuntimeValue},
	}}
	policy := &v1alpha1.GrantPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.GrantPolicyName},
		Spec:       v1alpha1.GrantPolicySpec{Runtimes: []v1alpha1.Runtime{rt}},
	}
	return ns, policy
}

// newLineage is a whole chain from the grant to the Pod acceptedStatus
// binds, as the reconciler and the Deployment controller would have built
// it, in a namespace that holds the exception for the grant's runtime. spec
// edits the grant before the Pod is stamped with its hash.
func newLineage(t *testing.T, spec func(*v1alpha1.CapacityGrant)) *lineage {
	t.Helper()
	cg := &v1alpha1.CapacityGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: podNamespace, UID: grantUID},
		Spec: v1alpha1.CapacityGrantSpec{
			Zygote:           v1alpha1.ZygoteSpec{Digest: grantDigest, Command: []string{"/app/zygote"}},
			Fibers:           v1alpha1.FiberBudget{Max: 4},
			CallerThumbprint: grantThumbprint,
			Runtime:          v1alpha1.RuntimeProc,
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  podtemplate.ContainerName,
					Image: grantImage,
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
					},
				}}},
			},
		},
	}
	if spec != nil {
		spec(cg)
	}
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name:            workload.ObjectName(cg),
		Namespace:       podNamespace,
		UID:             "deployment-uid",
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(cg, v1alpha1.GroupVersion.WithKind("CapacityGrant"))},
	}}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name:            "fiberd-web-1",
		Namespace:       podNamespace,
		UID:             "replicaset-uid",
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(d, appsv1.SchemeGroupVersion.WithKind("Deployment"))},
	}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            podName,
			Namespace:       podNamespace,
			UID:             podA,
			Labels:          workload.SelectorLabels(cg),
			Annotations:     map[string]string{podtemplate.AnnotationGrantHash: hashOf(t, cg)},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(rs, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))},
		},
		Spec: corev1.PodSpec{ServiceAccountName: testServiceAccount},
	}
	ns, policy := exceptionFor(cg.Spec.Runtime)
	return &lineage{grant: cg, deployment: d, replicaSet: rs, pod: pod, namespace: ns, policy: policy}
}

// hashOf is the hash the reconciler stamps on cg's Pods.
func hashOf(t *testing.T, cg *v1alpha1.CapacityGrant) string {
	t.Helper()
	hash, err := podtemplate.Hash(cg, podtemplate.ImageOf(cg))
	if err != nil {
		t.Fatalf("podtemplate.Hash: %v", err)
	}
	return hash
}

// objects is every object of the chain. A nil one is left out.
func (l *lineage) objects() []client.Object {
	var objs []client.Object
	if l.grant != nil {
		objs = append(objs, l.grant)
	}
	if l.deployment != nil {
		objs = append(objs, l.deployment)
	}
	if l.replicaSet != nil {
		objs = append(objs, l.replicaSet)
	}
	if l.pod != nil {
		objs = append(objs, l.pod)
	}
	if l.namespace != nil {
		objs = append(objs, l.namespace)
	}
	if l.policy != nil {
		objs = append(objs, l.policy)
	}
	return objs
}

// above is the chain without its Pod, for a client that has not seen the
// Pod yet.
func (l *lineage) above() []client.Object {
	c := *l
	c.pod = nil
	return c.objects()
}

// withGrant is the chain with cg in place of its grant. Nil leaves the
// grant out.
func (l *lineage) withGrant(cg *v1alpha1.CapacityGrant) []client.Object {
	c := *l
	c.grant = cg
	return c.objects()
}

// deleting marks obj as being deleted. The fake client refuses a deleting
// object without a finalizer, since the API server would have removed it.
func deleting(obj metav1.Object) {
	ts := metav1.NewTime(testStart.Add(-time.Minute))
	obj.SetDeletionTimestamp(&ts)
	obj.SetFinalizers([]string{v1alpha1.Finalizer})
}

// newReader is a fake client over copies of objs. The copies keep one chain
// safe to hand to both the cache and the live client.
func newReader(t *testing.T, funcs interceptor.Funcs, objs []client.Object) client.Reader {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add the client-go scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add the v1alpha1 scheme: %v", err)
	}
	copies := make([]client.Object, 0, len(objs))
	for _, o := range objs {
		copies = append(copies, o.DeepCopyObject().(client.Object))
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(copies...).WithInterceptorFuncs(funcs).Build()
}

// failGet fails every Get of an object of obj's type.
func failGet(obj client.Object) interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
			if reflect.TypeOf(o) == reflect.TypeOf(obj) {
				return errAPIServer
			}
			return c.Get(ctx, key, o, opts...)
		},
	}
}

// grantCase is one request to the grant route. The zero value stores a
// whole chain in both clients, reviews the bearer as acceptedStatus, posts
// an empty object and signs with a key.
type grantCase struct {
	name string
	// spec edits the grant before the Pod is stamped with its hash.
	spec func(*v1alpha1.CapacityGrant)
	// mutate edits the chain after that.
	mutate func(*lineage)
	// cache and live choose what each client holds. Nil holds the chain.
	cache, live           func(*lineage) []client.Object
	cacheFuncs, liveFuncs interceptor.Funcs
	// review edits the accepted status. reviewErr fails the review instead.
	review    func(*authenticationv1.TokenReviewStatus)
	reviewErr error
	noKey     bool
	noBearer  bool
	// body builds the body once the Handler exists. Nil posts {}.
	body func(*testing.T, *grantEnv) string
	// before runs against the Handler ahead of the request.
	before func(*Handler)

	wantStatus     int
	wantReason     string
	wantRetryAfter string
	// wantFiberMax, wantLease and wantIsolation are what the verified grant
	// of a 200 carries.
	wantFiberMax  int
	wantLease     time.Duration
	wantIsolation core.Isolation
}

// grantEnv is a Handler over one chain and what a case reads back from it.
type grantEnv struct {
	h *Handler
	// key signs the Handler's grants. Nil when the case has no key.
	key *jose.JSONWebKey
}

func newGrantEnv(t *testing.T, tc grantCase, log logr.Logger) *grantEnv {
	t.Helper()
	l := newLineage(t, tc.spec)
	if tc.mutate != nil {
		tc.mutate(l)
	}
	cached, live := l.objects(), l.objects()
	if tc.cache != nil {
		cached = tc.cache(l)
	}
	if tc.live != nil {
		live = tc.live(l)
	}
	status := acceptedStatus()
	if tc.review != nil {
		tc.review(&status)
	}

	clk := clocktesting.NewFakePassiveClock(testStart)
	iss := &grant.Issuer{URL: testIssuerURL, Now: clk.Now}
	var key *jose.JSONWebKey
	if !tc.noKey {
		ring, err := grant.NewKeyRing(jose.EdDSA)
		if err != nil {
			t.Fatalf("NewKeyRing: %v", err)
		}
		if key, err = ring.Signing(); err != nil {
			t.Fatalf("Signing: %v", err)
		}
		iss.Ring = ring
	}
	h := NewHandler(Config{
		Issuer:         iss,
		Reviewer:       fakeReviewer{status: status, err: tc.reviewErr},
		Cache:          newReader(t, tc.cacheFuncs, cached),
		Live:           newReader(t, tc.liveFuncs, live),
		Workload:       deployment.New(nil, nil),
		Limits:         testLimits,
		ServiceAccount: testServiceAccount,
		Lease:          grantLease,
		Clock:          clk,
		Log:            log,
	})
	return &grantEnv{h: h, key: key}
}

// previous is a grant for uid that this env's issuer signed at iat.
func (e *grantEnv) previous(t *testing.T, uid string, iat time.Time) string {
	t.Helper()
	return signPrevious(t, e.key, uid, iat)
}

// signPrevious signs a TRUSTED grant for uid with key, as the issuer would
// have at iat. Its claims are previousFiberMax and previousLease.
func signPrevious(t *testing.T, key *jose.JSONWebKey, uid string, iat time.Time) string {
	t.Helper()
	return signBound(t, key, uid, iat, grantThumbprint)
}

// signBound is signPrevious bound to the caller with thumbprint. Empty
// binds it to none, as a grant of a plaintext Pod was.
func signBound(t *testing.T, key *jose.JSONWebKey, uid string, iat time.Time, thumbprint string) string {
	t.Helper()
	return signIsolated(t, key, uid, iat, thumbprint, core.Trusted)
}

// signIsolated is signBound with isolation as the grant's isolation.
func signIsolated(t *testing.T, key *jose.JSONWebKey, uid string, iat time.Time, thumbprint string, isolation core.Isolation) string {
	t.Helper()
	tok, err := grant.Sign(core.Grant{
		UID:            uid,
		Issuer:         testIssuerURL,
		Audience:       podName,
		TemplateDigest: grantDigest,
		Tenant:         podNamespace,
		FiberMax:       previousFiberMax,
		MinTier:        core.TierBasic,
		LeaseExpiry:    iat.Add(previousLease),
		Policy: core.Policy{
			Durability:   core.BestEffort,
			Isolation:    isolation,
			EndpointMode: core.EndpointDirect,
		},
		CallerThumbprint: thumbprint,
	}, key, iat)
	if err != nil {
		t.Fatalf("grant.Sign: %v", err)
	}
	return tok
}

// newKey is a signing key the Handler does not publish.
func newKey(t *testing.T) *jose.JSONWebKey {
	t.Helper()
	key, err := grant.GenerateKey(jose.EdDSA)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return key
}

// renewal is a body that carries previous.
func renewal(previous string) string {
	b, _ := json.Marshal(grantRequest{Previous: previous})
	return string(b)
}

// renewingPodA is a body with a grant of podA this env signed a minute ago.
func renewingPodA(t *testing.T, e *grantEnv) string {
	return renewal(e.previous(t, podA, testStart.Add(-time.Minute)))
}

// stale gives the Pod a hash no spec has.
func stale(l *lineage) { l.pod.Annotations[podtemplate.AnnotationGrantHash] = staleHash }

// noLabel takes the exception's label off the namespace.
func noLabel(l *lineage) { l.namespace.Labels = nil }

// gvisor moves a grant to a runtime that needs no exception.
func gvisor(cg *v1alpha1.CapacityGrant) { cg.Spec.Runtime = v1alpha1.RuntimeGvisor }

// serve sends tc's request through the Handler's routes and returns the
// env and the answer.
func serve(t *testing.T, tc grantCase, log logr.Logger) (*grantEnv, *httptest.ResponseRecorder) {
	t.Helper()
	e := newGrantEnv(t, tc, log)
	if tc.before != nil {
		tc.before(e.h)
	}
	body := "{}"
	if tc.body != nil {
		body = tc.body(t, e)
	}
	r := post(body)
	if !tc.noBearer {
		r.Header.Set("Authorization", "Bearer "+testToken)
	}
	w := httptest.NewRecorder()
	e.h.Routes().ServeHTTP(w, r)
	return e, w
}

// grantRefusals is one case per refusal of the grant route.
func grantRefusals() []grantCase {
	return []grantCase{
		{
			name:       "401 no bearer",
			noBearer:   true,
			wantStatus: http.StatusUnauthorized,
			wantReason: reasonUnauthenticated,
		},
		{
			name: "401 a review that denies the token",
			review: func(s *authenticationv1.TokenReviewStatus) {
				s.Authenticated = false
				s.Error = reviewError
			},
			wantStatus: http.StatusUnauthorized,
			wantReason: reasonUnauthenticated,
		},
		{
			name:       "401 a token bound to no Pod",
			review:     func(s *authenticationv1.TokenReviewStatus) { delete(s.User.Extra, podUIDExtra) },
			wantStatus: http.StatusUnauthorized,
			wantReason: reasonUnauthenticated,
		},
		{
			name:       "403 a Pod that does not exist",
			mutate:     func(l *lineage) { l.pod = nil },
			wantStatus: http.StatusForbidden,
			wantReason: reasonForbidden,
		},
		{
			name:       "403 a Pod with another UID",
			mutate:     func(l *lineage) { l.pod.UID = podB },
			wantStatus: http.StatusForbidden,
			wantReason: reasonForbidden,
		},
		{
			name:       "403 a Pod of another service account",
			mutate:     func(l *lineage) { l.pod.Spec.ServiceAccountName = "default" },
			wantStatus: http.StatusForbidden,
			wantReason: reasonForbidden,
		},
		{
			name:       "403 a ReplicaSet recreated under the same name",
			mutate:     func(l *lineage) { l.replicaSet.UID = "other-uid" },
			wantStatus: http.StatusForbidden,
			wantReason: reasonForbidden,
		},
		{
			name:       "403 a Pod no grant owns",
			mutate:     func(l *lineage) { l.pod.OwnerReferences = nil },
			wantStatus: http.StatusForbidden,
			wantReason: reasonForbidden,
		},
		{
			name:       "403 a superseded Pod whose grant is gone from the API server",
			mutate:     stale,
			live:       func(l *lineage) []client.Object { return l.withGrant(nil) },
			wantStatus: http.StatusForbidden,
			wantReason: reasonForbidden,
		},
		{
			name:   "403 a superseded Pod whose grant was recreated",
			mutate: stale,
			live: func(l *lineage) []client.Object {
				cg := l.grant.DeepCopy()
				cg.UID = "other-uid"
				return l.withGrant(cg)
			},
			wantStatus: http.StatusForbidden,
			wantReason: reasonForbidden,
		},
		{
			name:       "409 a terminating Pod",
			mutate:     func(l *lineage) { deleting(l.pod) },
			wantStatus: http.StatusConflict,
			wantReason: reasonPodTerminating,
		},
		{
			name:       "409 a deleting grant on first issuance",
			mutate:     func(l *lineage) { deleting(l.grant) },
			wantStatus: http.StatusConflict,
			wantReason: reasonGrantDeleting,
		},
		{
			name:       "409 a deleting grant on renewal",
			mutate:     func(l *lineage) { deleting(l.grant) },
			body:       renewingPodA,
			wantStatus: http.StatusConflict,
			wantReason: reasonGrantDeleting,
		},
		{
			name:   "409 a deleting grant on the live re-read",
			mutate: stale,
			live: func(l *lineage) []client.Object {
				cg := l.grant.DeepCopy()
				deleting(cg)
				return l.withGrant(cg)
			},
			wantStatus: http.StatusConflict,
			wantReason: reasonGrantDeleting,
		},
		{
			name:       "409 a superseded first issuance",
			mutate:     stale,
			wantStatus: http.StatusConflict,
			wantReason: reasonSuperseded,
		},
		{
			name:       "409 a superseded Pod with a garbage previous",
			mutate:     stale,
			body:       func(*testing.T, *grantEnv) string { return renewal("not-a-grant") },
			wantStatus: http.StatusConflict,
			wantReason: reasonSuperseded,
		},
		{
			// The renewal is TRUSTED and gVisor has no exception, so it falls
			// to a first grant, and the Pod's spec is gone.
			name:       "409 a TRUSTED Pod whose spec moved to gVisor",
			spec:       gvisor,
			mutate:     stale,
			body:       renewingPodA,
			wantStatus: http.StatusConflict,
			wantReason: reasonSuperseded,
		},
		{
			// A Pod built when grant Pods still served plaintext holds a
			// grant bound to no caller. It must lapse, not renew forever.
			name: "409 a renewal of a grant bound to no caller",
			body: func(t *testing.T, e *grantEnv) string {
				return renewal(signBound(t, e.key, podA, testStart.Add(-time.Minute), ""))
			},
			wantStatus: http.StatusConflict,
			wantReason: reasonUnboundGrant,
		},
		{
			name:       "422 an invalid spec at first issuance",
			spec:       func(cg *v1alpha1.CapacityGrant) { cg.Spec.Fibers.Max = 0 },
			wantStatus: http.StatusUnprocessableEntity,
			wantReason: reasonInvalidSpec,
		},
		{
			name: "429 the per-Pod limit",
			before: func(h *Handler) {
				for range podBurst {
					h.limits.admitPod(podA)
				}
			},
			wantStatus:     http.StatusTooManyRequests,
			wantReason:     reasonRateLimited,
			wantRetryAfter: "1",
		},
		{
			name:           "503 a review the API server could not serve",
			reviewErr:      errAPIServer,
			wantStatus:     http.StatusServiceUnavailable,
			wantReason:     reasonUnavailable,
			wantRetryAfter: "5",
		},
		{
			name:           "503 a live read of the Pod that fails",
			cache:          func(l *lineage) []client.Object { return l.above() },
			liveFuncs:      failGet(&corev1.Pod{}),
			wantStatus:     http.StatusServiceUnavailable,
			wantReason:     reasonUnavailable,
			wantRetryAfter: "5",
		},
		{
			name:           "503 a live re-read of the grant that fails",
			mutate:         stale,
			liveFuncs:      failGet(&v1alpha1.CapacityGrant{}),
			wantStatus:     http.StatusServiceUnavailable,
			wantReason:     reasonUnavailable,
			wantRetryAfter: "5",
		},
		{
			name:           "503 a read of the Namespace that fails",
			liveFuncs:      failGet(&corev1.Namespace{}),
			wantStatus:     http.StatusServiceUnavailable,
			wantReason:     reasonUnavailable,
			wantRetryAfter: "5",
		},
		{
			name:           "503 a read of the GrantPolicy that fails",
			liveFuncs:      failGet(&v1alpha1.GrantPolicy{}),
			wantStatus:     http.StatusServiceUnavailable,
			wantReason:     reasonUnavailable,
			wantRetryAfter: "5",
		},
		{
			name:           "503 a read of the Namespace that fails on a renewal",
			liveFuncs:      failGet(&corev1.Namespace{}),
			body:           renewingPodA,
			wantStatus:     http.StatusServiceUnavailable,
			wantReason:     reasonUnavailable,
			wantRetryAfter: "5",
		},
		{
			name:           "503 a read of the GrantPolicy that fails on a renewal",
			liveFuncs:      failGet(&v1alpha1.GrantPolicy{}),
			body:           renewingPodA,
			wantStatus:     http.StatusServiceUnavailable,
			wantReason:     reasonUnavailable,
			wantRetryAfter: "5",
		},
		{
			name:       "500 an issuer with no key",
			noKey:      true,
			wantStatus: http.StatusInternalServerError,
			wantReason: reasonMintFailed,
		},
		{
			name:       "400 a body that does not parse",
			body:       func(*testing.T, *grantEnv) string { return "not json" },
			wantStatus: http.StatusBadRequest,
			wantReason: reasonBadRequest,
		},
	}
}

func TestServeGrantRefusals(t *testing.T) {
	for _, tc := range grantRefusals() {
		t.Run(tc.name, func(t *testing.T) {
			_, w := serve(t, tc, logr.Discard())

			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			if got, want := strings.TrimSpace(w.Body.String()), `{"error":"`+tc.wantReason+`"}`; got != want {
				t.Errorf("body = %s, want %s", got, want)
			}
			if got := w.Header().Get("Retry-After"); got != tc.wantRetryAfter {
				t.Errorf("Retry-After = %q, want %q", got, tc.wantRetryAfter)
			}
		})
	}
}

func TestServeGrantHidesWhichPodCheckFailed(t *testing.T) {
	_, missing := serve(t, grantCase{mutate: func(l *lineage) { l.pod = nil }}, logr.Discard())
	_, other := serve(t, grantCase{mutate: func(l *lineage) { l.pod.UID = podB }}, logr.Discard())

	if missing.Code != http.StatusForbidden || other.Code != http.StatusForbidden {
		t.Fatalf("status = %d and %d, want %d for both", missing.Code, other.Code, http.StatusForbidden)
	}
	if !bytes.Equal(missing.Body.Bytes(), other.Body.Bytes()) {
		t.Errorf("a missing Pod answers %s, another UID %s, want the same bytes", missing.Body, other.Body)
	}
	if !reflect.DeepEqual(missing.Header(), other.Header()) {
		t.Errorf("a missing Pod answers headers %v, another UID %v, want the same", missing.Header(), other.Header())
	}
}

func TestServeGrantIssues(t *testing.T) {
	first := grantCase{wantFiberMax: 4, wantLease: grantLease, wantIsolation: core.Trusted}
	renewed := grantCase{wantFiberMax: previousFiberMax, wantLease: previousLease, wantIsolation: core.Trusted}
	untrusted := grantCase{wantFiberMax: 4, wantLease: grantLease, wantIsolation: core.Untrusted}
	with := func(base grantCase, name string, change func(*grantCase)) grantCase {
		base.name = name
		change(&base)
		return base
	}
	cases := []grantCase{
		with(first, "a first grant with the exception is TRUSTED", func(*grantCase) {}),
		with(first, "a first runc grant with the exception is TRUSTED", func(c *grantCase) {
			c.spec = func(cg *v1alpha1.CapacityGrant) { cg.Spec.Runtime = v1alpha1.RuntimeRunc }
		}),
		with(untrusted, "a first grant with no label is UNTRUSTED", func(c *grantCase) { c.mutate = noLabel }),
		with(untrusted, "a first grant with no Namespace is UNTRUSTED", func(c *grantCase) {
			c.mutate = func(l *lineage) { l.namespace = nil }
		}),
		with(untrusted, "a first grant with no GrantPolicy is UNTRUSTED", func(c *grantCase) {
			c.mutate = func(l *lineage) { l.policy = nil }
		}),
		with(untrusted, "a first grant past the ceiling is UNTRUSTED", func(c *grantCase) {
			c.mutate = func(l *lineage) { l.policy.Spec.Runtimes = []v1alpha1.Runtime{v1alpha1.RuntimeHyperlight} }
		}),
		with(untrusted, "a first gVisor grant reads no trust object", func(c *grantCase) {
			c.spec = gvisor
			c.liveFuncs = failGet(&corev1.Namespace{})
		}),
		with(renewed, "a renewal with the exception keeps the claims of previous and stays TRUSTED", func(c *grantCase) {
			c.body = renewingPodA
		}),
		with(untrusted, "regression: a renewal after the label is gone is a first grant, UNTRUSTED", func(c *grantCase) {
			c.mutate = noLabel
			c.body = renewingPodA
		}),
		with(first, "regression: an UNTRUSTED renewal after the label is added is a first grant, TRUSTED", func(c *grantCase) {
			c.body = func(t *testing.T, e *grantEnv) string {
				return renewal(signIsolated(t, e.key, podA, testStart.Add(-time.Minute), grantThumbprint, core.Untrusted))
			}
		}),
		with(renewed, "an UNTRUSTED renewal needs no exception and reads no trust object", func(c *grantCase) {
			c.spec = gvisor
			c.liveFuncs = failGet(&corev1.Namespace{})
			c.wantIsolation = core.Untrusted
			c.body = func(t *testing.T, e *grantEnv) string {
				return renewal(signIsolated(t, e.key, podA, testStart.Add(-time.Minute), grantThumbprint, core.Untrusted))
			}
		}),
		with(untrusted, "regression: the exception of a stale cached runtime does not reach the live runtime", func(c *grantCase) {
			c.spec = func(cg *v1alpha1.CapacityGrant) { cg.Spec.Runtime = v1alpha1.RuntimeRunc }
			c.mutate = func(l *lineage) {
				l.namespace, l.policy = exceptionFor(v1alpha1.RuntimeProc)
				l.policy.Spec.Runtimes = []v1alpha1.Runtime{v1alpha1.RuntimeProc, v1alpha1.RuntimeRunc}
			}
			c.cache = func(l *lineage) []client.Object {
				old := l.grant.DeepCopy()
				old.Spec.Runtime = v1alpha1.RuntimeProc
				return l.withGrant(old)
			}
		}),
		with(renewed, "an expired previous still renews", func(c *grantCase) {
			c.body = func(t *testing.T, e *grantEnv) string {
				return renewal(e.previous(t, podA, testStart.Add(-2*previousLease)))
			}
		}),
		with(renewed, "a superseded Pod renews", func(c *grantCase) {
			c.mutate = stale
			c.body = renewingPodA
		}),
		with(renewed, "a Pod whose grant's spec became invalid renews", func(c *grantCase) {
			c.spec = func(cg *v1alpha1.CapacityGrant) { cg.Spec.Fibers.Max = 0 }
			c.body = renewingPodA
		}),
		with(first, "a previous of another Pod is ignored", func(c *grantCase) {
			c.body = func(t *testing.T, e *grantEnv) string {
				return renewal(e.previous(t, podB, testStart.Add(-time.Minute)))
			}
		}),
		with(first, "a previous signed by another key is ignored", func(c *grantCase) {
			c.body = func(t *testing.T, _ *grantEnv) string {
				return renewal(signPrevious(t, newKey(t), podA, testStart.Add(-time.Minute)))
			}
		}),
		with(first, "a garbage previous is ignored", func(c *grantCase) {
			c.body = func(*testing.T, *grantEnv) string { return renewal("not-a-grant") }
		}),
		with(first, "a Pod the cache has not seen is read live", func(c *grantCase) {
			c.cache = func(l *lineage) []client.Object { return l.above() }
		}),
		with(first, "regression: a stale cached grant is no 409", func(c *grantCase) {
			c.spec = func(cg *v1alpha1.CapacityGrant) { cg.Spec.Fibers.Max = 8 }
			c.cache = func(l *lineage) []client.Object {
				old := l.grant.DeepCopy()
				old.Spec.Fibers.Max = 4
				return l.withGrant(old)
			}
			c.wantFiberMax = 8
		}),
		with(first, "regression: a matching hash never reads the grant live", func(c *grantCase) {
			c.liveFuncs = failGet(&v1alpha1.CapacityGrant{})
		}),
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, w := serve(t, tc, logr.Discard())

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s, want %d", w.Code, w.Body, http.StatusOK)
			}
			if got := w.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			var got grantAnswer
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("the body %q is not a grant answer: %v", w.Body, err)
			}
			pub := grant.PublicJWKS(e.key).Keys[0]
			g, err := grant.Verify(got.Token, &pub, grant.VerifyOptions{Audience: podName, Issuer: testIssuerURL})
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if g.UID != podA || g.Tenant != podNamespace || g.Audience != podName || g.CallerThumbprint != grantThumbprint {
				t.Errorf("the grant is for uid %q tenant %q audience %q thumbprint %q, want %q %q %q %q",
					g.UID, g.Tenant, g.Audience, g.CallerThumbprint, podA, podNamespace, podName, grantThumbprint)
			}
			if g.FiberMax != tc.wantFiberMax {
				t.Errorf("fiberMax = %d, want %d", g.FiberMax, tc.wantFiberMax)
			}
			if g.Policy.Isolation != tc.wantIsolation {
				t.Errorf("isolation = %s, want %s", g.Policy.Isolation, tc.wantIsolation)
			}
			if got.UID != podA || got.Audience != podName {
				t.Errorf("the answer is for uid %q audience %q, want %q %q", got.UID, got.Audience, podA, podName)
			}
			if !got.IssuedAt.Equal(testStart) {
				t.Errorf("issuedAt = %s, want %s", got.IssuedAt, testStart)
			}
			if want := testStart.Add(tc.wantLease); !got.ExpiresAt.Equal(want) {
				t.Errorf("expiresAt = %s, want %s", got.ExpiresAt, want)
			}
			if !g.LeaseExpiry.Equal(got.ExpiresAt) {
				t.Errorf("the grant's lease ends at %s, the answer says %s", g.LeaseExpiry, got.ExpiresAt)
			}
		})
	}
}

func TestSignedUntrusted(t *testing.T) {
	key := newKey(t)
	iat := testStart.Add(-time.Minute)
	cases := []struct {
		name  string
		token string
		want  bool
	}{
		{name: "TRUSTED", token: signIsolated(t, key, podA, iat, grantThumbprint, core.Trusted)},
		{name: "UNTRUSTED", token: signIsolated(t, key, podA, iat, grantThumbprint, core.Untrusted), want: true},
		{name: "unspecified is untrusted", token: signIsolated(t, key, podA, iat, grantThumbprint, core.IsolationUnspecified), want: true},
		{name: "a token that cannot be read counts as trusted", token: "not-a-grant"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := signedUntrusted(tc.token); got != tc.want {
				t.Errorf("signedUntrusted = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestServeGrantLogsAnUntrustedGrant keeps the one log line that says why
// fiberd refuses a grant, since the CapacityGrant's status does not.
func TestServeGrantLogsAnUntrustedGrant(t *testing.T) {
	cases := []struct {
		name     string
		tc       grantCase
		wantLine bool
	}{
		{name: "proc with no exception", tc: grantCase{mutate: noLabel}, wantLine: true},
		{name: "a renewal that lost the exception", tc: grantCase{mutate: noLabel, body: renewingPodA}, wantLine: true},
		{name: "proc with the exception"},
		{name: "gVisor", tc: grantCase{spec: gvisor}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var lines []string
			log := funcr.New(func(prefix, args string) {
				if strings.Contains(args, untrustedMessage) {
					lines = append(lines, args)
				}
			}, funcr.Options{})

			if _, w := serve(t, tc.tc, log); w.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s, want %d", w.Code, w.Body, http.StatusOK)
			}

			if !tc.wantLine {
				if len(lines) != 0 {
					t.Errorf("logged %q, want no line", lines)
				}
				return
			}
			if len(lines) != 1 {
				t.Fatalf("logged %q, want one line", lines)
			}
			for _, want := range []string{podNamespace, `"web"`, string(v1alpha1.RuntimeProc)} {
				if !strings.Contains(lines[0], want) {
					t.Errorf("the line %q does not name %s", lines[0], want)
				}
			}
		})
	}
}

func TestServeGrantAnswerShape(t *testing.T) {
	_, w := serve(t, grantCase{}, logr.Discard())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s, want %d", w.Code, w.Body, http.StatusOK)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
		t.Fatalf("the body %q is not an object: %v", w.Body, err)
	}
	// Core's fetcher reads these names.
	want := []string{"audience", "expiresAt", "issuedAt", "token", "uid"}
	if got := slices.Sorted(maps.Keys(fields)); !slices.Equal(got, want) {
		t.Errorf("the answer has fields %q, want %q", got, want)
	}
	var got grantAnswer
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("the body %q is not a grant answer: %v", w.Body, err)
	}
	// Core's fetcher refuses a grant whose lease does not end after it
	// starts.
	if !got.ExpiresAt.After(got.IssuedAt) {
		t.Errorf("expiresAt %s is not after issuedAt %s", got.ExpiresAt, got.IssuedAt)
	}
}

func TestServeGrantNeverShowsATokenValue(t *testing.T) {
	cases := append(grantRefusals(), grantCase{name: "a renewal"})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var lines []string
			log := funcr.New(func(prefix, args string) {
				mu.Lock()
				defer mu.Unlock()
				lines = append(lines, prefix+" "+args)
			}, funcr.Options{Verbosity: 10})

			// Every case sends the bearer and a signed previous, whatever it
			// sent to earn its status, and the API server's own text rides
			// along on the review.
			tc.noBearer = false
			review := tc.review
			tc.review = func(s *authenticationv1.TokenReviewStatus) {
				if review != nil {
					review(s)
				}
				s.Error = reviewError
			}
			var previous string
			tc.body = func(t *testing.T, e *grantEnv) string {
				key := e.key
				if key == nil {
					key = newKey(t)
				}
				previous = signPrevious(t, key, podA, testStart.Add(-time.Minute))
				return renewal(previous)
			}

			_, w := serve(t, tc, log)

			secrets := map[string]string{"the bearer": testToken, "previous": previous, "the review error": reviewError}
			for what, secret := range secrets {
				if strings.Contains(w.Body.String(), secret) {
					t.Errorf("the %d body carries %s", w.Code, what)
				}
				for _, line := range lines {
					if strings.Contains(line, secret) {
						t.Errorf("a log line carries %s: %s", what, line)
					}
				}
			}
		})
	}
}
