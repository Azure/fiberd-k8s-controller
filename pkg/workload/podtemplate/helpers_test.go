package podtemplate

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

const (
	testIssuer = "https://fiberd-k8s-controller.fiberd-system.svc"
	testImage  = "ghcr.io/azure/fiberd:v0.4.0"
	testUID    = "3f2a6c1e-8b9d-4e5f-a1b2-c3d4e5f6a7b8"
	testLease  = 10 * time.Minute

	// testServiceName is workload.ObjectName(testGrant()). This package cannot import pkg/workload, which imports it.
	testServiceName = "fiberd-web"

	// testHelperImage is the helper image of a hyperlight grant.
	testHelperImage = "ghcr.io/azure/fiberd-helper:v0.4.0"
)

var testDigest = "sha256:" + strings.Repeat("ab", 32)

// testOptions are the fewest Options a grant Pod can be built with.
func testOptions() Options {
	return Options{IssuerURL: testIssuer, Lease: testLease}
}

// testGrant is a minimal valid grant. Each mod changes it in place.
func testGrant(mods ...func(*v1alpha1.CapacityGrant)) *v1alpha1.CapacityGrant {
	cg := &v1alpha1.CapacityGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "team-a", UID: testUID},
		Spec: v1alpha1.CapacityGrantSpec{
			Zygote:  v1alpha1.ZygoteSpec{Digest: testDigest, Command: []string{"/app/zygote", "--port=8080"}},
			Fibers:  v1alpha1.FiberBudget{Max: 4},
			Runtime: v1alpha1.RuntimeProc,
			PodTemplate: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: ContainerName,
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
					},
				}}},
			},
		},
	}
	for _, mod := range mods {
		mod(cg)
	}
	return cg
}

// setRuntime sets the grant's runtime, with the helper image that runtime
// hyperlight needs.
func setRuntime(cg *v1alpha1.CapacityGrant, rt v1alpha1.Runtime) {
	cg.Spec.Runtime = rt
	if rt == v1alpha1.RuntimeHyperlight {
		cg.Spec.HelperImage = testHelperImage
	}
}

// container returns the grant template's fiberd container, for a mod.
func container(cg *v1alpha1.CapacityGrant) *corev1.Container {
	return &cg.Spec.PodTemplate.Spec.Containers[0]
}

// mustBuild builds cg with opts and fails the test on an error.
func mustBuild(t *testing.T, cg *v1alpha1.CapacityGrant, opts Options) corev1.PodTemplateSpec {
	t.Helper()
	tmpl, err := Build(cg, testImage, testServiceName, opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if n := len(tmpl.Spec.Containers); n != 1 {
		t.Fatalf("Build returned %d containers, want 1", n)
	}
	return tmpl
}

// mustHash is Hash of cg and image, failing the test on an error.
func mustHash(t *testing.T, cg *v1alpha1.CapacityGrant, image string) string {
	t.Helper()
	h, err := Hash(cg, image)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	return h
}
