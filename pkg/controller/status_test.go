package controller

import (
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload"
)

// The conflict messages are the ones deployment.Workload writes, so the
// Event text the tests pin is the real one.
const (
	msgConflict        = "Deployment fiberd-web already exists and is not controlled by CapacityGrant web."
	msgServiceConflict = "Service fiberd-web already exists and is not controlled by CapacityGrant web."
	msgInvalid         = "spec.fibers.max: Invalid value: 0: must be at least 1."
)

var (
	earlier = time.Date(2026, time.November, 3, 11, 0, 0, 0, time.UTC)
	later   = time.Date(2026, time.November, 3, 12, 0, 0, 0, time.UTC)
)

// cond is a condition of generation 3 that last changed at at.
func cond(conditionType string, status metav1.ConditionStatus, reason, message string, at time.Time) metav1.Condition {
	return metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: 3,
		LastTransitionTime: metav1.NewTime(at),
	}
}

// rolledOut is the Progressing condition of a Deployment whose rollout is
// complete, as a Workload reports it.
var rolledOut = metav1.Condition{
	Type:    v1alpha1.ConditionProgressing,
	Status:  metav1.ConditionTrue,
	Reason:  "NewReplicaSetAvailable",
	Message: `ReplicaSet "fiberd-web-7d9f8c6b5" has successfully progressed.`,
}

func TestBlocked(t *testing.T) {
	specErr := errors.New(msgInvalid)
	cases := []struct {
		name    string
		obs     workload.Observation
		specErr error
		want    *blockage
	}{
		{
			name: "a found workload without a conflict or fault is not blocked",
			obs:  workload.Observation{Found: true, Available: 1},
		},
		{
			name:    "an invalid spec blocks with its fault as the message",
			specErr: specErr,
			want:    &blockage{reason: v1alpha1.ReasonInvalidSpec, message: msgInvalid},
		},
		{
			name: "a foreign Deployment blocks at observe",
			obs:  workload.Observation{ConflictReason: v1alpha1.ReasonDeploymentConflict, Message: msgConflict},
			want: &blockage{reason: v1alpha1.ReasonDeploymentConflict, action: actionObserve, message: msgConflict},
		},
		{
			name: "a foreign Service blocks at observe",
			obs:  workload.Observation{ConflictReason: v1alpha1.ReasonServiceConflict, Message: msgServiceConflict},
			want: &blockage{reason: v1alpha1.ReasonServiceConflict, action: actionObserve, message: msgServiceConflict},
		},
		{
			name:    "a conflict wins over an invalid spec",
			obs:     workload.Observation{ConflictReason: v1alpha1.ReasonDeploymentConflict, Message: msgConflict},
			specErr: specErr,
			want:    &blockage{reason: v1alpha1.ReasonDeploymentConflict, action: actionObserve, message: msgConflict},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := blocked(tc.obs, tc.specErr)
			if diff := cmp.Diff(tc.want, got, cmp.AllowUnexported(blockage{})); diff != "" {
				t.Errorf("blocked (-want +got):\n%s", diff)
			}
		})
	}
}

func TestComputeStatus(t *testing.T) {
	msgForbidden := `The controller may lack the tenant RoleBinding for ClusterRole fiberd-k8s-controller-tenant in namespace team-a. The apply failed with services "fiberd-web" is forbidden: no tenant RoleBinding.`
	cases := []struct {
		name string
		obs  workload.Observation
		blk  *blockage
		old  v1alpha1.CapacityGrantStatus
		want v1alpha1.CapacityGrantStatus
	}{
		{
			name: "a missing Deployment is pending",
			want: v1alpha1.CapacityGrantStatus{
				ObservedGeneration: 3,
				Conditions: []metav1.Condition{
					cond(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonPending, msgPending, later),
					cond(v1alpha1.ConditionProgressing, metav1.ConditionUnknown, v1alpha1.ReasonPending, msgPending, later),
				},
			},
		},
		{
			name: "a Deployment without an available Pod is unavailable",
			obs:  workload.Observation{Found: true, Progressing: rolledOut},
			want: v1alpha1.CapacityGrantStatus{
				ObservedGeneration: 3,
				Conditions: []metav1.Condition{
					cond(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonUnavailable, msgUnavailable, later),
					cond(v1alpha1.ConditionProgressing, metav1.ConditionTrue, rolledOut.Reason, rolledOut.Message, later),
				},
			},
		},
		{
			name: "a Deployment with an available Pod is ready",
			obs:  workload.Observation{Found: true, Available: 1, Progressing: rolledOut},
			want: v1alpha1.CapacityGrantStatus{
				ObservedGeneration: 3,
				Conditions: []metav1.Condition{
					cond(v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonAvailable, msgAvailable, later),
					cond(v1alpha1.ConditionProgressing, metav1.ConditionTrue, rolledOut.Reason, rolledOut.Message, later),
				},
			},
		},
		{
			name: "a Deployment without a Progressing condition reads as pending progress",
			obs:  workload.Observation{Found: true},
			want: v1alpha1.CapacityGrantStatus{
				ObservedGeneration: 3,
				Conditions: []metav1.Condition{
					cond(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonUnavailable, msgUnavailable, later),
					cond(v1alpha1.ConditionProgressing, metav1.ConditionUnknown, v1alpha1.ReasonPending, msgPending, later),
				},
			},
		},
		{
			name: "InvalidSpec keeps the progress of a serving Deployment",
			obs:  workload.Observation{Found: true, Available: 1, Progressing: rolledOut},
			blk:  &blockage{reason: v1alpha1.ReasonInvalidSpec, message: msgInvalid},
			want: v1alpha1.CapacityGrantStatus{
				ObservedGeneration: 3,
				Conditions: []metav1.Condition{
					cond(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonInvalidSpec, msgInvalid, later),
					cond(v1alpha1.ConditionProgressing, metav1.ConditionTrue, rolledOut.Reason, rolledOut.Message, later),
				},
			},
		},
		{
			name: "InvalidSpec without a Deployment leaves progress unknown",
			blk:  &blockage{reason: v1alpha1.ReasonInvalidSpec, message: msgInvalid},
			want: v1alpha1.CapacityGrantStatus{
				ObservedGeneration: 3,
				Conditions: []metav1.Condition{
					cond(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonInvalidSpec, msgInvalid, later),
					cond(v1alpha1.ConditionProgressing, metav1.ConditionUnknown, v1alpha1.ReasonInvalidSpec, msgInvalid, later),
				},
			},
		},
		{
			name: "Forbidden never copies the Deployment's progress",
			obs:  workload.Observation{Found: true, Available: 1, Progressing: rolledOut},
			blk:  &blockage{reason: v1alpha1.ReasonForbidden, message: msgForbidden},
			want: v1alpha1.CapacityGrantStatus{
				ObservedGeneration: 3,
				Conditions: []metav1.Condition{
					cond(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonForbidden, msgForbidden, later),
					cond(v1alpha1.ConditionProgressing, metav1.ConditionUnknown, v1alpha1.ReasonForbidden, msgForbidden, later),
				},
			},
		},
		{
			name: "DeploymentConflict never copies the foreign progress",
			obs:  workload.Observation{ConflictReason: v1alpha1.ReasonDeploymentConflict, Message: msgConflict, Found: true, Available: 1, Progressing: rolledOut},
			blk:  &blockage{reason: v1alpha1.ReasonDeploymentConflict, message: msgConflict},
			want: v1alpha1.CapacityGrantStatus{
				ObservedGeneration: 3,
				Conditions: []metav1.Condition{
					cond(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonDeploymentConflict, msgConflict, later),
					cond(v1alpha1.ConditionProgressing, metav1.ConditionUnknown, v1alpha1.ReasonDeploymentConflict, msgConflict, later),
				},
			},
		},
		{
			name: "ServiceConflict never copies the Deployment progress",
			obs:  workload.Observation{ConflictReason: v1alpha1.ReasonServiceConflict, Message: msgServiceConflict, Found: true, Progressing: rolledOut},
			blk:  &blockage{reason: v1alpha1.ReasonServiceConflict, action: actionObserve, message: msgServiceConflict},
			want: v1alpha1.CapacityGrantStatus{
				ObservedGeneration: 3,
				Conditions: []metav1.Condition{
					cond(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonServiceConflict, msgServiceConflict, later),
					cond(v1alpha1.ConditionProgressing, metav1.ConditionUnknown, v1alpha1.ReasonServiceConflict, msgServiceConflict, later),
				},
			},
		},
		{
			name: "a condition keeps its transition time while its status holds",
			obs:  workload.Observation{Found: true, Progressing: rolledOut},
			old: v1alpha1.CapacityGrantStatus{
				ObservedGeneration: 2,
				Conditions: []metav1.Condition{
					cond(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonPending, msgPending, earlier),
					cond(v1alpha1.ConditionProgressing, metav1.ConditionUnknown, v1alpha1.ReasonPending, msgPending, earlier),
				},
			},
			want: v1alpha1.CapacityGrantStatus{
				ObservedGeneration: 3,
				Conditions: []metav1.Condition{
					cond(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonUnavailable, msgUnavailable, earlier),
					cond(v1alpha1.ConditionProgressing, metav1.ConditionTrue, rolledOut.Reason, rolledOut.Message, later),
				},
			},
		},
		{
			name: "other conditions and fields are kept",
			old: v1alpha1.CapacityGrantStatus{
				ResolvedImage: "ghcr.io/azure/fiberd:v0.4.0",
				Conditions: []metav1.Condition{
					cond("Admitted", metav1.ConditionTrue, "Admitted", "", earlier),
				},
			},
			want: v1alpha1.CapacityGrantStatus{
				ObservedGeneration: 3,
				ResolvedImage:      "ghcr.io/azure/fiberd:v0.4.0",
				Conditions: []metav1.Condition{
					cond("Admitted", metav1.ConditionTrue, "Admitted", "", earlier),
					cond(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonPending, msgPending, later),
					cond(v1alpha1.ConditionProgressing, metav1.ConditionUnknown, v1alpha1.ReasonPending, msgPending, later),
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := *tc.old.DeepCopy()
			got := computeStatus(3, tc.obs, tc.blk, later, tc.old)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("computeStatus (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(old, tc.old); diff != "" {
				t.Errorf("computeStatus changed the old status (-before +after):\n%s", diff)
			}
		})
	}
}
