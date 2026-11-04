package podtemplate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

// AnnotationGrantHash holds Hash of the grant on the Pod template, so the
// controller can tell whether a running Deployment matches the grant.
const AnnotationGrantHash = "fiberd.io/grant-hash"

// hashLen is how many hex characters of the digest Hash keeps. 64 bits is
// far more than change detection between two versions of one grant needs.
const hashLen = 16

// hashInput is everything Hash covers, in a fixed order. Like the
// Deployment controller's pod-template-hash, it includes the tenant's
// template, so any change a tenant makes to the Pod rolls it. Unlike that
// hash, it serializes with encoding/json rather than a struct printer.
// encoding/json sorts map keys and skips unset omitempty fields, so a
// newer k8s.io/api that adds fields does not change the hash of a template
// that leaves them unset.
//
// WriteBudget is the grant's own pointer. Quantity.String caches its
// result inside the Quantity, which would write to the informer's cached
// grant. Quantity.MarshalJSON has a value receiver and writes nothing
// back. An unset budget serializes as null.
type hashInput struct {
	Namespace        string                 `json:"namespace"`
	Zygote           v1alpha1.ZygoteSpec    `json:"zygote"`
	Fibers           v1alpha1.FiberBudget   `json:"fibers"`
	WriteBudget      *resource.Quantity     `json:"writeBudget"`
	CallerThumbprint string                 `json:"callerThumbprint"`
	Runtime          v1alpha1.Runtime       `json:"runtime"`
	Image            string                 `json:"image"`
	PodTemplate      corev1.PodTemplateSpec `json:"podTemplate"`
}

// Hash is a stable hash over the fields that change the grant or the Pod,
// used for the fiberd.io/grant-hash annotation. Same inputs, same hash,
// across controller restarts. An unset runtime hashes as proc, its
// default. It never changes cg. A nil grant, or one that cannot be
// serialized, is an error.
func Hash(cg *v1alpha1.CapacityGrant, image string) (string, error) {
	if cg == nil {
		return "", errors.New("podtemplate: nil CapacityGrant")
	}
	in := hashInput{
		Namespace:        cg.Namespace,
		Zygote:           cg.Spec.Zygote,
		Fibers:           cg.Spec.Fibers,
		WriteBudget:      cg.Spec.WriteBudget,
		CallerThumbprint: cg.Spec.CallerThumbprint,
		Runtime:          runtimeOrDefault(cg.Spec.Runtime),
		Image:            image,
		PodTemplate:      cg.Spec.PodTemplate,
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return "", fmt.Errorf("podtemplate: serialize grant %s/%s for its hash: %w", cg.Namespace, cg.Name, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:hashLen], nil
}

// runtimeOrDefault is the runtime the CRD defaults an unset one to.
func runtimeOrDefault(rt v1alpha1.Runtime) v1alpha1.Runtime {
	if rt == "" {
		return v1alpha1.RuntimeProc
	}
	return rt
}
