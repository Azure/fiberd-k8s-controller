package issuer

import (
	"time"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

// specCheckPod stands in for a grant Pod when only the spec is checked.
// fiberd core validates a complete grant, which needs a UID and an
// audience. No Pod exists when the controller checks a spec, so this
// identity fills both with valid values that no real Pod carries.
var specCheckPod = PodIdentity{
	UID:  "00000000-0000-0000-0000-000000000000",
	Name: "fiberd-spec-check",
}

// ValidateSpec reports whether every Pod of cg could be given a grant. It
// maps cg as GrantFromSpec does, for a stand-in Pod, so it refuses exactly
// the specs GrantFromSpec and fiberd core refuse. now and lease are as for
// GrantFromSpec. cg is never modified.
func ValidateSpec(cg *v1alpha1.CapacityGrant, now time.Time, lease time.Duration) error {
	_, err := GrantFromSpec(cg, specCheckPod, now, lease)
	return err
}
