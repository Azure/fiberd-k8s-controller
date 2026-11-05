package deployment

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload"
)

// GrantOf walks pod's controller references through its ReplicaSet and its
// Deployment to the CapacityGrant. Names are reused, so each hop compares
// the UID the reference holds, which keeps a grant recreated under the same
// name from claiming the Pods of the one before it.
//
// The grant labels are then checked against the grant, as a consistency
// check only. Labels are mutable and say nothing about provenance. Owner
// references can be forged by a namespace admin too, so the chain proves
// what the controller built, not who wrote it. The token a caller presents
// binds the namespace.
func (w *Workload) GrantOf(ctx context.Context, r client.Reader, pod *corev1.Pod) (*v1alpha1.CapacityGrant, error) {
	rs := &appsv1.ReplicaSet{}
	if err := getController(ctx, r, pod, appsv1.GroupName, "ReplicaSet", rs); err != nil {
		return nil, err
	}
	d := &appsv1.Deployment{}
	if err := getController(ctx, r, rs, appsv1.GroupName, "Deployment", d); err != nil {
		return nil, err
	}
	cg := &v1alpha1.CapacityGrant{}
	if err := getController(ctx, r, d, v1alpha1.GroupName, "CapacityGrant", cg); err != nil {
		return nil, err
	}
	for k, v := range workload.SelectorLabels(cg) {
		if pod.Labels[k] != v {
			return nil, fmt.Errorf("%w: Pod %s/%s has label %s=%q, want %q", workload.ErrNotOwned, pod.Namespace, pod.Name, k, pod.Labels[k], v)
		}
	}
	return cg, nil
}

// getController reads obj's controller into owner. The reference must name
// kind in group, and the object read must carry the reference's UID.
// Anything else wraps workload.ErrNotOwned, and only a failed read does not.
func getController(ctx context.Context, r client.Reader, obj client.Object, group, kind string, owner client.Object) error {
	ref := metav1.GetControllerOf(obj)
	if ref == nil {
		return fmt.Errorf("%w: %s/%s has no controller", workload.ErrNotOwned, obj.GetNamespace(), obj.GetName())
	}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil || gv.Group != group || ref.Kind != kind {
		return fmt.Errorf("%w: %s/%s is controlled by %s %s, want a %s", workload.ErrNotOwned, obj.GetNamespace(), obj.GetName(), ref.APIVersion, ref.Kind, kind)
	}
	key := client.ObjectKey{Namespace: obj.GetNamespace(), Name: ref.Name}
	switch err := r.Get(ctx, key, owner); {
	case apierrors.IsNotFound(err):
		return fmt.Errorf("%w: %s %s does not exist", workload.ErrNotOwned, kind, key)
	case err != nil:
		return fmt.Errorf("get %s %s: %w", kind, key, err)
	case owner.GetUID() != ref.UID:
		return fmt.Errorf("%w: %s %s has UID %s, the reference names %s", workload.ErrNotOwned, kind, key, owner.GetUID(), ref.UID)
	}
	return nil
}
