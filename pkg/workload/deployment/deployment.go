// Package deployment runs a grant's Pods as a Deployment of one replica
// with a ClusterIP Service in front.
//
// The controller writes both objects by server-side apply under its own
// field manager. It reads them from a cache that holds only the objects it
// labelled, and from the API server on a miss. A replacement Pod comes from
// a rolling update, or from Recreate when the Pod holds an exclusive
// device.
package deployment

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload"
)

// Workload is the workload.Workload that runs a grant as a Deployment.
type Workload struct {
	client client.Client
	reader client.Reader
}

var _ workload.Workload = (*Workload)(nil)

// New returns a Workload that applies through c and reads through c, then
// through live on a miss. The cache behind c may hold only the objects the
// controller labelled, so live is what still finds any other. The scheme of
// c must know the CapacityGrant kind, since the objects carry a controller
// reference to their grant.
func New(c client.Client, live client.Reader) *Workload {
	return &Workload{client: c, reader: workload.CacheThenLive(c, live)}
}

// Observe reads the Deployment and the Service named
// workload.ObjectName(cg). An absent object is not an error. The Deployment
// is checked first, so when both conflict the observation names the
// Deployment.
//
// An object the cache does not hold is read live, so this is where a new
// grant costs one Get per kind. A foreign object the cache filters out is
// still reported. An object cg controls that lost its label is observed as
// usual, and Apply puts the label back.
func (w *Workload) Observe(ctx context.Context, cg *v1alpha1.CapacityGrant) (workload.Observation, error) {
	key := client.ObjectKey{Namespace: cg.Namespace, Name: workload.ObjectName(cg)}

	var obs workload.Observation
	d := &appsv1.Deployment{}
	switch err := w.reader.Get(ctx, key, d); {
	case apierrors.IsNotFound(err):
	case err != nil:
		return workload.Observation{}, fmt.Errorf("get Deployment %s: %w", key, err)
	case !metav1.IsControlledBy(d, cg):
		return conflict(v1alpha1.ReasonDeploymentConflict, "Deployment", d, cg), nil
	default:
		obs = observe(d)
	}

	svc := &corev1.Service{}
	switch err := w.reader.Get(ctx, key, svc); {
	case apierrors.IsNotFound(err):
	case err != nil:
		return workload.Observation{}, fmt.Errorf("get Service %s: %w", key, err)
	case !metav1.IsControlledBy(svc, cg):
		return conflict(v1alpha1.ReasonServiceConflict, "Service", svc, cg), nil
	default:
		obs.ServiceFound = true
	}
	return obs, nil
}

// conflict returns the observation of obj, a kind object of the grant's
// name that cg does not control. It names obj's controller, if any, but
// reads nothing else from it.
func conflict(reason, kind string, obj metav1.Object, cg *v1alpha1.CapacityGrant) workload.Observation {
	msg := fmt.Sprintf("%s %s already exists and is not controlled by CapacityGrant %s.", kind, obj.GetName(), cg.Name)
	if ref := metav1.GetControllerOf(obj); ref != nil {
		msg = fmt.Sprintf("%s %s already exists and is controlled by %s %s.", kind, obj.GetName(), ref.Kind, ref.Name)
	}
	return workload.Observation{ConflictReason: reason, Message: msg}
}

// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=create;patch
// +kubebuilder:rbac:groups=core,resources=services,verbs=create;patch

// Apply applies the Service, then the Deployment, by server-side apply
// with the field manager v1alpha1.ManagedBy. Both objects are built before
// either is sent, and the Service goes first, so a failed pass never
// leaves a Deployment without its Service.
//
// The Deployment apply forces ownership, so a field another manager took
// is taken back. Fields another manager owns alone, such as a restartedAt
// annotation on the template, stay. Its immutable selector keeps it from
// taking over a foreign Deployment created since Observe read.
//
// A Service has no immutable field to guard it, so obs decides. A Service
// cg controls is applied with forced ownership, so a hand edit heals on the
// next pass as it does on the Deployment. Any other Service is applied
// without forcing, so a foreign Service created since Observe read refuses
// the apply with a conflict on its selector instead of being rewritten and
// adopted. A foreign Service with no selector holds no field that conflicts,
// so that last guard does not cover it.
//
// An API error is returned wrapped, so apierrors.IsForbidden and
// apierrors.IsConflict still see it.
func (w *Workload) Apply(ctx context.Context, cg *v1alpha1.CapacityGrant, tmpl corev1.PodTemplateSpec, obs workload.Observation) error {
	scheme := w.client.Scheme()

	svc, err := workload.BuildService(cg, scheme)
	if err != nil {
		return err
	}
	svcAC, err := serviceApplyConfiguration(svc)
	if err != nil {
		return err
	}
	d, err := Build(cg, tmpl, scheme)
	if err != nil {
		return err
	}
	dAC, err := deploymentApplyConfiguration(d)
	if err != nil {
		return err
	}

	svcOpts := []client.ApplyOption{client.FieldOwner(v1alpha1.ManagedBy)}
	if obs.ServiceFound {
		svcOpts = append(svcOpts, client.ForceOwnership)
	}
	if err := w.client.Apply(ctx, svcAC, svcOpts...); err != nil {
		return fmt.Errorf("apply Service %s/%s: %w", svc.Namespace, svc.Name, err)
	}
	if err := w.client.Apply(ctx, dAC, client.FieldOwner(v1alpha1.ManagedBy), client.ForceOwnership); err != nil {
		return fmt.Errorf("apply Deployment %s/%s: %w", d.Namespace, d.Name, err)
	}
	return nil
}
