package issuer

import (
	"context"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

// holdsException reports whether the namespace of cg holds the exception for
// the runtime of cg. A runtime that isolates tenants needs no exception, so
// nothing is read for it. Both objects come from the API server, so a label
// removed a moment ago counts at once.
//
// Admission holds only when a grant is written, so this repeats its decision
// when a grant is signed. A missing Namespace or GrantPolicy means no
// exception. Any other read error is a refusal, so no grant is signed on a
// decision that was not made.
func (h *Handler) holdsException(ctx context.Context, cg *v1alpha1.CapacityGrant) (bool, error) {
	if !needsException(cg.Spec.Runtime) {
		return false, nil
	}
	ns := &corev1.Namespace{}
	switch err := h.live.Get(ctx, client.ObjectKey{Name: cg.Namespace}, ns); {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, unavailable(err)
	}
	policy := &v1alpha1.GrantPolicy{}
	switch err := h.live.Get(ctx, client.ObjectKey{Name: v1alpha1.GrantPolicyName}, policy); {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, unavailable(err)
	}
	return exceptionHeld(ns.Labels, policy.Spec.Runtimes, cg.Spec.Runtime), nil
}

// needsException reports whether r shares the host kernel, so a grant of r
// is TRUSTED only with the namespace's exception.
func needsException(r v1alpha1.Runtime) bool {
	return r == v1alpha1.RuntimeProc || r == v1alpha1.RuntimeRunc
}

// exceptionHeld is the decision itself. The labels must name r with the exact
// value, and the ceiling must hold r.
func exceptionHeld(labels map[string]string, ceiling []v1alpha1.Runtime, r v1alpha1.Runtime) bool {
	return labels[v1alpha1.RuntimeLabel(r)] == v1alpha1.LabelRuntimeValue && slices.Contains(ceiling, r)
}
