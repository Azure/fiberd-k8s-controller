// Package workload is the seam between the CapacityGrant reconciler and the
// objects that run a grant's Pods.
//
// A Workload reads what runs a grant, applies what should run it, and
// traces a Pod back to its grant. The reconciler builds the Pod template
// with package podtemplate and never names a workload kind itself, so a
// later kind, such as a LeaderWorkerSet, is a second implementation of the
// same three methods. Package deployment holds the one implementation, a
// Deployment with a Service in front.
package workload

import (
	"context"
	"errors"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

// objectNamePrefix comes before the grant name in the name of every object
// the controller creates for a grant. The CRD caps grant names at 56
// characters, so the name still fits a Service name.
const objectNamePrefix = "fiberd-"

// ErrNotOwned is a Pod whose owners do not lead to a CapacityGrant.
var ErrNotOwned = errors.New("the Pod is not owned by a CapacityGrant")

// Workload runs the Pods of a grant.
type Workload interface {
	// Observe reads the objects that run cg. An object that is absent is
	// not an error. An object of the right name that cg does not control
	// is reported as a conflict, not returned as cg's own.
	Observe(ctx context.Context, cg *v1alpha1.CapacityGrant) (Observation, error)

	// Apply makes the objects that run cg match tmpl, creating them when
	// they are absent. obs is what Observe reported in this pass, and a
	// caller must not apply over a conflict. It returns the API error
	// unchanged in its chain, so the caller can tell a forbidden request
	// from any other.
	Apply(ctx context.Context, cg *v1alpha1.CapacityGrant, tmpl corev1.PodTemplateSpec, obs Observation) error

	// GrantOf returns the CapacityGrant that pod's owners lead to. Each
	// object comes from r, so the caller chooses between the cache and a
	// live read. A chain that breaks anywhere wraps ErrNotOwned. A failed
	// read does not, so a caller can tell an outage from a stranger.
	GrantOf(ctx context.Context, r client.Reader, pod *corev1.Pod) (*v1alpha1.CapacityGrant, error)
}

// Observation is what a Workload found for a grant.
type Observation struct {
	// ConflictReason is v1alpha1.ReasonDeploymentConflict or
	// v1alpha1.ReasonServiceConflict when an object of the grant's name
	// exists and the grant does not control it. It is empty when nothing
	// conflicts. When it is set, only Message is set with it, so nothing
	// is ever read from a foreign object.
	ConflictReason string

	// Message says what conflicts. It is empty when ConflictReason is.
	Message string

	// Found reports whether the grant's workload exists.
	Found bool

	// ServiceFound reports whether the grant controls an existing Service.
	// Only then does Apply force ownership of it.
	ServiceFound bool

	// Available is how many of the grant's Pods are available.
	Available int32

	// Progressing is the workload's rollout state, with the type
	// v1alpha1.ConditionProgressing. Its ObservedGeneration and
	// LastTransitionTime are left for the caller, which owns the grant's
	// status. It is the zero condition when Found is false.
	Progressing metav1.Condition
}

// ObjectName returns the name of every object the controller creates for
// cg, which is fiberd-<grant name>.
func ObjectName(cg *v1alpha1.CapacityGrant) string {
	return objectNamePrefix + cg.Name
}

// SelectorLabels returns the labels that select cg's Pods. They hold the
// grant UID as well as its name, so a grant recreated under the same name
// never selects the Pods of the one before it.
func SelectorLabels(cg *v1alpha1.CapacityGrant) map[string]string {
	return map[string]string{
		v1alpha1.LabelGrant:    cg.Name,
		v1alpha1.LabelGrantUID: string(cg.UID),
	}
}

// Labels returns the labels of every object the controller creates for cg.
// They are the selector labels and the managed-by label.
func Labels(cg *v1alpha1.CapacityGrant) map[string]string {
	l := SelectorLabels(cg)
	l[v1alpha1.LabelManagedBy] = v1alpha1.ManagedBy
	return l
}
