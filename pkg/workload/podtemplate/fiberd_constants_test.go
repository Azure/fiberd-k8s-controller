package podtemplate

import (
	"testing"

	"github.com/Azure/fiberd/pkg/core"
	kubehome "github.com/Azure/fiberd/pkg/home/kubernetes"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

// TestConstantsMatchFiberd keeps the controller's copies of fiberd's
// constants equal to fiberd's own. The API package repeats them as
// literals because a CRD marker cannot name a Go constant, and the builder
// keeps its own stale TTL default so that a grant Pod never depends on the
// home's.
func TestConstantsMatchFiberd(t *testing.T) {
	cases := []struct {
		name         string
		ours, theirs any
	}{
		{name: "zygote digest pattern", ours: v1alpha1.DigestPattern, theirs: core.TemplateDigestPattern},
		{name: "zygote-ready Pod condition", ours: v1alpha1.PodConditionZygoteReady, theirs: kubehome.PodConditionZygoteReady},
		{name: "template warm reason", ours: v1alpha1.ReasonTemplateWarm, theirs: kubehome.ReasonTemplateWarm},
		{name: "no warm template reason", ours: v1alpha1.ReasonNoWarmTemplate, theirs: kubehome.ReasonNoWarmTemplate},
		{name: "default stale TTL", ours: DefaultStaleTTL, theirs: kubehome.DefaultStaleTTL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.ours != tc.theirs {
				t.Errorf("controller has %v, fiberd has %v", tc.ours, tc.theirs)
			}
		})
	}
}
