package deployment

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload/podtemplate"
)

const (
	testUID   = "3f2a6c1e-8b9d-4e5f-a1b2-c3d4e5f6a7b8"
	otherUID  = "9e8d7c6b-5a4f-4e3d-b2c1-a0f9e8d7c6b5"
	testImage = "ghcr.io/azure/fiberd:v0.4.0"
)

// testScheme knows the built-in kinds and the CapacityGrant kind.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add the client-go scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add the v1alpha1 scheme: %v", err)
	}
	return s
}

// testGrant is a minimal valid grant. Each mod changes it in place.
func testGrant(mods ...func(*v1alpha1.CapacityGrant)) *v1alpha1.CapacityGrant {
	cg := &v1alpha1.CapacityGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "team-a", UID: testUID, Generation: 1},
		Spec: v1alpha1.CapacityGrantSpec{
			Zygote: v1alpha1.ZygoteSpec{
				Digest:  "sha256:" + strings.Repeat("ab", 32),
				Command: []string{"/app/zygote", "--port=8080"},
			},
			Fibers:  v1alpha1.FiberBudget{Max: 4},
			Runtime: v1alpha1.RuntimeProc,
			PodTemplate: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: podtemplate.ContainerName,
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

// withGPU makes the grant's template request an extended resource.
func withGPU(cg *v1alpha1.CapacityGrant) {
	cg.Spec.PodTemplate.Spec.Containers[0].Resources.Limits["nvidia.com/gpu"] = resource.MustParse("1")
}

// testTemplate is what podtemplate.Build returns for cg.
func testTemplate(t *testing.T, cg *v1alpha1.CapacityGrant) corev1.PodTemplateSpec {
	t.Helper()
	tmpl, err := podtemplate.Build(cg, testImage, podtemplate.Options{
		IssuerURL: "https://fiberd-k8s-controller.fiberd-system.svc",
		Lease:     10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("podtemplate.Build: %v", err)
	}
	return tmpl
}

// controllerRef is the controller reference cg's objects carry.
func controllerRef(uid string) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         v1alpha1.GroupVersion.String(),
		Kind:               "CapacityGrant",
		Name:               "web",
		UID:                types.UID(uid),
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}
}
