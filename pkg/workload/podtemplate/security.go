package podtemplate

import (
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"github.com/Azure/fiberd/pkg/sys/caps"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

// hyperlightStartCaps are what fiberd needs on hyperlight before it narrows.
// It remounts cgroupfs with SYS_ADMIN and narrows with SETPCAP before any
// helper exists, and it holds neither afterwards unless the runtime's set
// does.
var hyperlightStartCaps = []int{caps.SysAdmin, caps.SetPCAP}

// securityContext returns the fiberd container's security context for the
// runtime and the container's args, starting from the tenant's.
//
// Every runtime runs unprivileged. It drops every capability and adds back
// the set fiberd measured for it. Hyperlight also holds what fiberd needs
// before it narrows, and args that set an engine add caps.Engine.
//
// Every runtime's seccomp and AppArmor profiles are Unconfined. CRIU on proc
// and runc cannot dump from under a filter, and fiberd remounts cgroupfs and
// sysctls on every runtime, which the default AppArmor profile denies.
//
// A tenant that sets privileged gets a privileged Pod on any runtime.
// Admission policy decides who may, not the builder.
//
// The fields above are the controller's. allowPrivilegeEscalation=false
// is dropped too, because the API server refuses it beside privileged or
// CAP_SYS_ADMIN. Every other tenant field, such as runAsUser or
// readOnlyRootFilesystem, stays.
func securityContext(tenant *corev1.SecurityContext, rt v1alpha1.Runtime, args []string) (*corev1.SecurityContext, error) {
	sc := tenant.DeepCopy()
	if sc == nil {
		sc = &corev1.SecurityContext{}
	}
	nums, err := addedCaps(rt, engineSet(args))
	if err != nil {
		return nil, err
	}
	add, err := capabilityNames(nums)
	if err != nil {
		return nil, err
	}
	if sc.Privileged != nil && *sc.Privileged {
		sc.Capabilities = nil
	} else {
		sc.Privileged = ptr.To(false)
		sc.Capabilities = &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: add}
	}
	sc.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}
	sc.AppArmorProfile = &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined}
	if sc.AllowPrivilegeEscalation != nil && !*sc.AllowPrivilegeEscalation {
		sc.AllowPrivilegeEscalation = nil
	}
	return sc, nil
}

// addedCaps is the capability set a Pod of rt adds back, which may hold
// repeats. A runtime fiberd has not measured is an error, so it never
// becomes a privileged Pod.
func addedCaps(rt v1alpha1.Runtime, engine bool) ([]int, error) {
	added, measured := caps.ForRuntime(string(rt))
	if !measured {
		return nil, fmt.Errorf("podtemplate: fiberd has not measured runtime %q", rt)
	}
	if rt == v1alpha1.RuntimeHyperlight {
		added = slices.Concat(added, hyperlightStartCaps)
	}
	if engine {
		added = slices.Concat(added, caps.Engine)
	}
	return added, nil
}

// capabilityNames spells capability numbers as Kubernetes does, sorted
// and without repeats. A number caps.Names lacks is an error rather than
// a capability silently left out.
func capabilityNames(nums []int) ([]corev1.Capability, error) {
	out := make([]corev1.Capability, 0, len(nums))
	for _, n := range nums {
		name, ok := caps.Names[n]
		if !ok {
			return nil, fmt.Errorf("podtemplate: fiberd capability %d has no Kubernetes name", n)
		}
		out = append(out, corev1.Capability(name))
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}
