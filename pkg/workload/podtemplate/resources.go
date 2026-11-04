package podtemplate

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

// guaranteed returns the container's resources in the Guaranteed QoS
// class. The container's memory limit is the ceiling fiberd carves every
// fiber from, and a Guaranteed Pod is the last the kubelet evicts.
//
// Limits start as the tenant's. A missing CPU limit becomes the tenant's
// CPU request, or defaultCPU when the tenant set neither. A missing memory
// limit is an error, since no default fits every zygote. An extended
// resource or hugepages request without a limit gets a limit equal to it,
// since the API server requires the two to match for those. Every
// resource in limits then gets an equal request, replacing the tenant's.
// A tenant request for another resource without a limit, such as
// ephemeral-storage, stays. Claims stay as the tenant wrote them.
func guaranteed(fld *field.Path, in corev1.ResourceRequirements, defaultCPU resource.Quantity) (corev1.ResourceRequirements, field.ErrorList) {
	var errs field.ErrorList
	limits := in.Limits.DeepCopy()
	if limits == nil {
		limits = corev1.ResourceList{}
	}
	if _, ok := limits[corev1.ResourceCPU]; !ok {
		if req, ok := in.Requests[corev1.ResourceCPU]; ok {
			limits[corev1.ResourceCPU] = req.DeepCopy()
		} else {
			limits[corev1.ResourceCPU] = defaultCPU.DeepCopy()
		}
	}
	for name, req := range in.Requests {
		if _, ok := limits[name]; !ok && needsEqualLimit(name) {
			limits[name] = req.DeepCopy()
		}
	}
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		q, ok := limits[name]
		switch {
		case !ok:
			errs = append(errs, field.Required(fld.Child("limits").Key(string(name)), "a grant Pod needs a memory limit, its fibers share it"))
		case q.Sign() <= 0:
			errs = append(errs, field.Invalid(fld.Child("limits").Key(string(name)), q.String(), "must be positive"))
		}
	}
	if len(errs) > 0 {
		return corev1.ResourceRequirements{}, errs
	}

	requests := in.Requests.DeepCopy()
	if requests == nil {
		requests = corev1.ResourceList{}
	}
	for name, q := range limits {
		requests[name] = q.DeepCopy()
	}
	out := corev1.ResourceRequirements{Limits: limits, Requests: requests}
	if in.Claims != nil {
		out.Claims = append([]corev1.ResourceClaim(nil), in.Claims...)
	}
	return out, nil
}

// NeedsExclusiveDevice reports whether the template requests an extended
// resource or a resource claim, which forces the Recreate strategy. A
// rolling update would start the new Pod while the old one still holds
// the device, and the new one would never schedule.
func NeedsExclusiveDevice(cg *v1alpha1.CapacityGrant) bool {
	if cg == nil {
		return false
	}
	spec := &cg.Spec.PodTemplate.Spec
	if len(spec.ResourceClaims) > 0 {
		return true
	}
	for i := range spec.Containers {
		r := &spec.Containers[i].Resources
		for _, list := range []corev1.ResourceList{r.Limits, r.Requests} {
			for name := range list {
				if isExtendedResource(name) {
					return true
				}
			}
		}
	}
	return false
}

// needsEqualLimit reports whether the API server requires a limit equal to
// the request for name. That holds for extended resources and hugepages,
// which cannot be overcommitted.
func needsEqualLimit(name corev1.ResourceName) bool {
	return isExtendedResource(name) || strings.HasPrefix(string(name), corev1.ResourceHugePagesPrefix)
}

// isExtendedResource follows Kubernetes' rule. An extended resource has a
// domain prefix outside kubernetes.io. A name that starts with "requests."
// names a quota, not a resource, so it is never extended.
func isExtendedResource(name corev1.ResourceName) bool {
	s := string(name)
	domain, _, ok := strings.Cut(s, "/")
	if !ok || strings.HasPrefix(s, corev1.DefaultResourceRequestsPrefix) {
		return false
	}
	return domain != "kubernetes.io" && !strings.HasSuffix(domain, ".kubernetes.io")
}
