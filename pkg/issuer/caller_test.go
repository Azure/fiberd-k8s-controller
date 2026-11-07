package issuer

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/fiberd/pkg/grant"
	"github.com/Azure/fiberd/pkg/tlsconf"
	"github.com/Azure/fiberd/pkg/tlsconf/tlsconftest"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload/deployment"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload/podtemplate"
)

// callerServerName is the name every Pod of the test grant's certificate
// carries.
const callerServerName = "fiberd-web." + podNamespace + ".svc"

// callerPKI is the fiberd CA and the caller a test grant names.
type callerPKI struct {
	ca     tlsconftest.Pair
	caller tlsconftest.Pair
}

func newCallerPKI(t *testing.T) callerPKI {
	t.Helper()
	ca := tlsconftest.NewCA(t, podtemplate.CAIssuerName)
	return callerPKI{ca: ca, caller: ca.Client(t, "caller")}
}

// thumbprint is the key thumbprint of p as fiberd computes it on Clone.
func thumbprint(t *testing.T, p tlsconftest.Pair) string {
	t.Helper()
	tp, err := tlsconf.Thumbprint(p.Cert)
	if err != nil {
		t.Fatalf("tlsconf.Thumbprint: %v", err)
	}
	return tp
}

// callerPod is a Pod of the lineage's grant, running at ip, created at
// created. rs is the ReplicaSet that owns it.
func callerPod(l *lineage, rs *appsv1.ReplicaSet, name, uid, ip string, created time.Time) *corev1.Pod {
	p := l.pod.DeepCopy()
	p.Name, p.UID = name, types.UID(uid)
	p.CreationTimestamp = metav1.NewTime(created)
	p.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(rs, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))}
	p.Status = corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip}
	return p
}

// callerEnv is the lineage of a grant that names the test caller, with its
// one Pod running, and what the caller presents.
type callerEnv struct {
	l   *lineage
	pki callerPKI
	// extra are the objects besides the lineage's.
	extra []client.Object
	// certs is the chain the caller presents. Nil presents none.
	certs  []*x509.Certificate
	body   string
	limits RateLimits
	funcs  interceptor.Funcs
	// liveFuncs, when set, replace funcs on the live client.
	liveFuncs *interceptor.Funcs
	noKey     bool
	noCA      bool
	before    func(*Handler)
}

func newCallerEnv(t *testing.T) *callerEnv {
	t.Helper()
	pki := newCallerPKI(t)
	tp := thumbprint(t, pki.caller)
	l := newLineage(t, func(cg *v1alpha1.CapacityGrant) { cg.Spec.CallerThumbprint = tp })
	l.pod.CreationTimestamp = metav1.NewTime(testStart.Add(-time.Hour))
	l.pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.1.0.1"}
	return &callerEnv{
		l:      l,
		pki:    pki,
		certs:  []*x509.Certificate{pki.caller.Cert},
		body:   `{"namespace":"` + podNamespace + `","name":"web"}`,
		limits: testLimits,
	}
}

// serve posts the env's request to the caller route.
func (e *callerEnv) serve(t *testing.T) (*grant.Issuer, *jose.JSONWebKey, *httptest.ResponseRecorder) {
	t.Helper()
	objs := append(e.l.objects(), e.extra...)
	clk := clocktesting.NewFakePassiveClock(testStart)
	iss := &grant.Issuer{URL: testIssuerURL, Now: clk.Now}
	var key *jose.JSONWebKey
	if !e.noKey {
		ring, err := grant.NewKeyRing(jose.EdDSA)
		if err != nil {
			t.Fatalf("NewKeyRing: %v", err)
		}
		if key, err = ring.Signing(); err != nil {
			t.Fatalf("Signing: %v", err)
		}
		iss.Ring = ring
	}
	pool := e.pki.ca.Pool()
	if e.noCA {
		pool = nil
	}
	live := e.funcs
	if e.liveFuncs != nil {
		live = *e.liveFuncs
	}
	h := NewHandler(Config{
		Issuer:         iss,
		ClientCAs:      pool,
		Cache:          newReader(t, e.funcs, objs),
		Live:           newReader(t, live, objs),
		Workload:       deployment.New(nil, nil),
		Limits:         e.limits,
		ServiceAccount: testServiceAccount,
		Lease:          grantLease,
		Clock:          clk,
		Log:            logr.Discard(),
	})
	if e.before != nil {
		e.before(h)
	}
	r := httptest.NewRequest(http.MethodPost, CallerGrantPath, strings.NewReader(e.body))
	r.RemoteAddr = ipA + ":40000"
	r.TLS = &tls.ConnectionState{PeerCertificates: e.certs}
	if e.certs == nil {
		r.TLS = nil
	}
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, r)
	return iss, key, w
}

// decodeCallerGrants reads a 200 of the caller route.
func decodeCallerGrants(t *testing.T, w *httptest.ResponseRecorder) callerGrantsAnswer {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s, want %d", w.Code, w.Body, http.StatusOK)
	}
	// The answer holds grant tokens, so no cache may keep it.
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	var got callerGrantsAnswer
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("the body %q is not a caller grants answer: %v", w.Body, err)
	}
	if got.ServerName != callerServerName {
		t.Errorf("serverName = %q, want %q", got.ServerName, callerServerName)
	}
	return got
}

// grantedPods is the audience of each grant, in answer order.
func grantedPods(a callerGrantsAnswer) []string {
	names := make([]string, 0, len(a.Grants))
	for _, g := range a.Grants {
		names = append(names, g.Audience)
	}
	return names
}

func TestServeCallerGrantsRefusals(t *testing.T) {
	cases := []struct {
		name           string
		edit           func(*testing.T, *callerEnv)
		wantStatus     int
		wantReason     string
		wantRetryAfter string
	}{
		{
			name:       "401 no client certificate",
			edit:       func(_ *testing.T, e *callerEnv) { e.certs = nil },
			wantStatus: http.StatusUnauthorized,
			wantReason: reasonUnauthenticated,
		},
		{
			name: "401 a client certificate from another CA",
			edit: func(t *testing.T, e *callerEnv) {
				e.certs = []*x509.Certificate{tlsconftest.NewCA(t, "other").Client(t, "caller").Cert}
			},
			wantStatus: http.StatusUnauthorized,
			wantReason: reasonUnauthenticated,
		},
		{
			// fiberd-ca signs leaves directly, so a leaf of an intermediate
			// it signed is refused even when the client sends the
			// intermediate and the grant names the leaf's key.
			name: "401 a client certificate chained through an intermediate of the fiberd CA",
			edit: func(t *testing.T, e *callerEnv) {
				intermediate := e.pki.ca.Issue(t, &x509.Certificate{Subject: pkix.Name{CommonName: "intermediate"}, IsCA: true,
					KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true})
				leaf := intermediate.Client(t, "caller")
				e.l.grant.Spec.CallerThumbprint = thumbprint(t, leaf)
				e.certs = []*x509.Certificate{leaf.Cert, intermediate.Cert}
			},
			wantStatus: http.StatusUnauthorized,
			wantReason: reasonUnauthenticated,
		},
		{
			name:       "401 a certificate for server auth only",
			edit:       func(t *testing.T, e *callerEnv) { e.certs = []*x509.Certificate{e.pki.ca.Server(t).Cert} },
			wantStatus: http.StatusUnauthorized,
			wantReason: reasonUnauthenticated,
		},
		{
			name:       "401 a client key with no thumbprint",
			edit:       func(t *testing.T, e *callerEnv) { e.certs = []*x509.Certificate{e.pki.ca.UnnamedClient(t).Cert} },
			wantStatus: http.StatusUnauthorized,
			wantReason: reasonUnauthenticated,
		},
		{
			name:       "401 an issuer with no client CA",
			edit:       func(_ *testing.T, e *callerEnv) { e.noCA = true },
			wantStatus: http.StatusUnauthorized,
			wantReason: reasonUnauthenticated,
		},
		{
			name: "403 a certificate the grant does not name",
			edit: func(t *testing.T, e *callerEnv) {
				e.certs = []*x509.Certificate{e.pki.ca.Client(t, "caller").Cert}
			},
			wantStatus: http.StatusForbidden,
			wantReason: reasonForbidden,
		},
		{
			name: "403 a grant that does not exist",
			edit: func(_ *testing.T, e *callerEnv) {
				e.body = `{"namespace":"` + podNamespace + `","name":"other"}`
			},
			wantStatus: http.StatusForbidden,
			wantReason: reasonForbidden,
		},
		{
			name:       "409 a deleting grant",
			edit:       func(_ *testing.T, e *callerEnv) { deleting(e.l.grant) },
			wantStatus: http.StatusConflict,
			wantReason: reasonGrantDeleting,
		},
		{
			name: "422 an invalid spec, with no Pod yet",
			edit: func(_ *testing.T, e *callerEnv) {
				e.l.grant.Spec.Fibers.Max = 0
				e.l.pod = nil
			},
			wantStatus: http.StatusUnprocessableEntity,
			wantReason: reasonInvalidSpec,
		},
		{
			name:       "400 a body that does not parse",
			edit:       func(_ *testing.T, e *callerEnv) { e.body = "not json" },
			wantStatus: http.StatusBadRequest,
			wantReason: reasonBadRequest,
		},
		{
			name:       "400 no grant name",
			edit:       func(_ *testing.T, e *callerEnv) { e.body = `{"namespace":"` + podNamespace + `"}` },
			wantStatus: http.StatusBadRequest,
			wantReason: reasonBadRequest,
		},
		{
			name:       "400 a namespace that is no name",
			edit:       func(_ *testing.T, e *callerEnv) { e.body = `{"namespace":"a/b","name":"web"}` },
			wantStatus: http.StatusBadRequest,
			wantReason: reasonBadRequest,
		},
		{
			// The per-address limit runs first, so even a client with no
			// certificate is stopped by it and costs no chain check.
			name: "429 the per-address limit before the certificate",
			edit: func(_ *testing.T, e *callerEnv) {
				e.certs = nil
				e.limits.PerIP = 1
				e.before = func(h *Handler) { h.limits.admit(ipA) }
			},
			wantStatus:     http.StatusTooManyRequests,
			wantReason:     reasonRateLimited,
			wantRetryAfter: "1",
		},
		{
			name: "429 the per-caller limit",
			edit: func(t *testing.T, e *callerEnv) {
				tp := thumbprint(t, e.pki.caller)
				e.limits.PodInterval = time.Second
				e.before = func(h *Handler) {
					for range podBurst {
						h.limits.admitCaller(tp, podNamespace+"/web", ipA)
					}
				}
			},
			wantStatus:     http.StatusTooManyRequests,
			wantReason:     reasonRateLimited,
			wantRetryAfter: "1",
		},
		{
			name: "503 a list of the Pods that fails",
			edit: func(_ *testing.T, e *callerEnv) {
				e.funcs = interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					return errAPIServer
				}}
			},
			wantStatus:     http.StatusServiceUnavailable,
			wantReason:     reasonUnavailable,
			wantRetryAfter: "5",
		},
		{
			name:           "503 a read of the grant that fails",
			edit:           func(_ *testing.T, e *callerEnv) { e.funcs = failGet(&v1alpha1.CapacityGrant{}) },
			wantStatus:     http.StatusServiceUnavailable,
			wantReason:     reasonUnavailable,
			wantRetryAfter: "5",
		},
		{
			name:       "500 an issuer with no key",
			edit:       func(_ *testing.T, e *callerEnv) { e.noKey = true },
			wantStatus: http.StatusInternalServerError,
			wantReason: reasonMintFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newCallerEnv(t)
			tc.edit(t, e)
			_, _, w := e.serve(t)

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

func TestServeCallerGrantsHidesWhetherTheGrantExists(t *testing.T) {
	other := newCallerEnv(t)
	other.certs = []*x509.Certificate{other.pki.ca.Client(t, "stranger").Cert}
	_, _, notNamed := other.serve(t)

	missing := newCallerEnv(t)
	missing.l.grant = nil
	_, _, gone := missing.serve(t)

	if notNamed.Code != http.StatusForbidden || gone.Code != http.StatusForbidden {
		t.Fatalf("status = %d and %d, want %d for both", notNamed.Code, gone.Code, http.StatusForbidden)
	}
	if !bytes.Equal(notNamed.Body.Bytes(), gone.Body.Bytes()) {
		t.Errorf("another key answers %s, a missing grant %s, want the same bytes", notNamed.Body, gone.Body)
	}
	if !reflect.DeepEqual(notNamed.Header(), gone.Header()) {
		t.Errorf("another key answers headers %v, a missing grant %v, want the same", notNamed.Header(), gone.Header())
	}
}

// TestServeCallerGrantsAcrossARollout serves a grant whose old Pod still
// runs beside the new one, with Pods the caller cannot use around them.
// Each grant verifies only for its own Pod, and is bound to the caller.
func TestServeCallerGrantsAcrossARollout(t *testing.T) {
	e := newCallerEnv(t)
	// The lineage's Pod is the old revision, built from an earlier spec.
	stale(e.l)
	newRS := e.l.replicaSet.DeepCopy()
	newRS.Name, newRS.UID = "fiberd-web-2", "replicaset-2-uid"
	newPod := callerPod(e.l, newRS, "fiberd-web-2-a1b2", podB, "10.1.0.2", testStart.Add(-time.Minute))
	newPod.Annotations = map[string]string{podtemplate.AnnotationGrantHash: hashOf(t, e.l.grant)}

	terminating := callerPod(e.l, newRS, "fiberd-web-2-term", "uid-term", "10.1.0.3", testStart)
	deleting(terminating)
	pending := callerPod(e.l, newRS, "fiberd-web-2-pend", "uid-pend", "", testStart)
	pending.Status = corev1.PodStatus{Phase: corev1.PodPending}
	unowned := callerPod(e.l, newRS, "fiberd-web-2-free", "uid-free", "10.1.0.4", testStart)
	unowned.OwnerReferences = nil
	otherAccount := callerPod(e.l, newRS, "fiberd-web-2-acct", "uid-acct", "10.1.0.5", testStart)
	otherAccount.Spec.ServiceAccountName = "default"
	// The new ReplicaSet sits under the same Deployment.
	e.extra = []client.Object{newRS, newPod, terminating, pending, unowned, otherAccount}

	_, key, w := e.serve(t)
	got := decodeCallerGrants(t, w)
	if want := []string{newPod.Name, podName}; !slices.Equal(grantedPods(got), want) {
		t.Fatalf("grants for %q, want %q, newest first", grantedPods(got), want)
	}
	pub := grant.PublicJWKS(key).Keys[0]
	tp := thumbprint(t, e.pki.caller)
	pods := []*corev1.Pod{newPod, e.l.pod}
	for i, cg := range got.Grants {
		pod := pods[i]
		if cg.UID != string(pod.UID) || cg.Address != pod.Status.PodIP+":8484" {
			t.Errorf("grant %d is for uid %q at %q, want %q at %q", i, cg.UID, cg.Address, pod.UID, pod.Status.PodIP+":8484")
		}
		g, err := grant.Verify(cg.Token, &pub, grant.VerifyOptions{Audience: pod.Name, Issuer: testIssuerURL})
		if err != nil {
			t.Fatalf("Verify grant %d for its Pod: %v", i, err)
		}
		if g.UID != string(pod.UID) || g.Audience != pod.Name || g.CallerThumbprint != tp || g.Tenant != podNamespace {
			t.Errorf("grant %d is for uid %q audience %q caller %q tenant %q, want %q %q %q %q",
				i, g.UID, g.Audience, g.CallerThumbprint, g.Tenant, pod.UID, pod.Name, tp, podNamespace)
		}
		if !g.LeaseExpiry.Equal(cg.ExpiresAt) || !cg.ExpiresAt.Equal(testStart.Add(grantLease)) {
			t.Errorf("grant %d expires at %s, the answer says %s, want %s", i, g.LeaseExpiry, cg.ExpiresAt, testStart.Add(grantLease))
		}
		other := pods[1-i]
		if _, err := grant.Verify(cg.Token, &pub, grant.VerifyOptions{Audience: other.Name, Issuer: testIssuerURL}); err == nil {
			t.Errorf("grant %d verifies for the other Pod %s too", i, other.Name)
		}
	}
}

// TestServeCallerGrantsAfterACallerThumbprintChange moves the grant to a
// new caller key. The change rolls the Deployment, and the old Pod still
// runs, built from the spec that named the old key. Every grant the route
// mints comes from the current spec, so each binds the new key, the old
// Pod's included. fiberd keeps that Pod on the grant it was built from and
// refuses a token for another caller there, so the new key reaches only the
// new Pod. The old key is refused like any key the grant does not name.
func TestServeCallerGrantsAfterACallerThumbprintChange(t *testing.T) {
	e := newCallerEnv(t)
	oldCaller := e.pki.caller
	oldSpec := e.l.grant.DeepCopy()
	e.l.pod.Annotations = map[string]string{podtemplate.AnnotationGrantHash: hashOf(t, oldSpec)}

	newCaller := e.pki.ca.Client(t, "caller-2")
	e.l.grant.Spec.CallerThumbprint = thumbprint(t, newCaller)
	newHash := hashOf(t, e.l.grant)
	if newHash == e.l.pod.Annotations[podtemplate.AnnotationGrantHash] {
		t.Fatal("a callerThumbprint change leaves the Pod-template hash as it was, so it would not roll")
	}
	newRS := e.l.replicaSet.DeepCopy()
	newRS.Name, newRS.UID = "fiberd-web-2", "replicaset-2-uid"
	newPod := callerPod(e.l, newRS, "fiberd-web-2-a1b2", podB, "10.1.0.2", testStart.Add(-time.Minute))
	newPod.Annotations = map[string]string{podtemplate.AnnotationGrantHash: newHash}
	e.extra = []client.Object{newRS, newPod}

	e.certs = []*x509.Certificate{newCaller.Cert}
	_, key, w := e.serve(t)
	got := decodeCallerGrants(t, w)
	if want := []string{newPod.Name, podName}; !slices.Equal(grantedPods(got), want) {
		t.Fatalf("grants for %q, want %q, newest first", grantedPods(got), want)
	}
	pub := grant.PublicJWKS(key).Keys[0]
	for i, cg := range got.Grants {
		g, err := grant.Verify(cg.Token, &pub, grant.VerifyOptions{Audience: cg.Audience, Issuer: testIssuerURL})
		if err != nil {
			t.Fatalf("Verify grant %d: %v", i, err)
		}
		if want := thumbprint(t, newCaller); g.CallerThumbprint != want {
			t.Errorf("grant %d for %s binds caller %q, want the new key %q", i, cg.Audience, g.CallerThumbprint, want)
		}
	}

	e.certs = []*x509.Certificate{oldCaller.Cert}
	if _, _, w := e.serve(t); w.Code != http.StatusForbidden {
		t.Errorf("the old key gets status %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestServeCallerGrantsChoosesPods(t *testing.T) {
	// many adds n more running Pods of the lineage's ReplicaSet, each newer
	// than the last.
	many := func(e *callerEnv, n int) {
		for i := range n {
			e.extra = append(e.extra, callerPod(e.l, e.l.replicaSet, fmt.Sprintf("fiberd-web-1-%02d", i),
				fmt.Sprintf("uid-%02d", i), fmt.Sprintf("10.2.0.%d", i+1), testStart.Add(time.Duration(i)*time.Second)))
		}
	}
	cases := []struct {
		name string
		edit func(*callerEnv)
		want []string
	}{
		{name: "the one running Pod", edit: func(*callerEnv) {}, want: []string{podName}},
		{name: "a grant with no Pod yet returns none", edit: func(e *callerEnv) { e.l.pod = nil }},
		{name: "a Pod that is not running yet returns none", edit: func(e *callerEnv) { e.l.pod.Status = corev1.PodStatus{Phase: corev1.PodPending} }},
		{name: "a terminating Pod returns none", edit: func(e *callerEnv) { deleting(e.l.pod) }},
		{name: "a Pod no grant owns returns none", edit: func(e *callerEnv) { e.l.pod.OwnerReferences = nil }},
		{name: "a Pod whose ReplicaSet another Deployment owns returns none", edit: func(e *callerEnv) {
			e.l.replicaSet.OwnerReferences[0].UID = "other-deployment-uid"
		}},
		{name: "an old revision is included", edit: func(e *callerEnv) { stale(e.l) }, want: []string{podName}},
		{name: "an audience names one Pod", edit: func(e *callerEnv) {
			many(e, 2)
			e.body = `{"namespace":"` + podNamespace + `","name":"web","audience":"` + podName + `"}`
		}, want: []string{podName}},
		{name: "an audience no Pod has returns none", edit: func(e *callerEnv) {
			e.body = `{"namespace":"` + podNamespace + `","name":"web","audience":"fiberd-web-9"}`
		}},
		{name: "one answer holds the newest Pods up to its bound", edit: func(e *callerEnv) { many(e, maxCallerGrants+2) }, want: []string{
			"fiberd-web-1-09", "fiberd-web-1-08", "fiberd-web-1-07", "fiberd-web-1-06",
			"fiberd-web-1-05", "fiberd-web-1-04", "fiberd-web-1-03", "fiberd-web-1-02",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newCallerEnv(t)
			tc.edit(e)
			_, _, w := e.serve(t)
			got := decodeCallerGrants(t, w)
			if !slices.Equal(grantedPods(got), tc.want) {
				t.Errorf("grants for %q, want %q", grantedPods(got), tc.want)
			}
			if got.Grants == nil {
				t.Error("grants is null, want an array")
			}
		})
	}
}

func TestServeCallerGrantsAnswerShape(t *testing.T) {
	_, _, w := newCallerEnv(t).serve(t)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
		t.Fatalf("the body %q is not an object: %v", w.Body, err)
	}
	if got, want := slices.Sorted(maps.Keys(fields)), []string{"grants", "serverName"}; !slices.Equal(got, want) {
		t.Errorf("the answer has fields %q, want %q", got, want)
	}
	var grants []map[string]json.RawMessage
	if err := json.Unmarshal(fields["grants"], &grants); err != nil || len(grants) != 1 {
		t.Fatalf("grants %s is not one object: %v", fields["grants"], err)
	}
	want := []string{"address", "audience", "expiresAt", "issuedAt", "token", "uid"}
	if got := slices.Sorted(maps.Keys(grants[0])); !slices.Equal(got, want) {
		t.Errorf("a grant has fields %q, want %q", got, want)
	}
}

// TestServeCallerGrantsPodRouteNeedsNoCertificate keeps the Pod's route on
// the same Handler open to a client with no certificate.
func TestServeCallerGrantsPodRouteNeedsNoCertificate(t *testing.T) {
	_, w := serve(t, grantCase{}, logr.Discard())
	if w.Code != http.StatusOK {
		t.Fatalf("the Pod's route answers %d %s without a client certificate, want 200", w.Code, w.Body)
	}
}

// TestServeCallerGrantsLimitsEachGrant shows that one caller named by two
// grants spends each grant's budget on its own.
func TestServeCallerGrantsLimitsEachGrant(t *testing.T) {
	e := newCallerEnv(t)
	e.limits.PodInterval = time.Hour
	other := e.l.grant.DeepCopy()
	other.Name, other.UID = "web2", "web2-uid"
	e.extra = []client.Object{other}
	tp := thumbprint(t, e.pki.caller)
	e.before = func(h *Handler) {
		for range podBurst {
			h.limits.admitCaller(tp, podNamespace+"/web", ipA)
		}
	}
	e.body = `{"namespace":"` + podNamespace + `","name":"web2"}`
	if _, _, w := e.serve(t); w.Code != http.StatusOK {
		t.Errorf("the second grant answers %d %s once the first's budget is spent, want 200", w.Code, w.Body)
	}
	e.body = `{"namespace":"` + podNamespace + `","name":"web"}`
	if _, _, w := e.serve(t); w.Code != http.StatusTooManyRequests {
		t.Errorf("the first grant answers %d %s with its budget spent, want 429", w.Code, w.Body)
	}
}

// TestServeCallerGrantsReadsOnlyTheCache keeps a missing grant from costing
// a live read, whose latency would tell it from a grant that names another
// key.
func TestServeCallerGrantsReadsOnlyTheCache(t *testing.T) {
	fail := interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errAPIServer
		},
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errAPIServer
		},
	}
	for name, body := range map[string]string{
		"a missing grant": `{"namespace":"` + podNamespace + `","name":"other"}`,
		"a served grant":  `{"namespace":"` + podNamespace + `","name":"web"}`,
	} {
		t.Run(name, func(t *testing.T) {
			e := newCallerEnv(t)
			e.liveFuncs = &fail
			e.body = body
			if _, _, w := e.serve(t); w.Code == http.StatusServiceUnavailable {
				t.Errorf("the route read the API server: %d %s", w.Code, w.Body)
			}
		})
	}
}

// TestServeCallerGrantsLimitsEachAddress shows that the replicas of one
// caller, sharing its key, spend their budgets for one grant apart.
func TestServeCallerGrantsLimitsEachAddress(t *testing.T) {
	e := newCallerEnv(t)
	e.limits.PodInterval = time.Hour
	tp := thumbprint(t, e.pki.caller)
	e.before = func(h *Handler) {
		for range podBurst {
			h.limits.admitCaller(tp, podNamespace+"/web", ipB)
		}
	}
	if _, _, w := e.serve(t); w.Code != http.StatusOK {
		t.Errorf("an address answers %d %s once another address spent its budget, want 200", w.Code, w.Body)
	}
	e.before = func(h *Handler) {
		for range podBurst {
			h.limits.admitCaller(tp, podNamespace+"/web", ipA)
		}
	}
	if _, _, w := e.serve(t); w.Code != http.StatusTooManyRequests {
		t.Errorf("an address answers %d %s with its own budget spent, want 429", w.Code, w.Body)
	}
}
