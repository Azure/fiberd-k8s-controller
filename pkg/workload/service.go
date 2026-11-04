package workload

import (
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload/podtemplate"
)

// BuildService returns the Service in front of cg's Pods. It is a ClusterIP
// Service named ObjectName(cg) with one port, grpc, on fiberd's Fibers API
// port. cg controls it, so garbage collection removes it with the grant.
// The scheme must know the CapacityGrant kind. BuildService never changes
// cg.
func BuildService(cg *v1alpha1.CapacityGrant, scheme *runtime.Scheme) (*corev1.Service, error) {
	if cg == nil {
		return nil, errors.New("workload: nil CapacityGrant")
	}
	svc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ObjectName(cg),
			Namespace: cg.Namespace,
			Labels:    Labels(cg),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: SelectorLabels(cg),
			Ports: []corev1.ServicePort{{
				Name:       podtemplate.PortNameGRPC,
				Protocol:   corev1.ProtocolTCP,
				Port:       podtemplate.PortGRPC,
				TargetPort: intstr.FromString(podtemplate.PortNameGRPC),
			}},
		},
	}
	if err := controllerutil.SetControllerReference(cg, svc, scheme); err != nil {
		return nil, fmt.Errorf("set the controller reference of Service %s: %w", svc.Name, err)
	}
	return svc, nil
}
