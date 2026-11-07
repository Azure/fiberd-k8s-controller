package podtemplate

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

// sizeResources returns the fiberd container's resources. The memory
// limit is the ceiling fiberd carves every fiber from, so its request is
// pinned to it. CPU gets a request but no limit unless the tenant set
// one. GOMAXPROCS follows the CPU limit and a restore burst is CPU-bound,
// so a limit would throttle restores while the node has idle cores.
//
// The Pod is Burstable unless the tenant set a CPU limit equal to its
// request. The kubelet ranks eviction candidates first by whether their
// usage exceeds their request, and memory usage cannot pass a request
// equal to its limit, so the Pod stays near the bottom of the eviction
// order with the Guaranteed Pods. Its OOM score is still a Burstable one.
//
// Limits start as the tenant's. A missing memory limit is an error, since
// no default fits every zygote. An extended resource or hugepages request
// without a limit gets a limit equal to it, since the API server requires
// the two to match for those. Every resource in limits but CPU then gets
// an equal request, replacing the tenant's. A tenant CPU limit keeps the
// tenant's CPU request, or gets an equal one when there is none, as
// Kubernetes does. Without a CPU limit, the CPU request is the tenant's or
// defaultCPU. A tenant request for another resource without a limit, such
// as ephemeral-storage, stays. Claims stay as the tenant wrote them.
func sizeResources(fld *field.Path, in corev1.ResourceRequirements, defaultCPU resource.Quantity) (corev1.ResourceRequirements, field.ErrorList) {
	var errs field.ErrorList
	limits := in.Limits.DeepCopy()
	if limits == nil {
		limits = corev1.ResourceList{}
	}
	for name, req := range in.Requests {
		if _, ok := limits[name]; !ok && needsEqualLimit(name) {
			limits[name] = req.DeepCopy()
		}
	}
	switch q, ok := limits[corev1.ResourceMemory]; {
	case !ok:
		errs = append(errs, field.Required(fld.Child("limits").Key(string(corev1.ResourceMemory)), "a grant Pod needs a memory limit, its fibers share it"))
	case q.Sign() <= 0:
		errs = append(errs, field.Invalid(fld.Child("limits").Key(string(corev1.ResourceMemory)), q.String(), "must be positive"))
	}

	cpuLimit, hasCPULimit := limits[corev1.ResourceCPU]
	cpu, hasCPU := in.Requests[corev1.ResourceCPU]
	switch {
	case !hasCPU && hasCPULimit:
		cpu = cpuLimit
	case !hasCPU:
		cpu = defaultCPU
	}
	if hasCPULimit && cpuLimit.Sign() <= 0 {
		errs = append(errs, field.Invalid(fld.Child("limits").Key(string(corev1.ResourceCPU)), cpuLimit.String(), "must be positive"))
	}
	switch {
	case hasCPU && cpu.Sign() <= 0:
		errs = append(errs, field.Invalid(fld.Child("requests").Key(string(corev1.ResourceCPU)), cpu.String(), "must be positive"))
	case hasCPU && hasCPULimit && cpu.Cmp(cpuLimit) > 0:
		errs = append(errs, field.Invalid(fld.Child("requests").Key(string(corev1.ResourceCPU)), cpu.String(), "must not exceed the CPU limit "+cpuLimit.String()))
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
	requests[corev1.ResourceCPU] = cpu.DeepCopy()
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
