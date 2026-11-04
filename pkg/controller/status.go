package controller

import (
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload"
)

// The condition messages the controller writes itself. Blocked passes
// carry the message of what blocked them.
const (
	msgPending     = "The Deployment has not been created yet."
	msgUnavailable = "The Deployment has no available Pod."
	msgAvailable   = "The Deployment has an available Pod."
)

// blockage is why a pass applied nothing. The reason is one of
// v1alpha1.ReasonInvalidSpec, ReasonForbidden, ReasonDeploymentConflict and
// ReasonServiceConflict. The action is the step of the pass that found the
// block, which a recorded Event carries.
type blockage struct {
	reason  string
	action  string
	message string
}

// blocked returns what keeps a pass from applying, or nil when it may
// apply. specErr is the error of building the grant Pod. A conflict wins
// over an invalid spec, because the conflicting object is not the grant's
// and its progress must never reach the grant's status.
func blocked(obs workload.Observation, specErr error) *blockage {
	switch {
	case obs.ConflictReason != "":
		return &blockage{reason: obs.ConflictReason, action: actionObserve, message: obs.Message}
	case specErr != nil:
		return &blockage{reason: v1alpha1.ReasonInvalidSpec, message: specErr.Error()}
	}
	return nil
}

// computeStatus returns the status a pass leaves on a grant of the given
// generation. obs is what the pass observed before it applied, and blk is
// what blocked it, nil when nothing did. old is never modified, and fields
// and conditions the pass does not decide are kept from it. Each condition
// takes now as its LastTransitionTime only when its status changes.
func computeStatus(generation int64, obs workload.Observation, blk *blockage, now time.Time, old v1alpha1.CapacityGrantStatus) v1alpha1.CapacityGrantStatus {
	status := *old.DeepCopy()
	status.ObservedGeneration = generation
	ready, progressing := conditions(obs, blk)
	for _, c := range []metav1.Condition{ready, progressing} {
		c.ObservedGeneration = generation
		c.LastTransitionTime = metav1.NewTime(now)
		meta.SetStatusCondition(&status.Conditions, c)
	}
	return status
}

// conditions decides the Ready and Progressing conditions, leaving their
// generation and transition time to computeStatus.
func conditions(obs workload.Observation, blk *blockage) (ready, progressing metav1.Condition) {
	switch {
	case blk != nil && blk.reason == v1alpha1.ReasonInvalidSpec:
		// Nothing was applied, so a Deployment built from an earlier spec
		// keeps rolling out or serving, and its progress is still true.
		ready = condition(v1alpha1.ConditionReady, metav1.ConditionFalse, blk.reason, blk.message)
		if obs.Found {
			return ready, progressingFrom(obs.Progressing)
		}
		return ready, condition(v1alpha1.ConditionProgressing, metav1.ConditionUnknown, blk.reason, blk.message)
	case blk != nil:
		// The Deployment, if one exists, may not be the grant's, or the
		// controller may no longer apply it. Its progress says nothing about
		// the grant.
		return condition(v1alpha1.ConditionReady, metav1.ConditionFalse, blk.reason, blk.message),
			condition(v1alpha1.ConditionProgressing, metav1.ConditionUnknown, blk.reason, blk.message)
	case !obs.Found:
		return condition(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonPending, msgPending),
			condition(v1alpha1.ConditionProgressing, metav1.ConditionUnknown, v1alpha1.ReasonPending, msgPending)
	case obs.Available >= 1:
		return condition(v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonAvailable, msgAvailable),
			progressingFrom(obs.Progressing)
	default:
		return condition(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonUnavailable, msgUnavailable),
			progressingFrom(obs.Progressing)
	}
}

// progressingFrom returns the Deployment's Progressing condition c as the
// grant's. A condition without a status or reason would be refused by the
// API server, so it reads as Unknown and Pending instead.
func progressingFrom(c metav1.Condition) metav1.Condition {
	if c.Status == "" || c.Reason == "" {
		return condition(v1alpha1.ConditionProgressing, metav1.ConditionUnknown, v1alpha1.ReasonPending, msgPending)
	}
	return condition(v1alpha1.ConditionProgressing, c.Status, c.Reason, c.Message)
}

// condition returns a condition of the given type, status, reason and
// message.
func condition(conditionType string, status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{Type: conditionType, Status: status, Reason: reason, Message: message}
}
