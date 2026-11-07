//go:build envtest

package envtest_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/go-jose/go-jose/v4"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/fiberd/pkg/core"
	"github.com/Azure/fiberd/pkg/grant"
	"github.com/Azure/fiberd/pkg/tlsconf/tlsconftest"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/pkg/issuer"
	"github.com/Azure/fiberd-k8s-controller/pkg/testutil"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload/podtemplate"
)

// callerGrant is one grant of the caller route's answer.
type callerGrant struct {
	grantAnswer
	Address string `json:"address"`
}

// callerAnswer is what the caller route answers.
type callerAnswer struct {
	ServerName string        `json:"serverName"`
	Grants     []callerGrant `json:"grants"`
	Error      string        `json:"error"`
}

// TestCallerGrantRoute runs a caller across a rollout of its grant against
// a real API server and the running manager's listener. envtest runs no
// Deployment controller or kubelet, so the test builds the ReplicaSets and
// Pods they would have built and sets the Pods' status.
func TestCallerGrantRoute(t *testing.T) {
	waitForProbe(t, "/readyz")

	caller := fiberdCA.Client(t, "caller")
	ns := tenantNamespace(t, true)
	onboardGrantAccount(t, ns)
	cg := newGrant(ns, "rollout")
	cg.Spec.CallerThumbprint = caller.Thumbprint(t)
	if err := admin.Create(t.Context(), cg); err != nil {
		t.Fatalf("create CapacityGrant: %v", err)
	}
	d, _ := waitForObjects(t, cg)
	oldPod := newReplicaSet(t, d, 1).pod(t, "fiberd-rollout-1-aaaaa", nil)
	running(t, oldPod, "10.244.0.10")
	body := fmt.Sprintf(`{"namespace":%q,"name":%q}`, ns, cg.Name)
	waitForCallerGrants(t, caller, body, oldPod)

	// The rollout: a spec change, a new ReplicaSet and its Pod, while the
	// old Pod still serves.
	oldHash := d.Spec.Template.Annotations[podtemplate.AnnotationGrantHash]
	patchGrant(t, cg, func(cg *v1alpha1.CapacityGrant) { cg.Spec.Fibers.Max = 16 })
	d = waitForHashChange(t, cg, oldHash)
	newPod := newReplicaSet(t, d, 2).pod(t, "fiberd-rollout-2-bbbbb", nil)
	running(t, newPod, "10.244.0.11")
	answer := waitForCallerGrants(t, caller, body, oldPod, newPod)
	verifyCallerGrants(t, answer, cg.Spec.CallerThumbprint, oldPod, newPod)

	t.Run("an audience names one Pod", func(t *testing.T) {
		withAudience := fmt.Sprintf(`{"namespace":%q,"name":%q,"audience":%q}`, ns, cg.Name, newPod.Name)
		waitForCallerGrants(t, caller, withAudience, newPod)
	})

	t.Run("a refusal for a certificate the grant does not name matches one for a grant that does not exist", func(t *testing.T) {
		stranger := fiberdCA.Client(t, "stranger")
		status, notNamed := postCaller(t, &stranger, body)
		missingStatus, missing := postCaller(t, &caller, fmt.Sprintf(`{"namespace":%q,"name":"missing"}`, ns))
		if status != http.StatusForbidden || missingStatus != http.StatusForbidden || notNamed.Error != reasonForbidden || missing.Error != reasonForbidden {
			t.Fatalf("answers %d %q and %d %q, want %d %q for both", status, notNamed.Error, missingStatus, missing.Error, http.StatusForbidden, reasonForbidden)
		}
	})

	t.Run("no client certificate and one from another CA are refused", func(t *testing.T) {
		other := tlsconftest.NewCA(t, "other").Client(t, "caller")
		for name, cert := range map[string]*tlsconftest.Pair{"none": nil, "another CA": &other} {
			if status, got := postCaller(t, cert, body); status != http.StatusUnauthorized || got.Error != reasonUnauthenticated {
				t.Errorf("%s: answer %d %q, want %d %q", name, status, got.Error, http.StatusUnauthorized, reasonUnauthenticated)
			}
		}
	})

	// The old Pod terminates once the new one is ready, and its grant
	// leaves the answer.
	if err := admin.Get(t.Context(), client.ObjectKeyFromObject(oldPod), oldPod); err != nil {
		t.Fatal(err)
	}
	oldPod.Finalizers = append(oldPod.Finalizers, holdFinalizer)
	if err := admin.Update(t.Context(), oldPod); err != nil {
		t.Fatalf("hold Pod %s: %v", oldPod.Name, err)
	}
	t.Cleanup(func() { release(t, oldPod) })
	if err := admin.Delete(t.Context(), oldPod); err != nil {
		t.Fatalf("delete Pod %s: %v", oldPod.Name, err)
	}
	waitForCallerGrants(t, caller, body, newPod)
}

// TestCallerGrantIsolation runs a caller's grant for a proc Pod through the
// manager's own client and role. The GrantPolicy holds proc in every case,
// so the label alone decides.
func TestCallerGrantIsolation(t *testing.T) {
	waitForProbe(t, "/readyz")

	cases := []struct {
		name     string
		labelled bool
		want     core.Isolation
	}{
		{name: "a proc grant in a labelled namespace signs TRUSTED", labelled: true, want: core.Trusted},
		{name: "a proc grant with no label signs UNTRUSTED", want: core.Untrusted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			createGrantPolicy(t, v1alpha1.RuntimeProc)
			caller := fiberdCA.Client(t, "caller")
			ns := tenantNamespace(t, true)
			onboardGrantAccount(t, ns)
			if tc.labelled {
				labelRuntime(t, ns, v1alpha1.RuntimeProc)
			}
			cg := newGrant(ns, "isolation")
			cg.Spec.CallerThumbprint = caller.Thumbprint(t)
			if err := admin.Create(t.Context(), cg); err != nil {
				t.Fatalf("create CapacityGrant: %v", err)
			}
			d, _ := waitForObjects(t, cg)
			pod := newReplicaSet(t, d, 1).pod(t, "fiberd-isolation-1-aaaaa", nil)
			running(t, pod, "10.244.0.20")

			answer := waitForCallerGrants(t, caller, fmt.Sprintf(`{"namespace":%q,"name":%q}`, ns, cg.Name), pod)

			if g := verifyGrant(t, answer.Grants[0].Token, pod.Name); g.Policy.Isolation != tc.want {
				t.Errorf("isolation = %s, want %s", g.Policy.Isolation, tc.want)
			}
		})
	}
}

// running sets the status a kubelet would report for pod once it runs at
// ip.
func running(t *testing.T, pod *corev1.Pod, ip string) {
	t.Helper()
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip, PodIPs: []corev1.PodIP{{IP: ip}}}
	if err := admin.Status().Update(t.Context(), pod); err != nil {
		t.Fatalf("set the status of Pod %s: %v", pod.Name, err)
	}
}

// release lets a held Pod go.
func release(t *testing.T, pod *corev1.Pod) {
	p := &corev1.Pod{}
	if err := admin.Get(context.Background(), client.ObjectKeyFromObject(pod), p); err != nil {
		return
	}
	p.Finalizers = slices.DeleteFunc(p.Finalizers, func(f string) bool { return f == holdFinalizer })
	if err := admin.Update(context.Background(), p); err != nil {
		t.Errorf("release Pod %s: %v", p.Name, err)
	}
}

// postCaller sends body to the caller route, presenting cert when it is
// set.
func postCaller(t *testing.T, cert *tlsconftest.Pair, body string) (int, callerAnswer) {
	t.Helper()
	cfg := &tls.Config{RootCAs: issuerRoots, ServerName: issuerHost}
	if cert != nil {
		cfg.Certificates = []tls.Certificate{cert.TLS()}
	}
	tr := &http.Transport{TLSClientConfig: cfg}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: issuerRequestTimeout}
	url := "https://" + issuerAddr + issuer.CallerGrantPath
	resp, err := c.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("read the answer of POST %s: %v", url, err)
	}
	var got callerAnswer
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode the %d answer %q of POST %s: %v", resp.StatusCode, raw, url, err)
	}
	return resp.StatusCode, got
}

// waitForCallerGrants waits for the caller's answer to hold one grant for
// each of pods and no other, since the manager reads Pods from its cache.
func waitForCallerGrants(t *testing.T, caller tlsconftest.Pair, body string, pods ...*corev1.Pod) callerAnswer {
	t.Helper()
	want := make([]string, 0, len(pods))
	for _, p := range pods {
		want = append(want, p.Name)
	}
	slices.Sort(want)
	var got callerAnswer
	testutil.Eventually(t, fmt.Sprintf("grants for %q", want), func(context.Context) (bool, error) {
		var status int
		status, got = postCaller(t, &caller, body)
		if status != http.StatusOK {
			return false, testutil.NotYet(fmt.Errorf("answer %d %q", status, got.Error))
		}
		var names []string
		for _, g := range got.Grants {
			names = append(names, g.Audience)
		}
		slices.Sort(names)
		return slices.Equal(names, want), nil
	})
	return got
}

// verifyCallerGrants checks each grant against the served key set as the
// Pod it names would: it verifies for that Pod's audience only, carries
// that Pod's UID and the caller's key, and comes with the Pod's address
// and the Service name the Pod's certificate carries.
func verifyCallerGrants(t *testing.T, answer callerAnswer, thumbprint string, pods ...*corev1.Pod) {
	t.Helper()
	var set jose.JSONWebKeySet
	getIssuerJSON(t, issuer.JWKSPath, &set)
	if len(set.Keys) == 0 {
		t.Fatal("the issuer serves no key")
	}
	for _, g := range answer.Grants {
		i := slices.IndexFunc(pods, func(p *corev1.Pod) bool { return p.Name == g.Audience })
		pod := pods[i]
		if want := "fiberd-rollout." + pod.Namespace + ".svc"; answer.ServerName != want {
			t.Errorf("serverName = %q, want %q", answer.ServerName, want)
		}
		if want := pod.Status.PodIP + ":8484"; g.Address != want || g.UID != string(pod.UID) {
			t.Errorf("the grant of %s is for uid %q at %q, want %q at %q", pod.Name, g.UID, g.Address, pod.UID, want)
		}
		v, err := grant.Verify(g.Token, &set.Keys[0], grant.VerifyOptions{Audience: pod.Name, Issuer: issuerURL})
		if err != nil {
			t.Fatalf("Verify the grant of %s: %v", pod.Name, err)
		}
		if v.UID != string(pod.UID) || v.CallerThumbprint != thumbprint || v.Tenant != pod.Namespace {
			t.Errorf("the grant of %s names uid %q caller %q tenant %q, want %q %q %q", pod.Name, v.UID, v.CallerThumbprint, v.Tenant, pod.UID, thumbprint, pod.Namespace)
		}
		for _, other := range pods {
			if other.Name == pod.Name {
				continue
			}
			if _, err := grant.Verify(g.Token, &set.Keys[0], grant.VerifyOptions{Audience: other.Name, Issuer: issuerURL}); err == nil {
				t.Errorf("the grant of %s verifies for %s too", pod.Name, other.Name)
			}
		}
	}
}
