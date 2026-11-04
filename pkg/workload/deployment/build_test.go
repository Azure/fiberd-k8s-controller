package deployment

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload"
)

var rollingUpdate = appsv1.DeploymentStrategy{
	Type: appsv1.RollingUpdateDeploymentStrategyType,
	RollingUpdate: &appsv1.RollingUpdateDeployment{
		MaxSurge:       ptr.To(intstr.FromInt32(1)),
		MaxUnavailable: ptr.To(intstr.FromInt32(0)),
	},
}

var recreate = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}

func TestBuild(t *testing.T) {
	fiberdLabels := map[string]string{
		v1alpha1.LabelGrant:     "web",
		v1alpha1.LabelGrantUID:  testUID,
		v1alpha1.LabelManagedBy: v1alpha1.ManagedBy,
	}
	selector := map[string]string{
		v1alpha1.LabelGrant:    "web",
		v1alpha1.LabelGrantUID: testUID,
	}
	withLabels := func(l map[string]string) func(*v1alpha1.CapacityGrant) {
		return func(cg *v1alpha1.CapacityGrant) { cg.Labels = l }
	}
	withClaim := func(cg *v1alpha1.CapacityGrant) {
		cg.Spec.PodTemplate.Spec.ResourceClaims = []corev1.PodResourceClaim{{Name: "gpu", ResourceClaimName: ptr.To("gpu-0")}}
	}

	// Each case's mods change the grant Build gets. The template is always
	// built from the unchanged grant, since only the grant decides the
	// strategy and the object labels.
	cases := []struct {
		name         string
		mods         []func(*v1alpha1.CapacityGrant)
		tmpl         func(*corev1.PodTemplateSpec)
		scheme       func(*testing.T) *runtime.Scheme
		nilGrant     bool
		wantLabels   map[string]string
		wantStrategy appsv1.DeploymentStrategy
		wantErr      bool
	}{
		{
			name:         "a grant without a device gets a rolling update",
			wantLabels:   fiberdLabels,
			wantStrategy: rollingUpdate,
		},
		{
			name:         "an extended resource makes the strategy Recreate",
			mods:         []func(*v1alpha1.CapacityGrant){withGPU},
			wantLabels:   fiberdLabels,
			wantStrategy: recreate,
		},
		{
			name:         "a resource claim makes the strategy Recreate",
			mods:         []func(*v1alpha1.CapacityGrant){withClaim},
			wantLabels:   fiberdLabels,
			wantStrategy: recreate,
		},
		{
			name: "Kueue labels are copied, other grant labels are not",
			mods: []func(*v1alpha1.CapacityGrant){withLabels(map[string]string{
				"kueue.x-k8s.io/queue-name":     "team-a-queue",
				"kueue.x-k8s.io/priority-class": "high",
				"team":                          "a",
			})},
			wantLabels: map[string]string{
				v1alpha1.LabelGrant:             "web",
				v1alpha1.LabelGrantUID:          testUID,
				v1alpha1.LabelManagedBy:         v1alpha1.ManagedBy,
				"kueue.x-k8s.io/queue-name":     "team-a-queue",
				"kueue.x-k8s.io/priority-class": "high",
			},
			wantStrategy: rollingUpdate,
		},
		{
			name:         "a grant label cannot replace a fiberd label",
			mods:         []func(*v1alpha1.CapacityGrant){withLabels(map[string]string{v1alpha1.LabelGrantUID: "forged"})},
			wantLabels:   fiberdLabels,
			wantStrategy: rollingUpdate,
		},
		{
			name:     "a nil grant is refused",
			nilGrant: true,
			wantErr:  true,
		},
		{
			name:    "template labels that miss the selector are refused",
			tmpl:    func(p *corev1.PodTemplateSpec) { delete(p.Labels, v1alpha1.LabelGrantUID) },
			wantErr: true,
		},
		{
			name:    "a scheme without the CapacityGrant kind is refused",
			scheme:  func(*testing.T) *runtime.Scheme { return runtime.NewScheme() },
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := testTemplate(t, testGrant())
			if tc.tmpl != nil {
				tc.tmpl(&tmpl)
			}
			tmplBefore := tmpl.DeepCopy()
			var cg *v1alpha1.CapacityGrant
			if !tc.nilGrant {
				cg = testGrant(tc.mods...)
			}
			cgBefore := cg.DeepCopy()
			scheme := testScheme
			if tc.scheme != nil {
				scheme = tc.scheme
			}

			d, err := Build(cg, tmpl, scheme(t))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Build = %v, want an error", d)
				}
				return
			}
			if err != nil {
				t.Fatalf("Build: %v", err)
			}

			want := &appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
				ObjectMeta: metav1.ObjectMeta{
					Name:            "fiberd-web",
					Namespace:       "team-a",
					Labels:          tc.wantLabels,
					OwnerReferences: []metav1.OwnerReference{controllerRef(testUID)},
				},
				Spec: appsv1.DeploymentSpec{
					Replicas:             ptr.To[int32](1),
					Selector:             &metav1.LabelSelector{MatchLabels: selector},
					Template:             *tmplBefore,
					Strategy:             tc.wantStrategy,
					RevisionHistoryLimit: ptr.To[int32](1),
				},
			}
			if diff := cmp.Diff(want, d); diff != "" {
				t.Errorf("Build mismatch (-want +got):\n%s", diff)
			}
			for _, k := range kueueLabels {
				if _, ok := d.Spec.Template.Labels[k]; ok {
					t.Errorf("the Pod template carries the Kueue label %s", k)
				}
			}
			if diff := cmp.Diff(cgBefore, cg); diff != "" {
				t.Errorf("Build changed the grant (-before +after):\n%s", diff)
			}
			if diff := cmp.Diff(*tmplBefore, tmpl); diff != "" {
				t.Errorf("Build changed the template (-before +after):\n%s", diff)
			}
		})
	}
}

// TestApplyBody checks the JSON each object is applied with. It decodes
// back to the typed object, it holds no status and no creationTimestamp,
// which would claim fields the controller never sets, and a Recreate body
// holds no rollingUpdate, so a strategy flip drops it.
func TestApplyBody(t *testing.T) {
	deploymentBody := func(cg *v1alpha1.CapacityGrant) func(*testing.T) ([]byte, runtime.Object) {
		return func(t *testing.T) ([]byte, runtime.Object) {
			d, err := Build(cg, testTemplate(t, testGrant()), testScheme(t))
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			ac, err := deploymentApplyConfiguration(d)
			if err != nil {
				t.Fatalf("deploymentApplyConfiguration: %v", err)
			}
			return mustMarshal(t, ac), d
		}
	}
	serviceBody := func(t *testing.T) ([]byte, runtime.Object) {
		svc, err := workload.BuildService(testGrant(), testScheme(t))
		if err != nil {
			t.Fatalf("BuildService: %v", err)
		}
		ac, err := serviceApplyConfiguration(svc)
		if err != nil {
			t.Fatalf("serviceApplyConfiguration: %v", err)
		}
		return mustMarshal(t, ac), svc
	}

	cases := []struct {
		name       string
		body       func(*testing.T) ([]byte, runtime.Object)
		into       func() runtime.Object
		wantAbsent []string
		wantKeys   []string
	}{
		{
			name:       "a rolling update Deployment body carries no status",
			body:       deploymentBody(testGrant()),
			into:       func() runtime.Object { return &appsv1.Deployment{} },
			wantAbsent: []string{"status", "creationTimestamp"},
			wantKeys:   []string{"rollingUpdate", "ownerReferences", "selector"},
		},
		{
			name:       "a Recreate Deployment body carries no rollingUpdate",
			body:       deploymentBody(testGrant(withGPU)),
			into:       func() runtime.Object { return &appsv1.Deployment{} },
			wantAbsent: []string{"status", "creationTimestamp", "rollingUpdate"},
			wantKeys:   []string{"strategy"},
		},
		{
			name:       "a Service body carries no status or clusterIP",
			body:       serviceBody,
			into:       func() runtime.Object { return &corev1.Service{} },
			wantAbsent: []string{"status", "creationTimestamp", "clusterIP"},
			wantKeys:   []string{"ports", "ownerReferences", "selector"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, typed := tc.body(t)

			got := tc.into()
			if err := json.Unmarshal(data, got); err != nil {
				t.Fatalf("decode the apply body: %v", err)
			}
			if diff := cmp.Diff(typed, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("the apply body does not decode to the built object (-built +decoded):\n%s", diff)
			}

			var tree any
			if err := json.Unmarshal(data, &tree); err != nil {
				t.Fatalf("decode the apply body: %v", err)
			}
			for _, k := range tc.wantAbsent {
				if hasKey(tree, k) {
					t.Errorf("the apply body holds %q:\n%s", k, data)
				}
			}
			for _, k := range tc.wantKeys {
				if !hasKey(tree, k) {
					t.Errorf("the apply body lacks %q:\n%s", k, data)
				}
			}
		})
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return data
}

// hasKey reports whether key names a field anywhere in the decoded JSON v.
func hasKey(v any, key string) bool {
	switch v := v.(type) {
	case map[string]any:
		for k, child := range v {
			if k == key || hasKey(child, key) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if hasKey(child, key) {
				return true
			}
		}
	}
	return false
}
