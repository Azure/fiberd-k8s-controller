package podtemplate

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

func TestBuildResources(t *testing.T) {
	q := resource.MustParse
	claims := []corev1.ResourceClaim{{Name: "gpu"}}
	cases := []struct {
		name       string
		in         corev1.ResourceRequirements
		defaultCPU string
		want       corev1.ResourceRequirements
	}{
		{
			name: "memory only gets the default CPU",
			in:   corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: q("1Gi")}},
			want: corev1.ResourceRequirements{
				Limits:   corev1.ResourceList{corev1.ResourceCPU: q("500m"), corev1.ResourceMemory: q("1Gi")},
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("500m"), corev1.ResourceMemory: q("1Gi")},
			},
		},
		{
			name:       "a custom default CPU",
			in:         corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: q("1Gi")}},
			defaultCPU: "2",
			want: corev1.ResourceRequirements{
				Limits:   corev1.ResourceList{corev1.ResourceCPU: q("2"), corev1.ResourceMemory: q("1Gi")},
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("2"), corev1.ResourceMemory: q("1Gi")},
			},
		},
		{
			name: "a tenant CPU limit is kept",
			in:   corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: q("3"), corev1.ResourceMemory: q("2Gi")}},
			want: corev1.ResourceRequirements{
				Limits:   corev1.ResourceList{corev1.ResourceCPU: q("3"), corev1.ResourceMemory: q("2Gi")},
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("3"), corev1.ResourceMemory: q("2Gi")},
			},
		},
		{
			name: "tenant requests are replaced by the limits",
			in: corev1.ResourceRequirements{
				Limits:   corev1.ResourceList{corev1.ResourceCPU: q("1"), corev1.ResourceMemory: q("1Gi")},
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("100m"), corev1.ResourceMemory: q("128Mi")},
			},
			want: corev1.ResourceRequirements{
				Limits:   corev1.ResourceList{corev1.ResourceCPU: q("1"), corev1.ResourceMemory: q("1Gi")},
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("1"), corev1.ResourceMemory: q("1Gi")},
			},
		},
		{
			name: "a request without a limit stays",
			in: corev1.ResourceRequirements{
				Limits:   corev1.ResourceList{corev1.ResourceMemory: q("1Gi")},
				Requests: corev1.ResourceList{corev1.ResourceEphemeralStorage: q("5Gi")},
			},
			want: corev1.ResourceRequirements{
				Limits:   corev1.ResourceList{corev1.ResourceCPU: q("500m"), corev1.ResourceMemory: q("1Gi")},
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("500m"), corev1.ResourceMemory: q("1Gi"), corev1.ResourceEphemeralStorage: q("5Gi")},
			},
		},
		{
			name: "a CPU request without a CPU limit becomes the limit",
			in: corev1.ResourceRequirements{
				Limits:   corev1.ResourceList{corev1.ResourceMemory: q("1Gi")},
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("4")},
			},
			want: corev1.ResourceRequirements{
				Limits:   corev1.ResourceList{corev1.ResourceCPU: q("4"), corev1.ResourceMemory: q("1Gi")},
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("4"), corev1.ResourceMemory: q("1Gi")},
			},
		},
		{
			name:       "a CPU request wins over a custom default CPU",
			in:         corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: q("1Gi")}, Requests: corev1.ResourceList{corev1.ResourceCPU: q("250m")}},
			defaultCPU: "2",
			want: corev1.ResourceRequirements{
				Limits:   corev1.ResourceList{corev1.ResourceCPU: q("250m"), corev1.ResourceMemory: q("1Gi")},
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("250m"), corev1.ResourceMemory: q("1Gi")},
			},
		},
		{
			name: "an extended resource only in requests gets an equal limit",
			in: corev1.ResourceRequirements{
				Limits:   corev1.ResourceList{corev1.ResourceMemory: q("1Gi")},
				Requests: corev1.ResourceList{"example.com/fpga": q("1")},
			},
			want: corev1.ResourceRequirements{
				Limits:   corev1.ResourceList{corev1.ResourceCPU: q("500m"), corev1.ResourceMemory: q("1Gi"), "example.com/fpga": q("1")},
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("500m"), corev1.ResourceMemory: q("1Gi"), "example.com/fpga": q("1")},
			},
		},
		{
			name: "hugepages only in requests get an equal limit",
			in: corev1.ResourceRequirements{
				Limits:   corev1.ResourceList{corev1.ResourceMemory: q("1Gi")},
				Requests: corev1.ResourceList{"hugepages-2Mi": q("64Mi")},
			},
			want: corev1.ResourceRequirements{
				Limits:   corev1.ResourceList{corev1.ResourceCPU: q("500m"), corev1.ResourceMemory: q("1Gi"), "hugepages-2Mi": q("64Mi")},
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("500m"), corev1.ResourceMemory: q("1Gi"), "hugepages-2Mi": q("64Mi")},
			},
		},
		{
			name: "a kubernetes.io request without a limit stays a request",
			in: corev1.ResourceRequirements{
				Limits:   corev1.ResourceList{corev1.ResourceMemory: q("1Gi")},
				Requests: corev1.ResourceList{"kubernetes.io/batch-cpu": q("1")},
			},
			want: corev1.ResourceRequirements{
				Limits:   corev1.ResourceList{corev1.ResourceCPU: q("500m"), corev1.ResourceMemory: q("1Gi")},
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("500m"), corev1.ResourceMemory: q("1Gi"), "kubernetes.io/batch-cpu": q("1")},
			},
		},
		{
			name: "extended resources and claims",
			in: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{corev1.ResourceMemory: q("1Gi"), "nvidia.com/gpu": q("1")},
				Claims: claims,
			},
			want: corev1.ResourceRequirements{
				Limits:   corev1.ResourceList{corev1.ResourceCPU: q("500m"), corev1.ResourceMemory: q("1Gi"), "nvidia.com/gpu": q("1")},
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("500m"), corev1.ResourceMemory: q("1Gi"), "nvidia.com/gpu": q("1")},
				Claims:   claims,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cg := testGrant(func(cg *v1alpha1.CapacityGrant) { container(cg).Resources = tc.in })
			opts := testOptions()
			if tc.defaultCPU != "" {
				opts.DefaultCPU = q(tc.defaultCPU)
			}
			got := mustBuild(t, cg, opts).Spec.Containers[0].Resources
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("resources mismatch (-want +got):\n%s", diff)
			}
			if !isGuaranteed(got) {
				t.Errorf("resources %#v are not Guaranteed", got)
			}
		})
	}
}

// isGuaranteed is the kubelet's rule for one container. CPU and memory
// both have limits, and every request equals its limit.
func isGuaranteed(r corev1.ResourceRequirements) bool {
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		limit, ok := r.Limits[name]
		if !ok {
			return false
		}
		if req, ok := r.Requests[name]; ok && req.Cmp(limit) != 0 {
			return false
		}
	}
	return true
}

func TestNeedsExclusiveDevice(t *testing.T) {
	limits := func(name corev1.ResourceName) func(*v1alpha1.CapacityGrant) {
		return func(cg *v1alpha1.CapacityGrant) { container(cg).Resources.Limits[name] = resource.MustParse("1") }
	}
	requests := func(name corev1.ResourceName) func(*v1alpha1.CapacityGrant) {
		return func(cg *v1alpha1.CapacityGrant) {
			container(cg).Resources.Requests = corev1.ResourceList{name: resource.MustParse("1")}
		}
	}
	cases := []struct {
		name  string
		nilCG bool
		mod   func(*v1alpha1.CapacityGrant)
		want  bool
	}{
		{name: "nil grant", nilCG: true, want: false},
		{name: "cpu and memory only", want: false},
		{name: "ephemeral storage", mod: limits(corev1.ResourceEphemeralStorage), want: false},
		{name: "hugepages", mod: limits("hugepages-2Mi"), want: false},
		{name: "kubernetes.io prefix", mod: limits("kubernetes.io/batteries"), want: false},
		{name: "kubernetes.io subdomain", mod: limits("node.kubernetes.io/thing"), want: false},
		{name: "quota requests form", mod: requests("requests.nvidia.com/gpu"), want: false},
		{name: "extended limit", mod: limits("nvidia.com/gpu"), want: true},
		{name: "extended request", mod: requests("example.com/fpga"), want: true},
		{
			name: "resource claim",
			mod: func(cg *v1alpha1.CapacityGrant) {
				cg.Spec.PodTemplate.Spec.ResourceClaims = []corev1.PodResourceClaim{{Name: "gpu", ResourceClaimName: ptr.To("gpu-0")}}
			},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cg *v1alpha1.CapacityGrant
			if !tc.nilCG {
				cg = testGrant()
				if tc.mod != nil {
					tc.mod(cg)
				}
			}
			if got := NeedsExclusiveDevice(cg); got != tc.want {
				t.Errorf("NeedsExclusiveDevice = %v, want %v", got, tc.want)
			}
		})
	}
}
