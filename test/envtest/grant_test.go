//go:build envtest

package envtest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"

	"github.com/Azure/fiberd/pkg/grant"

	"github.com/Azure/fiberd-k8s-controller/pkg/issuer"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload/podtemplate"
	"github.com/Azure/fiberd-k8s-controller/test/envtest/internal/testutil"
)

const (
	// holdFinalizer keeps a deleted Pod in the API, since envtest has no
	// kubelet to end it and an unscheduled Pod goes at once.
	holdFinalizer = "envtest.fiberd.io/hold"

	// tokenSeconds is the shortest life the API server allows.
	tokenSeconds = 600

	// otherAudience is an audience the issuer never asks for.
	otherAudience = "https://other.example.com"

	// reasonUnauthenticated, reasonForbidden and reasonPodTerminating are the
	// error field of a refusal, the contract core's fetcher reads.
	reasonUnauthenticated = "Unauthenticated"
	reasonForbidden       = "Forbidden"
	reasonPodTerminating  = "PodTerminating"
)

// grantAnswer is what the grant route answers, as core's fetcher reads it.
type grantAnswer struct {
	Token     string    `json:"token"`
	UID       string    `json:"uid"`
	Audience  string    `json:"audience"`
	IssuedAt  time.Time `json:"issuedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	Error     string    `json:"error"`
}

// TestGrantRoute runs the route against a real API server, so the TokenReview
// answers what the unit tests fake. envtest runs no Deployment controller or
// kubelet, so each case builds what they would have built from the Deployment
// the running manager applied.
func TestGrantRoute(t *testing.T) {
	waitForProbe(t, "/readyz")

	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{name: "a Pod of a grant gets its grant over HTTPS and renews it", run: testIssuesAndRenews},
		{name: "a token for another audience gets nothing", run: testOtherAudience},
		{name: "a token bound to no Pod gets nothing", run: testUnboundToken},
		{name: "a token of another service account gets nothing", run: testOtherServiceAccount},
		{name: "a Pod no grant owns gets nothing", run: testUnownedPod},
		{name: "a terminating Pod gets nothing while its token still reviews", run: testTerminatingPod},
		{name: "the grant service account can read its Pod and no Secret", run: testGrantServiceAccountRights},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.run)
	}
}

func testIssuesAndRenews(t *testing.T) {
	pod := newGrantChain(t, "issues").pod(t, "fiberd-issues-1-abcde", nil)
	token := podToken(t, pod, issuerURL)

	first := wantGrant(t, pod, token, `{}`)

	body, err := json.Marshal(map[string]string{"previous": first.Token})
	if err != nil {
		t.Fatal(err)
	}
	renewed := wantGrant(t, pod, token, string(body))
	if renewed.IssuedAt.Before(first.IssuedAt) || renewed.ExpiresAt.Before(first.ExpiresAt) {
		t.Errorf("the renewal runs %s to %s, before the grant's %s to %s", renewed.IssuedAt, renewed.ExpiresAt, first.IssuedAt, first.ExpiresAt)
	}
}

func testOtherAudience(t *testing.T) {
	pod := newGrantChain(t, "other-audience").pod(t, "fiberd-other-audience-1-abcde", nil)

	wantRefusal(t, podToken(t, pod, otherAudience), http.StatusUnauthorized, reasonUnauthenticated)
}

func testUnboundToken(t *testing.T) {
	pod := newGrantChain(t, "unbound").pod(t, "fiberd-unbound-1-abcde", nil)
	req := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{
		Audiences:         []string{issuerURL},
		ExpirationSeconds: ptr.To[int64](tokenSeconds),
	}}

	wantRefusal(t, requestToken(t, pod, req), http.StatusUnauthorized, reasonUnauthenticated)
}

func testOtherServiceAccount(t *testing.T) {
	c := newGrantChain(t, "other-sa")
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: c.deployment.Namespace, Name: "other"}}
	if err := admin.Create(t.Context(), sa); err != nil {
		t.Fatalf("create ServiceAccount %s: %v", sa.Name, err)
	}
	pod := c.pod(t, "fiberd-other-sa-1-abcde", func(p *corev1.Pod) { p.Spec.ServiceAccountName = sa.Name })

	wantRefusal(t, podToken(t, pod, issuerURL), http.StatusUnauthorized, reasonUnauthenticated)
}

func testUnownedPod(t *testing.T) {
	pod := newGrantChain(t, "unowned").pod(t, "fiberd-unowned-1-abcde", func(p *corev1.Pod) { p.OwnerReferences = nil })

	wantRefusal(t, podToken(t, pod, issuerURL), http.StatusForbidden, reasonForbidden)
}

// testTerminatingPod deletes the Pod after its token is minted. The API
// server honours that token for a minute more, so the refusal can only come
// from the controller's own check. The cached Pod can lag the delete, so the
// answers before the refusal are not held against it.
func testTerminatingPod(t *testing.T) {
	pod := newGrantChain(t, "terminating").pod(t, "fiberd-terminating-1-abcde", func(p *corev1.Pod) {
		p.Finalizers = []string{holdFinalizer}
	})
	token := podToken(t, pod, issuerURL)
	if err := admin.Delete(t.Context(), pod); err != nil {
		t.Fatalf("delete Pod %s: %v", pod.Name, err)
	}

	testutil.Eventually(t, "a refusal of the terminating Pod", func(ctx context.Context) (bool, error) {
		status, got := postGrant(ctx, t, token, `{}`)
		if status != http.StatusConflict {
			return false, testutil.NotYet(fmt.Errorf("the route answered %d", status))
		}
		if got.Error != reasonPodTerminating {
			return false, fmt.Errorf("the route answered 409 %q, want %q", got.Error, reasonPodTerminating)
		}
		return true, nil
	})
}

// testGrantServiceAccountRights asks as the account a grant Pod runs as. The
// Pod rule proves the binding took effect, so the Secret refusal is not just
// an account that holds nothing yet.
func testGrantServiceAccountRights(t *testing.T) {
	ns := tenantNamespace(t, false)
	onboardGrantAccount(t, ns)
	user := "system:serviceaccount:" + ns + ":" + podtemplate.DefaultServiceAccount

	testutil.Eventually(t, "the grant RoleBinding to take effect", func(ctx context.Context) (bool, error) {
		return userAllowed(ctx, user, &authorizationv1.ResourceAttributes{Namespace: ns, Verb: "get", Resource: "pods"})
	})
	for _, verb := range []string{"get", "list", "watch"} {
		ok, err := userAllowed(t.Context(), user, &authorizationv1.ResourceAttributes{Namespace: ns, Verb: verb, Resource: "secrets"})
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Errorf("%s may %s Secrets in its namespace", user, verb)
		}
	}
}

// grantChain is the Deployment the manager applied for a grant, and the
// ReplicaSet the Deployment controller would have made from it.
type grantChain struct {
	deployment *appsv1.Deployment
	replicaSet *appsv1.ReplicaSet
}

// newGrantChain creates a grant in a namespace onboarded as config/tenant
// onboards one, and waits for the manager to apply its Deployment.
func newGrantChain(t *testing.T, name string) *grantChain {
	t.Helper()
	ns := tenantNamespace(t, true)
	onboardGrantAccount(t, ns)
	d, _ := waitForObjects(t, createGrant(t, ns, name))
	ref := metav1.NewControllerRef(d, appsv1.SchemeGroupVersion.WithKind("Deployment"))
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       ns,
			Name:            d.Name + "-1",
			Labels:          d.Spec.Template.Labels,
			OwnerReferences: []metav1.OwnerReference{*ref},
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: ptr.To[int32](0),
			Selector: d.Spec.Selector,
			Template: d.Spec.Template,
		},
	}
	if err := admin.Create(t.Context(), rs); err != nil {
		t.Fatalf("create ReplicaSet %s: %v", rs.Name, err)
	}
	return &grantChain{deployment: d, replicaSet: rs}
}

// pod creates the Pod the chain's ReplicaSet would have made, after edit.
func (c *grantChain) pod(t *testing.T, name string, edit func(*corev1.Pod)) *corev1.Pod {
	t.Helper()
	tmpl := c.replicaSet.Spec.Template
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       c.replicaSet.Namespace,
			Name:            name,
			Labels:          tmpl.Labels,
			Annotations:     tmpl.Annotations,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(c.replicaSet, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))},
		},
		Spec: *tmpl.Spec.DeepCopy(),
	}
	if edit != nil {
		edit(p)
	}
	if err := admin.Create(t.Context(), p); err != nil {
		t.Fatalf("create Pod %s: %v", name, err)
	}
	return p
}

// onboardGrantAccount applies the ServiceAccount and RoleBinding of
// config/tenant to ns. kustomize fills in the namespace of the binding's
// subject, so it is set here.
func onboardGrantAccount(t *testing.T, ns string) {
	t.Helper()
	objs, err := testutil.ReadObjects(
		filepath.Join(repoRoot, "config", "tenant", "service_account.yaml"),
		filepath.Join(repoRoot, "config", "tenant", "grant_role_binding.yaml"),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range objs {
		obj.SetNamespace(ns)
		subjects, found, err := unstructured.NestedSlice(obj.Object, "subjects")
		if err != nil {
			t.Fatal(err)
		}
		if found {
			for _, s := range subjects {
				s.(map[string]any)["namespace"] = ns
			}
			if err := unstructured.SetNestedSlice(obj.Object, subjects, "subjects"); err != nil {
				t.Fatal(err)
			}
		}
		if err := admin.Create(t.Context(), obj); err != nil {
			t.Fatalf("create %s %s in %s: %v", obj.GetKind(), obj.GetName(), ns, err)
		}
	}
}

// podToken mints a token bound to pod for audience, as the kubelet does.
func podToken(t *testing.T, pod *corev1.Pod, audience string) string {
	t.Helper()
	return requestToken(t, pod, &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{
		Audiences:         []string{audience},
		ExpirationSeconds: ptr.To[int64](tokenSeconds),
		BoundObjectRef: &authenticationv1.BoundObjectReference{
			Kind:       "Pod",
			APIVersion: "v1",
			Name:       pod.Name,
			UID:        pod.UID,
		},
	}})
}

// requestToken runs req for the service account pod runs as.
func requestToken(t *testing.T, pod *corev1.Pod, req *authenticationv1.TokenRequest) string {
	t.Helper()
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: pod.Namespace, Name: pod.Spec.ServiceAccountName}}
	if err := admin.SubResource("token").Create(t.Context(), sa, req); err != nil {
		t.Fatalf("request a token of ServiceAccount %s: %v", sa.Name, err)
	}
	return req.Status.Token
}

// postGrant sends body to the grant route with token as the bearer.
func postGrant(ctx context.Context, t *testing.T, token, body string) (int, grantAnswer) {
	t.Helper()
	url := "https://" + issuerAddr + issuer.GrantPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := issuerClient(t).Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("read the answer of POST %s: %v", url, err)
	}
	var got grantAnswer
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode the %d answer of POST %s: %v", resp.StatusCode, url, err)
	}
	return resp.StatusCode, got
}

// wantGrant posts body and checks that the answer is a grant for pod that
// verifies against the served key set, as fiberd would verify it.
func wantGrant(t *testing.T, pod *corev1.Pod, token, body string) grantAnswer {
	t.Helper()
	status, got := postGrant(t.Context(), t, token, body)
	if status != http.StatusOK {
		t.Fatalf("POST %s = %d %q, want 200", issuer.GrantPath, status, got.Error)
	}
	if got.UID != string(pod.UID) || got.Audience != pod.Name {
		t.Errorf("the answer names uid %q audience %q, want %q %q", got.UID, got.Audience, pod.UID, pod.Name)
	}
	if !got.ExpiresAt.After(got.IssuedAt) {
		t.Errorf("the lease ends at %s, not after it starts at %s", got.ExpiresAt, got.IssuedAt)
	}

	var set jose.JSONWebKeySet
	getIssuerJSON(t, issuer.JWKSPath, &set)
	if len(set.Keys) == 0 {
		t.Fatal("the issuer serves no key")
	}
	g, err := grant.Verify(got.Token, &set.Keys[0], grant.VerifyOptions{Audience: pod.Name, Issuer: issuerURL})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if g.UID != string(pod.UID) || g.Tenant != pod.Namespace {
		t.Errorf("the grant names uid %q tenant %q, want %q %q", g.UID, g.Tenant, pod.UID, pod.Namespace)
	}
	return got
}

// wantRefusal posts as token and checks the refusal.
func wantRefusal(t *testing.T, token string, wantStatus int, wantReason string) {
	t.Helper()
	status, got := postGrant(t.Context(), t, token, `{}`)
	if status != wantStatus || got.Error != wantReason {
		t.Fatalf("POST %s = %d %q, want %d %q", issuer.GrantPath, status, got.Error, wantStatus, wantReason)
	}
	if got.Token != "" {
		t.Error("a refusal carries a token")
	}
}
