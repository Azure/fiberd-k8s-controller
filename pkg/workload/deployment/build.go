package deployment

import (
	"encoding/json"
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload/podtemplate"
)

// The Kueue labels a grant passes on to its Deployment. Kueue reads them
// from the Deployment object and writes its own onto the Pod template, so
// they are never copied there.
const (
	labelKueueQueueName     = "kueue.x-k8s.io/queue-name"
	labelKueuePriorityClass = "kueue.x-k8s.io/priority-class"
)

// kueueLabels are the grant labels Build copies onto the Deployment object.
var kueueLabels = []string{labelKueueQueueName, labelKueuePriorityClass}

// Build returns the Deployment that runs cg's Pods from tmpl, which is
// what podtemplate.Build returned for cg. It is named
// workload.ObjectName(cg), runs one replica and keeps one old ReplicaSet.
// cg controls it with blockOwnerDeletion, so garbage collection removes it
// with the grant. The selector holds the grant name and UID. It is
// immutable, so a same-named Deployment that the cache has not shown yet
// is never taken over, and its apply fails instead. The scheme must know
// the CapacityGrant kind. Build never changes cg or tmpl.
func Build(cg *v1alpha1.CapacityGrant, tmpl corev1.PodTemplateSpec, scheme *runtime.Scheme) (*appsv1.Deployment, error) {
	if cg == nil {
		return nil, errors.New("deployment: nil CapacityGrant")
	}
	selector := workload.SelectorLabels(cg)
	if !labels.SelectorFromSet(selector).Matches(labels.Set(tmpl.Labels)) {
		return nil, fmt.Errorf("the Pod template labels %v do not match the selector %v", tmpl.Labels, selector)
	}

	d := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: appsv1.SchemeGroupVersion.String(), Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      workload.ObjectName(cg),
			Namespace: cg.Namespace,
			Labels:    objectLabels(cg),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas:             ptr.To[int32](1),
			Selector:             &metav1.LabelSelector{MatchLabels: selector},
			Template:             *tmpl.DeepCopy(),
			Strategy:             strategy(cg),
			RevisionHistoryLimit: ptr.To[int32](1),
		},
	}
	if err := controllerutil.SetControllerReference(cg, d, scheme); err != nil {
		return nil, fmt.Errorf("set the controller reference of Deployment %s: %w", d.Name, err)
	}
	return d, nil
}

// objectLabels returns the Deployment's own labels. They are the labels of
// every grant object and the Kueue labels the grant carries.
func objectLabels(cg *v1alpha1.CapacityGrant) map[string]string {
	l := workload.Labels(cg)
	for _, k := range kueueLabels {
		if v, ok := cg.Labels[k]; ok {
			l[k] = v
		}
	}
	return l
}

// strategy returns Recreate when the template holds an exclusive device,
// since a surge Pod would wait forever for the device the old Pod holds.
// Otherwise it returns a rolling update that starts the new Pod before it
// stops the old one, so the grant keeps serving. When the strategy flips,
// server-side apply drops rollingUpdate, which only the controller owns.
func strategy(cg *v1alpha1.CapacityGrant) appsv1.DeploymentStrategy {
	if podtemplate.NeedsExclusiveDevice(cg) {
		return appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	}
	return appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxSurge:       ptr.To(intstr.FromInt32(1)),
			MaxUnavailable: ptr.To(intstr.FromInt32(0)),
		},
	}
}

// deploymentApplyConfiguration converts d to the apply configuration that
// carries it. client-go has no converter from a typed object, so it is one
// JSON round trip. The status is cleared, since the controller never
// applies it and an empty one would claim its fields.
func deploymentApplyConfiguration(d *appsv1.Deployment) (*appsv1ac.DeploymentApplyConfiguration, error) {
	ac, err := roundTrip[appsv1ac.DeploymentApplyConfiguration](d)
	if err != nil {
		return nil, fmt.Errorf("convert Deployment %s to its apply configuration: %w", d.Name, err)
	}
	ac.Status = nil
	return ac, nil
}

// serviceApplyConfiguration converts svc to the apply configuration that
// carries it, as deploymentApplyConfiguration does for a Deployment.
func serviceApplyConfiguration(svc *corev1.Service) (*corev1ac.ServiceApplyConfiguration, error) {
	ac, err := roundTrip[corev1ac.ServiceApplyConfiguration](svc)
	if err != nil {
		return nil, fmt.Errorf("convert Service %s to its apply configuration: %w", svc.Name, err)
	}
	ac.Status = nil
	return ac, nil
}

// roundTrip encodes obj as JSON and decodes it into a new T.
func roundTrip[T any](obj any) (*T, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	out := new(T)
	if err := json.Unmarshal(data, out); err != nil {
		return nil, err
	}
	return out, nil
}
