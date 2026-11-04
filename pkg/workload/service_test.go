package workload

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

const testUID = "3f2a6c1e-8b9d-4e5f-a1b2-c3d4e5f6a7b8"

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return s
}

func testGrant() *v1alpha1.CapacityGrant {
	return &v1alpha1.CapacityGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web",
			Namespace: "team-a",
			UID:       testUID,
			Labels:    map[string]string{"kueue.x-k8s.io/queue-name": "q"},
		},
	}
}

func TestObjectName(t *testing.T) {
	cases := []struct {
		name  string
		grant string
		want  string
	}{
		{name: "a short grant name gets the prefix", grant: "web", want: "fiberd-web"},
		{name: "a dotted grant name keeps its dots", grant: "web.v2", want: "fiberd-web.v2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cg := &v1alpha1.CapacityGrant{ObjectMeta: metav1.ObjectMeta{Name: tc.grant}}
			if got := ObjectName(cg); got != tc.want {
				t.Errorf("ObjectName = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildService(t *testing.T) {
	want := &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fiberd-web",
			Namespace: "team-a",
			Labels: map[string]string{
				v1alpha1.LabelGrant:     "web",
				v1alpha1.LabelGrantUID:  testUID,
				v1alpha1.LabelManagedBy: v1alpha1.ManagedBy,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         v1alpha1.GroupVersion.String(),
				Kind:               "CapacityGrant",
				Name:               "web",
				UID:                testUID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Selector: map[string]string{
				v1alpha1.LabelGrant:    "web",
				v1alpha1.LabelGrantUID: testUID,
			},
			Ports: []corev1.ServicePort{{
				Name:       "grpc",
				Protocol:   corev1.ProtocolTCP,
				Port:       8484,
				TargetPort: intstr.FromString("grpc"),
			}},
		},
	}

	cases := []struct {
		name    string
		grant   *v1alpha1.CapacityGrant
		scheme  func(*testing.T) *runtime.Scheme
		want    *corev1.Service
		wantErr bool
	}{
		{
			name:   "the Service is ClusterIP on the grpc port without Kueue labels",
			grant:  testGrant(),
			scheme: testScheme,
			want:   want,
		},
		{
			name:    "a nil grant is refused",
			scheme:  testScheme,
			wantErr: true,
		},
		{
			name:    "a scheme without the CapacityGrant kind is refused",
			grant:   testGrant(),
			scheme:  func(*testing.T) *runtime.Scheme { return runtime.NewScheme() },
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var before *v1alpha1.CapacityGrant
			if tc.grant != nil {
				before = tc.grant.DeepCopy()
			}
			got, err := BuildService(tc.grant, tc.scheme(t))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("BuildService = %v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildService: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("BuildService mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(before, tc.grant); diff != "" {
				t.Errorf("BuildService changed the grant (-before +after):\n%s", diff)
			}
		})
	}
}
