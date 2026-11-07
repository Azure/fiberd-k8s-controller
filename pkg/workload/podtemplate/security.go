package podtemplate

import (
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"github.com/Azure/fiberd/pkg/sys/caps"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

// securityContext returns the fiberd container's security context for the
// runtime, starting from the tenant's.
//
// proc and runc run unprivileged. They drop every capability and add back
// the set fiberd measured for the runtime, caps.Proc or caps.Runc. Their
// seccomp and AppArmor profiles are Unconfined, because CRIU cannot dump
// from under a filter and the home remounts cgroupfs and sysctls, which
// the default AppArmor profile denies.
//
// gvisor and hyperlight run privileged. A tenant that sets privileged
// gets a privileged Pod on any runtime. Admission policy decides who may,
// not the builder.
//
// The fields above are the controller's. allowPrivilegeEscalation=false
// is dropped too, because the API server refuses it beside privileged or
// CAP_SYS_ADMIN. Every other tenant field, such as runAsUser or
// readOnlyRootFilesystem, stays.
func securityContext(tenant *corev1.SecurityContext, rt v1alpha1.Runtime) (*corev1.SecurityContext, error) {
	sc := tenant.DeepCopy()
	if sc == nil {
		sc = &corev1.SecurityContext{}
	}
	keep, measured := measuredCaps(rt)
	tenantPrivileged := sc.Privileged != nil && *sc.Privileged

	if measured && !tenantPrivileged {
		add, err := capabilityNames(keep)
		if err != nil {
			return nil, err
		}
		sc.Privileged = ptr.To(false)
		sc.Capabilities = &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: add}
	} else {
		sc.Privileged = ptr.To(true)
		sc.Capabilities = nil
	}
	if measured {
		sc.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}
		sc.AppArmorProfile = &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined}
	}
	if sc.AllowPrivilegeEscalation != nil && !*sc.AllowPrivilegeEscalation {
		sc.AllowPrivilegeEscalation = nil
	}
	return sc, nil
}

// measuredCaps is the capability set a Pod of rt runs with, and false for
// a runtime that runs privileged.
func measuredCaps(rt v1alpha1.Runtime) ([]int, bool) {
	switch rt {
	case v1alpha1.RuntimeProc:
		return caps.Proc, true
	case v1alpha1.RuntimeRunc:
		return caps.Runc, true
	}
	return nil, false
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
