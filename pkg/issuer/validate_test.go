package issuer_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/pkg/issuer"
)

func TestValidateSpec(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*v1alpha1.CapacityGrant)
		// wantErr is a substring of the error. Empty means no error.
		wantErr string
	}{
		{
			name: "a valid spec passes",
		},
		{
			// ValidateSpec passes no exception, and that must not refuse a
			// runtime, since trust does not change what a spec may be.
			name:   "proc passes with no exception",
			mutate: func(cg *v1alpha1.CapacityGrant) { cg.Spec.Runtime = v1alpha1.RuntimeProc },
		},
		{
			name:   "runc passes with no exception",
			mutate: func(cg *v1alpha1.CapacityGrant) { cg.Spec.Runtime = v1alpha1.RuntimeRunc },
		},
		{
			// ParseQuantity clamps 16Ei to MaxInt64 without an error, and the
			// mapping refuses it rather than carry a clamped budget.
			name:    "a write budget that overflows is refused",
			mutate:  func(cg *v1alpha1.CapacityGrant) { cg.Spec.WriteBudget = quantity("16Ei") },
			wantErr: "spec.writeBudget",
		},
		{
			name:    "an unset runtime is refused",
			mutate:  func(cg *v1alpha1.CapacityGrant) { cg.Spec.Runtime = "" },
			wantErr: "spec.runtime: Required value",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cg := validGrant()
			if tc.mutate != nil {
				tc.mutate(cg)
			}
			err := issuer.ValidateSpec(cg, testNow, time.Hour)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("ValidateSpec: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("ValidateSpec returned nil, want an error with %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("ValidateSpec error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}
