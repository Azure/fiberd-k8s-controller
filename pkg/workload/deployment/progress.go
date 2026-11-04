package deployment

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload"
)

// observe reads d's rollout state the way kubectl rollout status does.
// Until the Deployment controller has observed d's generation, its status
// describes an older spec, so Progressing is True with the reason Pending.
// After that, the Deployment's own Progressing condition is copied whole,
// reason and message included. Available is d's available replica count,
// which counts an old Pod that still serves during a surge or after a
// stalled rollout.
func observe(d *appsv1.Deployment) workload.Observation {
	obs := workload.Observation{Found: true, Available: d.Status.AvailableReplicas}
	if d.Status.ObservedGeneration < d.Generation {
		obs.Progressing = metav1.Condition{
			Type:    v1alpha1.ConditionProgressing,
			Status:  metav1.ConditionTrue,
			Reason:  v1alpha1.ReasonPending,
			Message: fmt.Sprintf("Waiting for the Deployment controller to observe generation %d.", d.Generation),
		}
		return obs
	}
	c := progressingCondition(d.Status.Conditions)
	if c == nil {
		obs.Progressing = metav1.Condition{
			Type:    v1alpha1.ConditionProgressing,
			Status:  metav1.ConditionUnknown,
			Reason:  v1alpha1.ReasonPending,
			Message: "The Deployment reports no Progressing condition.",
		}
		return obs
	}
	obs.Progressing = metav1.Condition{
		Type:    v1alpha1.ConditionProgressing,
		Status:  metav1.ConditionStatus(c.Status),
		Reason:  c.Reason,
		Message: c.Message,
	}
	return obs
}

// progressingCondition returns the Progressing condition in conditions, or
// nil when there is none.
func progressingCondition(conditions []appsv1.DeploymentCondition) *appsv1.DeploymentCondition {
	for i := range conditions {
		if conditions[i].Type == appsv1.DeploymentProgressing {
			return &conditions[i]
		}
	}
	return nil
}
