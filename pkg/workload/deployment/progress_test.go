package deployment

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload"
)

// The Progressing reasons the Deployment controller sets, as
// kubernetes/kubernetes pkg/controller/deployment/util names them.
const (
	reasonReplicaSetUpdated        = "ReplicaSetUpdated"
	reasonNewRSAvailable           = "NewReplicaSetAvailable"
	reasonProgressDeadlineExceeded = "ProgressDeadlineExceeded"
)

// TestObserve covers the Deployment rows of the design's status table.
// The Ready column follows from Available, which the reconciler reads.
func TestObserve(t *testing.T) {
	since := metav1.NewTime(time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC))
	progressing := func(status corev1.ConditionStatus, reason, msg string) appsv1.DeploymentCondition {
		return appsv1.DeploymentCondition{
			Type:               appsv1.DeploymentProgressing,
			Status:             status,
			Reason:             reason,
			Message:            msg,
			LastUpdateTime:     since,
			LastTransitionTime: since,
		}
	}
	available := func(status corev1.ConditionStatus) appsv1.DeploymentCondition {
		return appsv1.DeploymentCondition{
			Type:   appsv1.DeploymentAvailable,
			Status: status,
			Reason: "MinimumReplicasAvailable",
		}
	}
	cond := func(status metav1.ConditionStatus, reason, msg string) metav1.Condition {
		return metav1.Condition{Type: v1alpha1.ConditionProgressing, Status: status, Reason: reason, Message: msg}
	}

	cases := []struct {
		name               string
		generation         int64
		observedGeneration int64
		availableReplicas  int32
		conditions         []appsv1.DeploymentCondition
		want               workload.Observation
	}{
		{
			name:       "a new Deployment without status is pending",
			generation: 1,
			want: workload.Observation{
				Found:       true,
				Progressing: cond(metav1.ConditionTrue, v1alpha1.ReasonPending, "Waiting for the Deployment controller to observe generation 1."),
			},
		},
		{
			name:               "a progressing Deployment with no available Pod copies its condition",
			generation:         1,
			observedGeneration: 1,
			conditions: []appsv1.DeploymentCondition{
				available(corev1.ConditionFalse),
				progressing(corev1.ConditionTrue, reasonReplicaSetUpdated, `ReplicaSet "fiberd-web-1" is progressing.`),
			},
			want: workload.Observation{
				Found:       true,
				Progressing: cond(metav1.ConditionTrue, reasonReplicaSetUpdated, `ReplicaSet "fiberd-web-1" is progressing.`),
			},
		},
		{
			name:               "an available Deployment reports its Pod",
			generation:         1,
			observedGeneration: 1,
			availableReplicas:  1,
			conditions: []appsv1.DeploymentCondition{
				available(corev1.ConditionTrue),
				progressing(corev1.ConditionTrue, reasonNewRSAvailable, `ReplicaSet "fiberd-web-1" has successfully progressed.`),
			},
			want: workload.Observation{
				Found:       true,
				Available:   1,
				Progressing: cond(metav1.ConditionTrue, reasonNewRSAvailable, `ReplicaSet "fiberd-web-1" has successfully progressed.`),
			},
		},
		{
			name:               "the old Pod counts as available during a surge",
			generation:         2,
			observedGeneration: 2,
			availableReplicas:  1,
			conditions: []appsv1.DeploymentCondition{
				available(corev1.ConditionTrue),
				progressing(corev1.ConditionTrue, reasonReplicaSetUpdated, `ReplicaSet "fiberd-web-2" is progressing.`),
			},
			want: workload.Observation{
				Found:       true,
				Available:   1,
				Progressing: cond(metav1.ConditionTrue, reasonReplicaSetUpdated, `ReplicaSet "fiberd-web-2" is progressing.`),
			},
		},
		{
			name:               "an exceeded progress deadline keeps the old Pod available",
			generation:         2,
			observedGeneration: 2,
			availableReplicas:  1,
			conditions: []appsv1.DeploymentCondition{
				available(corev1.ConditionTrue),
				progressing(corev1.ConditionFalse, reasonProgressDeadlineExceeded, `ReplicaSet "fiberd-web-2" has timed out progressing.`),
			},
			want: workload.Observation{
				Found:       true,
				Available:   1,
				Progressing: cond(metav1.ConditionFalse, reasonProgressDeadlineExceeded, `ReplicaSet "fiberd-web-2" has timed out progressing.`),
			},
		},
		{
			name:               "a Recreate gap has no available Pod",
			generation:         2,
			observedGeneration: 2,
			conditions: []appsv1.DeploymentCondition{
				available(corev1.ConditionFalse),
				progressing(corev1.ConditionTrue, reasonReplicaSetUpdated, `ReplicaSet "fiberd-web-2" is progressing.`),
			},
			want: workload.Observation{
				Found:       true,
				Progressing: cond(metav1.ConditionTrue, reasonReplicaSetUpdated, `ReplicaSet "fiberd-web-2" is progressing.`),
			},
		},
		{
			name:               "spec change not yet observed hides the old condition",
			generation:         3,
			observedGeneration: 2,
			availableReplicas:  1,
			conditions: []appsv1.DeploymentCondition{
				available(corev1.ConditionTrue),
				progressing(corev1.ConditionTrue, reasonNewRSAvailable, `ReplicaSet "fiberd-web-2" has successfully progressed.`),
			},
			want: workload.Observation{
				Found:       true,
				Available:   1,
				Progressing: cond(metav1.ConditionTrue, v1alpha1.ReasonPending, "Waiting for the Deployment controller to observe generation 3."),
			},
		},
		{
			name:               "an observed Deployment without a Progressing condition is unknown",
			generation:         1,
			observedGeneration: 1,
			conditions:         []appsv1.DeploymentCondition{available(corev1.ConditionFalse)},
			want: workload.Observation{
				Found:       true,
				Progressing: cond(metav1.ConditionUnknown, v1alpha1.ReasonPending, "The Deployment reports no Progressing condition."),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "fiberd-web", Namespace: "team-a", Generation: tc.generation},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: tc.observedGeneration,
					AvailableReplicas:  tc.availableReplicas,
					Conditions:         tc.conditions,
				},
			}
			if diff := cmp.Diff(tc.want, observe(d)); diff != "" {
				t.Errorf("observe mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
