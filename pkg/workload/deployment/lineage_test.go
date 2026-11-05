package deployment

import (
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload"
)

// lineage is one grant Pod and the objects above it. A nil object is left
// out of the API.
type lineage struct {
	grant      *v1alpha1.CapacityGrant
	deployment *appsv1.Deployment
	replicaSet *appsv1.ReplicaSet
	pod        *corev1.Pod
}

// newLineage is a whole chain from the grant down to its Pod.
func newLineage() *lineage {
	cg := testGrant()
	d := servingDeployment(testUID)
	d.UID = "deployment-uid"
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name:            "fiberd-web-1",
		Namespace:       "team-a",
		UID:             "replicaset-uid",
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(d, appsv1.SchemeGroupVersion.WithKind("Deployment"))},
	}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:            "fiberd-web-1-abcde",
		Namespace:       "team-a",
		UID:             "pod-uid",
		Labels:          workload.SelectorLabels(cg),
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(rs, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))},
	}}
	return &lineage{grant: cg, deployment: d, replicaSet: rs, pod: pod}
}

// objects are the chain's objects above the Pod. GrantOf is handed the Pod.
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
	return objs
}

// setController makes ref obj's one owner reference.
func setController(obj metav1.Object, ref metav1.OwnerReference) {
	obj.SetOwnerReferences([]metav1.OwnerReference{ref})
}

// foreignRef is a controller reference to a kind outside the chain.
func foreignRef(apiVersion, kind string) metav1.OwnerReference {
	return *metav1.NewControllerRef(&metav1.ObjectMeta{Name: "other", UID: "other-uid"}, schema.FromAPIVersionAndKind(apiVersion, kind))
}

func TestGrantOf(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*lineage)
		funcs   interceptor.Funcs
		wantErr error
	}{
		{name: "a whole chain leads to the grant"},
		{
			name:    "a Pod with no owner",
			mutate:  func(l *lineage) { l.pod.OwnerReferences = nil },
			wantErr: workload.ErrNotOwned,
		},
		{
			name:    "a Pod whose only owner is not its controller",
			mutate:  func(l *lineage) { l.pod.OwnerReferences[0].Controller = ptr.To(false) },
			wantErr: workload.ErrNotOwned,
		},
		{
			name:    "a Pod controlled by another kind",
			mutate:  func(l *lineage) { setController(l.pod, foreignRef("apps/v1", "StatefulSet")) },
			wantErr: workload.ErrNotOwned,
		},
		{
			name:    "a Pod controlled by a ReplicaSet of another group",
			mutate:  func(l *lineage) { setController(l.pod, foreignRef("example.com/v1", "ReplicaSet")) },
			wantErr: workload.ErrNotOwned,
		},
		{
			name:    "a controller reference with a bad API version",
			mutate:  func(l *lineage) { l.pod.OwnerReferences[0].APIVersion = "a/b/c" },
			wantErr: workload.ErrNotOwned,
		},
		{
			name:    "a ReplicaSet that is gone",
			mutate:  func(l *lineage) { l.replicaSet = nil },
			wantErr: workload.ErrNotOwned,
		},
		{
			name:    "a ReplicaSet recreated under the same name",
			mutate:  func(l *lineage) { l.replicaSet.UID = otherUID },
			wantErr: workload.ErrNotOwned,
		},
		{
			name:    "a ReplicaSet with no controller",
			mutate:  func(l *lineage) { l.replicaSet.OwnerReferences = nil },
			wantErr: workload.ErrNotOwned,
		},
		{
			name:    "a ReplicaSet controlled by another kind",
			mutate:  func(l *lineage) { setController(l.replicaSet, foreignRef("argoproj.io/v1alpha1", "Rollout")) },
			wantErr: workload.ErrNotOwned,
		},
		{
			name:    "a Deployment that is gone",
			mutate:  func(l *lineage) { l.deployment = nil },
			wantErr: workload.ErrNotOwned,
		},
		{
			name:    "a Deployment recreated under the same name",
			mutate:  func(l *lineage) { l.deployment.UID = otherUID },
			wantErr: workload.ErrNotOwned,
		},
		{
			name:    "a Deployment with no controller",
			mutate:  func(l *lineage) { l.deployment.OwnerReferences = nil },
			wantErr: workload.ErrNotOwned,
		},
		{
			name:    "a Deployment controlled by another kind",
			mutate:  func(l *lineage) { setController(l.deployment, foreignRef("example.com/v1", "Tenant")) },
			wantErr: workload.ErrNotOwned,
		},
		{
			name:    "a grant that is gone",
			mutate:  func(l *lineage) { l.grant = nil },
			wantErr: workload.ErrNotOwned,
		},
		{
			name:    "a grant recreated under the same name",
			mutate:  func(l *lineage) { l.grant.UID = otherUID },
			wantErr: workload.ErrNotOwned,
		},
		{
			name:    "a Pod without the grant labels",
			mutate:  func(l *lineage) { l.pod.Labels = nil },
			wantErr: workload.ErrNotOwned,
		},
		{
			name:    "a Pod labeled for another grant name",
			mutate:  func(l *lineage) { l.pod.Labels[v1alpha1.LabelGrant] = "other" },
			wantErr: workload.ErrNotOwned,
		},
		{
			name:    "a Pod labeled for another grant UID",
			mutate:  func(l *lineage) { l.pod.Labels[v1alpha1.LabelGrantUID] = otherUID },
			wantErr: workload.ErrNotOwned,
		},
		{
			name:    "a failed ReplicaSet read is not a stranger",
			funcs:   failGet(&appsv1.ReplicaSet{}),
			wantErr: errRead,
		},
		{
			name:    "a failed Deployment read is not a stranger",
			funcs:   failGet(&appsv1.Deployment{}),
			wantErr: errRead,
		},
		{
			name:    "a failed grant read is not a stranger",
			funcs:   failGet(&v1alpha1.CapacityGrant{}),
			wantErr: errRead,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newLineage()
			if tc.mutate != nil {
				tc.mutate(l)
			}
			r := newClient(t, tc.funcs, l.objects()...)

			// New gets no client, since every read must go through r.
			got, err := New(nil).GrantOf(t.Context(), r, l.pod)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("GrantOf error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				if errors.Is(tc.wantErr, errRead) && errors.Is(err, workload.ErrNotOwned) {
					t.Fatalf("GrantOf error = %v, a failed read must not wrap ErrNotOwned", err)
				}
				if got != nil {
					t.Fatalf("GrantOf = %s, want nil on an error", got.Name)
				}
				return
			}
			if got.Name != l.grant.Name || got.Namespace != l.grant.Namespace || got.UID != l.grant.UID {
				t.Fatalf("GrantOf = %s/%s uid %s, want %s/%s uid %s", got.Namespace, got.Name, got.UID, l.grant.Namespace, l.grant.Name, l.grant.UID)
			}
		})
	}
}
