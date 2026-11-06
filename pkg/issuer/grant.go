// Package issuer turns a CapacityGrant into the grant fiberd verifies and
// stores the controller's key ring in a Secret. fiberd core validates and
// signs the grant and owns the ring itself.
package issuer

import (
	"fmt"
	"math"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/Azure/fiberd/pkg/core"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

// PodIdentity names the Pod a grant is minted for.
type PodIdentity struct {
	// UID is the Pod UID. It becomes the grant UID.
	UID string
	// Name is the Pod name. It becomes the grant audience.
	Name string
}

// GrantFromSpec builds the grant fiberd will verify for one Pod of cg. now
// and lease fix the lease expiry, which is cut to whole seconds because
// the token's exp claim holds whole seconds. Issuer is left empty because
// grant.Issuer.Mint sets it. cg is never modified.
//
// GrantFromSpec maps fields and leaves the rules a grant must follow to
// core.Grant.Validate, so the controller and every other issuer refuse the
// same grants. It checks only what the mapping itself needs. Those checks
// cover a CapacityGrant, a namespace, a caller thumbprint and a runtime
// that are set, a Pod name to use as the audience, a fiber max of at least
// one, a write budget that is a whole number of bytes, and a positive
// lease of whole seconds.
// When the grant cannot be built, GrantFromSpec returns the zero grant and
// an error that names the CapacityGrant.
//
// now must not be later than the clock of the grant.Issuer that mints the
// grant. fiberd measures the lease from the iat claim, which the Issuer
// sets at mint time, so a now ahead of that clock makes the lease fiberd
// sees longer than lease.
func GrantFromSpec(cg *v1alpha1.CapacityGrant, pod PodIdentity, now time.Time, lease time.Duration) (core.Grant, error) {
	if cg == nil {
		return core.Grant{}, fmt.Errorf("issuer: %w", field.Required(field.NewPath("capacityGrant"), "the CapacityGrant is nil"))
	}
	specPath := field.NewPath("spec")
	spec := &cg.Spec

	errs := validateAudience(field.NewPath("pod", "metadata", "name"), pod.Name)
	if cg.Namespace == "" {
		// An empty tenant would silently drop the grant to anonymous fibers.
		errs = append(errs, field.Required(field.NewPath("metadata", "namespace"), "the namespace becomes the tenant"))
	}
	// fiberd reads a max of zero as unlimited, and core.Grant.Validate
	// allows that. A CapacityGrant always bounds its fibers, so a zero here
	// must never reach fiberd.
	if spec.Fibers.Max < 1 {
		errs = append(errs, field.Invalid(specPath.Child("fibers", "max"), spec.Fibers.Max, "must be at least 1"))
	}
	// A grant Pod serves mutual TLS only, and fiberd there refuses every
	// grant bound to no caller. The CRD requires the field, but an object
	// stored before the CRD required it can still lack it.
	if spec.CallerThumbprint == "" {
		errs = append(errs, field.Required(specPath.Child("callerThumbprint"), "a grant Pod serves mutual TLS only and refuses a grant bound to no caller"))
	}
	writeBudget, budgetErrs := budgetBytes(specPath.Child("writeBudget"), spec.WriteBudget)
	errs = append(errs, budgetErrs...)
	isolation, rtErrs := isolationFor(specPath.Child("runtime"), spec.Runtime)
	errs = append(errs, rtErrs...)
	// fiberd measures the lease from the iat claim, and both claims hold
	// whole seconds. A fractional lease can then come out one second
	// longer than a home's MaxLease equal to it.
	switch {
	case lease <= 0:
		errs = append(errs, field.Invalid(field.NewPath("lease"), lease.String(), "must be positive"))
	case lease%time.Second != 0:
		errs = append(errs, field.Invalid(field.NewPath("lease"), lease.String(), "must be a whole number of seconds"))
	}
	if len(errs) > 0 {
		return core.Grant{}, grantError(cg, errs.ToAggregate())
	}

	g := core.Grant{
		UID:            pod.UID,
		Audience:       pod.Name,
		TemplateDigest: spec.Zygote.Digest,
		Tenant:         cg.Namespace,
		FiberMax:       int(spec.Fibers.Max),
		FiberWarm:      int(spec.Fibers.Warm),
		WBudgetBytes:   writeBudget,
		// The controller built the grant Pod, so the grant does not need to
		// ask for more than the lowest tier.
		MinTier:     core.TierBasic,
		LeaseExpiry: now.Add(lease).Truncate(time.Second),
		Policy: core.Policy{
			Durability:   core.BestEffort,
			Isolation:    isolation,
			EndpointMode: core.EndpointDirect,
		},
		CallerThumbprint: spec.CallerThumbprint,
	}
	if err := g.Validate(); err != nil {
		return core.Grant{}, grantError(cg, err)
	}
	return g, nil
}

// grantError wraps err with the namespace and name of cg.
func grantError(cg *v1alpha1.CapacityGrant, err error) error {
	return fmt.Errorf("issuer: CapacityGrant %s/%s: %w", cg.Namespace, cg.Name, err)
}

// validateAudience checks the Pod name that becomes the audience.
// core.Grant.Validate does not check the audience, and a Pod name is
// always a DNS-1123 subdomain.
func validateAudience(fld *field.Path, name string) field.ErrorList {
	if name == "" {
		return field.ErrorList{field.Required(fld, "the Pod name becomes the audience")}
	}
	if msgs := validation.IsDNS1123Subdomain(name); len(msgs) > 0 {
		return field.ErrorList{field.Invalid(fld, name, strings.Join(msgs, ", "))}
	}
	return nil
}

// budgetBytes reads q as a whole number of bytes. Nil means no limit,
// which fiberd spells as zero.
func budgetBytes(fld *field.Path, q *resource.Quantity) (uint64, field.ErrorList) {
	if q == nil {
		return 0, nil
	}
	// String caches its result and Cmp can convert a Quantity to its
	// decimal form, both in place. The checks run on a copy so the
	// caller's object stays as it was.
	v := q.DeepCopy()
	s := v.String()
	if v.Sign() < 0 {
		return 0, field.ErrorList{field.Invalid(fld, s, "must not be negative")}
	}
	// Quantity reads out at most an int64, which is far above any real
	// memory size. ParseQuantity clamps a binary-suffixed value above that,
	// such as 16Ei, to MaxInt64 without an error. A budget at MaxInt64 may
	// therefore not be what the spec says, so it is refused too.
	if v.Cmp(*resource.NewQuantity(math.MaxInt64, resource.BinarySI)) >= 0 {
		return 0, field.ErrorList{field.Invalid(fld, s, fmt.Sprintf("must be below %d bytes, the largest value a Quantity holds without clamping", int64(math.MaxInt64)))}
	}
	// Value rounds up, so it equals v only when v is a whole number.
	n := v.Value()
	if v.Cmp(*resource.NewQuantity(n, resource.BinarySI)) != 0 {
		return 0, field.ErrorList{field.Invalid(fld, s, "must be a whole number of bytes")}
	}
	return uint64(n), nil
}

// isolationFor says whether a runtime keeps fibers off the host kernel.
// gVisor and Hyperlight do, so they may run untrusted code. proc and runc
// share the host kernel and are only for trusted code. An unset runtime
// is refused instead of read as proc, because the API server always
// defaults it and an object without it skipped admission.
func isolationFor(fld *field.Path, r v1alpha1.Runtime) (core.Isolation, field.ErrorList) {
	switch r {
	case v1alpha1.RuntimeGvisor, v1alpha1.RuntimeHyperlight:
		return core.Untrusted, nil
	case v1alpha1.RuntimeProc, v1alpha1.RuntimeRunc:
		return core.Trusted, nil
	case "":
		return core.IsolationUnspecified, field.ErrorList{field.Required(fld, "")}
	default:
		return core.IsolationUnspecified, field.ErrorList{field.NotSupported(fld, r, []v1alpha1.Runtime{
			v1alpha1.RuntimeProc, v1alpha1.RuntimeRunc, v1alpha1.RuntimeGvisor, v1alpha1.RuntimeHyperlight,
		})}
	}
}
