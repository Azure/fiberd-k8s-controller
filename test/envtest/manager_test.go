//go:build envtest

package envtest_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/fiberd/pkg/grant"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/cmd/fiberd-k8s-controller/app"
	"github.com/Azure/fiberd-k8s-controller/pkg/issuer"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload/podtemplate"
	"github.com/Azure/fiberd-k8s-controller/test/envtest/internal/testutil"
)

const (
	testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testImage  = "ghcr.io/azure/fiberd:envtest"

	// restartedAt is the template annotation kubectl rollout restart sets,
	// under its own field manager.
	restartedAt       = "kubectl.kubernetes.io/restartedAt"
	rolloutFieldOwner = "kubectl-rollout"
	restartedAtValue  = "2026-11-03T10:00:00Z"

	// quietPeriod is how long a steady grant must leave its objects alone.
	quietPeriod = 2 * time.Second

	// issuerRequestTimeout is short of testutil.PollTimeout, so a request
	// that hangs is retried within the wait.
	issuerRequestTimeout = 5 * time.Second
)

// reasonNewRSAvailable is the Deployment controller's Progressing reason
// once a rollout is done. The grant's Progressing condition carries it over.
const reasonNewRSAvailable = "NewReplicaSetAvailable"

// TestManager runs every case against the one manager TestMain starts. A
// case that needs a tenant namespace creates its own.
func TestManager(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{name: "one replica holds the leader-election Lease", run: testLease},
		{name: "healthz, readyz and each named readyz check answer 200 once the manager has started", run: testProbes},
		{name: "discovery names the issuer URL and the JWKS under it over HTTPS", run: testDiscovery},
		{name: "the JWKS serves exactly the keys of the key Secret the controller created", run: testKeySet},
		{name: "a new grant gets a Service and a Deployment it controls, written only by server-side apply", run: testCreated},
		{name: "a spec change is applied by server-side apply and changes the grant hash", run: testSpecChange},
		{name: "a restartedAt annotation from another field manager survives an apply", run: testRestartedAt},
		{name: "a deleted Service is created again", run: testServiceReturns},
		{name: "a foreign Deployment of the same name is left alone and the grant reports DeploymentConflict", run: testForeignDeployment},
		{name: "a foreign Service of the same name is left alone and the grant reports ServiceConflict", run: testForeignService},
		{name: "a grant in a namespace without the tenant RoleBinding reports Forbidden and records a Warning Event", run: testForbidden},
		{name: "a pass after a Deployment status change writes neither object", run: testSecondPass},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.run)
	}
}

func testLease(t *testing.T) {
	key := client.ObjectKey{Namespace: controllerNamespace, Name: app.LeaderElectionID}
	testutil.Eventually(t, "a holder of Lease "+key.String(), func(ctx context.Context) (bool, error) {
		lease := &coordinationv1.Lease{}
		if err := admin.Get(ctx, key, lease); err != nil {
			return false, err
		}
		return ptr.Deref(lease.Spec.HolderIdentity, "") != "", nil
	})
}

// testProbes checks the probe server on the free port controllerOptions
// picked. /healthz is a ping. /readyz answers 200 only once the informers
// of every watched kind have synced and the issuer serves. The aggregate
// passes with a check missing, so each readyz check is asked by name. A
// name that is not registered answers 404.
func testProbes(t *testing.T) {
	for _, path := range []string{"/healthz", "/readyz", "/readyz/cache-sync", "/readyz/issuer"} {
		waitForProbe(t, path)
	}
}

// waitForProbe waits for path on the probe server to answer 200.
func waitForProbe(t *testing.T, path string) {
	t.Helper()
	url := "http://" + probeAddr + path
	testutil.Eventually(t, url+" to answer 200", func(ctx context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false, testutil.NotYet(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false, testutil.NotYet(fmt.Errorf("GET %s = %d", url, resp.StatusCode))
		}
		return true, nil
	})
}

func testDiscovery(t *testing.T) {
	var got struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	getIssuerJSON(t, issuer.DiscoveryPath, &got)
	if got.Issuer != issuerURL {
		t.Errorf("discovery issuer = %q, want %q", got.Issuer, issuerURL)
	}
	if want := issuerURL + issuer.JWKSPath; got.JWKSURI != want {
		t.Errorf("discovery jwks_uri = %q, want %q", got.JWKSURI, want)
	}
}

// testKeySet proves the generated Role lets the controller user create and
// read the key Secret, since the manager ran as that user.
func testKeySet(t *testing.T) {
	key := client.ObjectKey{Namespace: controllerNamespace, Name: issuer.KeySecretName}
	secret := &corev1.Secret{}
	if err := admin.Get(t.Context(), key, secret); err != nil {
		t.Fatalf("get Secret %s: %v", key, err)
	}
	if got := secret.Labels[v1alpha1.LabelManagedBy]; got != v1alpha1.ManagedBy {
		t.Errorf("Secret %s label %s = %q, want %q", key, v1alpha1.LabelManagedBy, got, v1alpha1.ManagedBy)
	}
	ring, err := grant.DecodeKeyRing(secret.Data[issuer.KeySecretField])
	if err != nil {
		t.Fatalf("decode Secret %s field %s: %v", key, issuer.KeySecretField, err)
	}

	var set jose.JSONWebKeySet
	getIssuerJSON(t, issuer.JWKSPath, &set)
	if diff := cmp.Diff(keyIDs(ring.Public()), keyIDs(set)); diff != "" {
		t.Errorf("JWKS key ids mismatch (-want +got):\n%s", diff)
	}
}

func testCreated(t *testing.T) {
	ns := tenantNamespace(t, true)
	cg := createGrant(t, ns, "created")
	dep, svc := waitForObjects(t, cg)
	for _, obj := range []client.Object{dep, svc} {
		checkControllerRef(t, obj, cg)
		checkAppliedOnly(t, obj)
	}
	waitForCondition(t, cg, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonUnavailable)
}

func testSpecChange(t *testing.T) {
	ns := tenantNamespace(t, true)
	cg := createGrant(t, ns, "spec-change")
	dep, _ := waitForObjects(t, cg)
	oldHash := dep.Spec.Template.Annotations[podtemplate.AnnotationGrantHash]
	if oldHash == "" {
		t.Fatalf("Deployment %s has no %s annotation", dep.Name, podtemplate.AnnotationGrantHash)
	}

	patchGrant(t, cg, func(cg *v1alpha1.CapacityGrant) { cg.Spec.Fibers.Max = 16 })
	dep = waitForHashChange(t, cg, oldHash)
	if dep.Generation < 2 {
		t.Errorf("Deployment generation = %d after a spec change, want at least 2", dep.Generation)
	}
	checkAppliedOnly(t, dep)
}

func testRestartedAt(t *testing.T) {
	ns := tenantNamespace(t, true)
	cg := createGrant(t, ns, "restarted")
	dep, _ := waitForObjects(t, cg)
	oldHash := dep.Spec.Template.Annotations[podtemplate.AnnotationGrantHash]

	before := dep.DeepCopy()
	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = map[string]string{}
	}
	dep.Spec.Template.Annotations[restartedAt] = restartedAtValue
	if err := admin.Patch(t.Context(), dep, client.MergeFrom(before), client.FieldOwner(rolloutFieldOwner)); err != nil {
		t.Fatalf("set %s on Deployment %s: %v", restartedAt, dep.Name, err)
	}

	// A spec change makes the controller apply a new template, so the
	// annotation must survive a real apply and not only a no-op.
	patchGrant(t, cg, func(cg *v1alpha1.CapacityGrant) { cg.Spec.Fibers.Max = 16 })
	dep = waitForHashChange(t, cg, oldHash)
	if got := dep.Spec.Template.Annotations[restartedAt]; got != restartedAtValue {
		t.Errorf("template annotation %s = %q after the apply, want %q", restartedAt, got, restartedAtValue)
	}
}

func testServiceReturns(t *testing.T) {
	ns := tenantNamespace(t, true)
	cg := createGrant(t, ns, "service-returns")
	_, svc := waitForObjects(t, cg)
	oldUID := svc.UID
	if err := admin.Delete(t.Context(), svc); err != nil {
		t.Fatalf("delete Service %s: %v", svc.Name, err)
	}
	key := client.ObjectKeyFromObject(svc)
	got := &corev1.Service{}
	testutil.Eventually(t, "Service "+key.String()+" to come back", func(ctx context.Context) (bool, error) {
		if err := admin.Get(ctx, key, got); err != nil {
			return false, err
		}
		return got.UID != oldUID, nil
	})
	checkControllerRef(t, got, cg)
}

func testForeignDeployment(t *testing.T) {
	ns := tenantNamespace(t, true)
	const name = "foreign"
	labels := map[string]string{"app": "foreign"}
	foreign := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: workload.ObjectName(newGrant(ns, name))},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "app",
					Image: "registry.k8s.io/pause:3.10",
				}}},
			},
		},
	}
	if err := admin.Create(t.Context(), foreign); err != nil {
		t.Fatalf("create the foreign Deployment: %v", err)
	}

	cg := createGrant(t, ns, name)
	waitForCondition(t, cg, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonDeploymentConflict)
	waitForWarningEvent(t, cg, v1alpha1.ReasonDeploymentConflict)

	got := &appsv1.Deployment{}
	if err := admin.Get(t.Context(), client.ObjectKeyFromObject(foreign), got); err != nil {
		t.Fatal(err)
	}
	if got.ResourceVersion != foreign.ResourceVersion {
		t.Errorf("foreign Deployment resourceVersion = %s, want %s untouched", got.ResourceVersion, foreign.ResourceVersion)
	}
	if len(got.OwnerReferences) != 0 {
		t.Errorf("foreign Deployment gained owner references %v", got.OwnerReferences)
	}
}

func testForeignService(t *testing.T) {
	ns := tenantNamespace(t, true)
	const name = "foreign-service"
	foreign := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: workload.ObjectName(newGrant(ns, name))},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "foreign"},
			Ports:    []corev1.ServicePort{{Name: "http", Port: 80}},
		},
	}
	if err := admin.Create(t.Context(), foreign); err != nil {
		t.Fatalf("create the foreign Service: %v", err)
	}

	cg := createGrant(t, ns, name)
	waitForCondition(t, cg, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonServiceConflict)
	waitForWarningEvent(t, cg, v1alpha1.ReasonServiceConflict)

	got := &corev1.Service{}
	if err := admin.Get(t.Context(), client.ObjectKeyFromObject(foreign), got); err != nil {
		t.Fatal(err)
	}
	if got.ResourceVersion != foreign.ResourceVersion {
		t.Errorf("foreign Service resourceVersion = %s, want %s untouched", got.ResourceVersion, foreign.ResourceVersion)
	}
	if len(got.OwnerReferences) != 0 {
		t.Errorf("foreign Service gained owner references %v", got.OwnerReferences)
	}
	err := admin.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: workload.ObjectName(cg)}, &appsv1.Deployment{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("get Deployment beside a foreign Service = %v, want NotFound", err)
	}
}

func testForbidden(t *testing.T) {
	ns := tenantNamespace(t, false)
	cg := createGrant(t, ns, "forbidden")
	waitForCondition(t, cg, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonForbidden)
	waitForWarningEvent(t, cg, v1alpha1.ReasonForbidden)

	err := admin.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: workload.ObjectName(cg)}, &appsv1.Deployment{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("get Deployment in a namespace without the tenant RoleBinding = %v, want NotFound", err)
	}
}

// testSecondPass writes Deployment status by hand, which wakes the
// reconciler through its Deployment watch. The grant turning Ready proves
// that pass ran, and it must not have written either object.
func testSecondPass(t *testing.T) {
	ns := tenantNamespace(t, true)
	cg := createGrant(t, ns, "second-pass")
	dep, svc := waitForObjects(t, cg)
	waitForCondition(t, cg, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonUnavailable)

	if err := admin.Get(t.Context(), client.ObjectKeyFromObject(dep), dep); err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	dep.Status = appsv1.DeploymentStatus{
		ObservedGeneration: dep.Generation,
		Replicas:           1,
		UpdatedReplicas:    1,
		ReadyReplicas:      1,
		AvailableReplicas:  1,
		Conditions: []appsv1.DeploymentCondition{
			{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue, Reason: "MinimumReplicasAvailable", LastUpdateTime: now, LastTransitionTime: now},
			{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: reasonNewRSAvailable, LastUpdateTime: now, LastTransitionTime: now},
		},
	}
	if err := admin.Status().Update(t.Context(), dep); err != nil {
		t.Fatalf("write Deployment %s status: %v", dep.Name, err)
	}

	waitForCondition(t, cg, v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonAvailable)
	waitForCondition(t, cg, v1alpha1.ConditionProgressing, metav1.ConditionTrue, reasonNewRSAvailable)

	want := map[client.Object]string{dep: dep.ResourceVersion, svc: svc.ResourceVersion}
	deadline := time.Now().Add(quietPeriod)
	for {
		for obj, rv := range want {
			got := obj.DeepCopyObject().(client.Object)
			if err := admin.Get(t.Context(), client.ObjectKeyFromObject(obj), got); err != nil {
				t.Fatal(err)
			}
			if got.GetResourceVersion() != rv {
				t.Fatalf("%T %s resourceVersion = %s after a pass with nothing to change, want %s", obj, obj.GetName(), got.GetResourceVersion(), rv)
			}
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(testutil.PollInterval)
	}
}

// newGrant is the smallest grant the controller builds a Pod for.
func newGrant(ns, name string) *v1alpha1.CapacityGrant {
	return &v1alpha1.CapacityGrant{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: v1alpha1.CapacityGrantSpec{
			Zygote:  v1alpha1.ZygoteSpec{Digest: testDigest, Command: []string{"/usr/local/bin/refzygote"}},
			Fibers:  v1alpha1.FiberBudget{Max: 8},
			Runtime: v1alpha1.RuntimeProc,
			PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name:  podtemplate.ContainerName,
				Image: testImage,
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				}},
			}}}},
		},
	}
}

// createGrant creates newGrant(ns, name) and returns it with its UID.
func createGrant(t *testing.T, ns, name string) *v1alpha1.CapacityGrant {
	t.Helper()
	cg := newGrant(ns, name)
	if err := admin.Create(t.Context(), cg); err != nil {
		t.Fatalf("create CapacityGrant %s/%s: %v", ns, name, err)
	}
	return cg
}

// patchGrant applies edit to the stored grant with a merge patch.
func patchGrant(t *testing.T, cg *v1alpha1.CapacityGrant, edit func(*v1alpha1.CapacityGrant)) {
	t.Helper()
	if err := admin.Get(t.Context(), client.ObjectKeyFromObject(cg), cg); err != nil {
		t.Fatal(err)
	}
	before := cg.DeepCopy()
	edit(cg)
	if err := admin.Patch(t.Context(), cg, client.MergeFrom(before)); err != nil {
		t.Fatalf("patch CapacityGrant %s: %v", cg.Name, err)
	}
}

// waitForObjects waits for the grant's Service and Deployment.
func waitForObjects(t *testing.T, cg *v1alpha1.CapacityGrant) (*appsv1.Deployment, *corev1.Service) {
	t.Helper()
	key := client.ObjectKey{Namespace: cg.Namespace, Name: workload.ObjectName(cg)}
	dep := &appsv1.Deployment{}
	svc := &corev1.Service{}
	testutil.Eventually(t, "Deployment and Service "+key.String(), func(ctx context.Context) (bool, error) {
		if err := admin.Get(ctx, key, dep); err != nil {
			return false, err
		}
		return true, admin.Get(ctx, key, svc)
	})
	return dep, svc
}

// waitForHashChange waits for the Deployment template's grant hash to
// leave old.
func waitForHashChange(t *testing.T, cg *v1alpha1.CapacityGrant, old string) *appsv1.Deployment {
	t.Helper()
	key := client.ObjectKey{Namespace: cg.Namespace, Name: workload.ObjectName(cg)}
	dep := &appsv1.Deployment{}
	testutil.Eventually(t, "a new grant hash on Deployment "+key.String(), func(ctx context.Context) (bool, error) {
		if err := admin.Get(ctx, key, dep); err != nil {
			return false, err
		}
		hash := dep.Spec.Template.Annotations[podtemplate.AnnotationGrantHash]
		return hash != "" && hash != old, nil
	})
	return dep
}

// waitForCondition waits for a condition of the grant's current
// generation with the given status and reason.
func waitForCondition(t *testing.T, cg *v1alpha1.CapacityGrant, typ string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	key := client.ObjectKeyFromObject(cg)
	got := &v1alpha1.CapacityGrant{}
	testutil.Eventually(t, "condition "+typ+"="+string(status)+" "+reason+" on CapacityGrant "+key.String(), func(ctx context.Context) (bool, error) {
		if err := admin.Get(ctx, key, got); err != nil {
			return false, err
		}
		c := meta.FindStatusCondition(got.Status.Conditions, typ)
		return c != nil && c.Status == status && c.Reason == reason && c.ObservedGeneration == got.Generation, nil
	})
}

// waitForWarningEvent waits for an events.k8s.io Warning Event about cg
// with reason, reported by the controller.
func waitForWarningEvent(t *testing.T, cg *v1alpha1.CapacityGrant, reason string) {
	t.Helper()
	testutil.Eventually(t, fmt.Sprintf("a Warning Event %s on CapacityGrant %s/%s", reason, cg.Namespace, cg.Name), func(ctx context.Context) (bool, error) {
		list := &eventsv1.EventList{}
		if err := admin.List(ctx, list, client.InNamespace(cg.Namespace)); err != nil {
			return false, err
		}
		for _, ev := range list.Items {
			if ev.Regarding.UID == cg.UID && ev.Type == corev1.EventTypeWarning && ev.Reason == reason && ev.ReportingController == app.EventSource {
				return true, nil
			}
		}
		return false, nil
	})
}

// checkControllerRef checks that obj names cg as its controller and blocks
// cg's deletion until the garbage collector removes it.
func checkControllerRef(t *testing.T, obj client.Object, cg *v1alpha1.CapacityGrant) {
	t.Helper()
	ref := metav1.GetControllerOf(obj)
	if ref == nil {
		t.Fatalf("%T %s has no controller reference", obj, obj.GetName())
	}
	want := metav1.OwnerReference{
		APIVersion:         v1alpha1.GroupVersion.String(),
		Kind:               "CapacityGrant",
		Name:               cg.Name,
		UID:                cg.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}
	if diff := cmp.Diff(want, *ref); diff != "" {
		t.Errorf("%T %s controller reference mismatch (-want +got):\n%s", obj, obj.GetName(), diff)
	}
}

// checkAppliedOnly checks that the controller wrote obj only by
// server-side apply under its own field manager.
func checkAppliedOnly(t *testing.T, obj client.Object) {
	t.Helper()
	applied := false
	for _, mf := range obj.GetManagedFields() {
		if mf.Manager != v1alpha1.ManagedBy {
			continue
		}
		if mf.Operation != metav1.ManagedFieldsOperationApply {
			t.Errorf("%T %s has a %s entry from %s, want only Apply", obj, obj.GetName(), mf.Operation, mf.Manager)
			continue
		}
		applied = true
	}
	if !applied {
		t.Errorf("%T %s has no Apply entry from %s in %v", obj, obj.GetName(), v1alpha1.ManagedBy, obj.GetManagedFields())
	}
}

// getIssuerJSON gets path from the issuer listener over HTTPS and decodes
// the body of a 200 answer into v. The listener is bound before the
// manager starts, but nothing waits for it to serve before the cases run,
// so a request that fails to complete is retried.
func getIssuerJSON(t *testing.T, path string, v any) {
	t.Helper()
	c := issuerClient(t)
	url := "https://" + issuerAddr + path
	var body []byte
	testutil.Eventually(t, url+" to answer", func(ctx context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false, err
		}
		resp, err := c.Do(req)
		if err != nil {
			return false, testutil.NotYet(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return false, testutil.NotYet(err)
		}
		if resp.StatusCode != http.StatusOK {
			return false, fmt.Errorf("GET %s = %d, want 200, body %q", url, resp.StatusCode, body)
		}
		return true, nil
	})
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("decode %s %q: %v", url, body, err)
	}
}

// issuerClient verifies the issuer listener as a grant Pod would. The
// certificate names issuerHost, which does not resolve here, so a URL dials
// the loopback address and ServerName carries the name to verify.
func issuerClient(t *testing.T) *http.Client {
	t.Helper()
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: issuerRoots, ServerName: issuerHost}}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: issuerRequestTimeout}
}

// keyIDs lists the key ids of set in order.
func keyIDs(set jose.JSONWebKeySet) []string {
	ids := make([]string, 0, len(set.Keys))
	for _, k := range set.Keys {
		ids = append(ids, k.KeyID)
	}
	return ids
}
