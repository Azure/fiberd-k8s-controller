package podtemplate

import (
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"github.com/Azure/fiberd/pkg/sys/caps"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

// capNames spells nums the way Kubernetes does, sorted, for a want value.
func capNames(t *testing.T, nums []int) []corev1.Capability {
	t.Helper()
	var out []corev1.Capability
	for _, n := range nums {
		name, ok := caps.Names[n]
		if !ok {
			t.Fatalf("caps.Names has no name for capability %d", n)
		}
		if !slices.Contains(out, corev1.Capability(name)) {
			out = append(out, corev1.Capability(name))
		}
	}
	slices.Sort(out)
	return out
}

func TestBuildSecurityContext(t *testing.T) {
	unconfinedSeccomp := &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}
	unconfinedAppArmor := &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined}
	measured := func(nums []int) *corev1.SecurityContext {
		return &corev1.SecurityContext{
			Privileged:      ptr.To(false),
			Capabilities:    &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: capNames(t, nums)},
			SeccompProfile:  unconfinedSeccomp,
			AppArmorProfile: unconfinedAppArmor,
		}
	}
	cases := []struct {
		name    string
		runtime v1alpha1.Runtime
		tenant  *corev1.SecurityContext
		want    *corev1.SecurityContext
	}{
		{
			name:    "proc drops all and adds its measured set",
			runtime: v1alpha1.RuntimeProc,
			want:    measured(caps.Proc),
		},
		{
			name:    "runc drops all and adds its measured set",
			runtime: v1alpha1.RuntimeRunc,
			want:    measured(caps.Runc),
		},
		{
			name:    "gvisor runs privileged",
			runtime: v1alpha1.RuntimeGvisor,
			want:    &corev1.SecurityContext{Privileged: ptr.To(true)},
		},
		{
			name:    "hyperlight runs privileged",
			runtime: v1alpha1.RuntimeHyperlight,
			want:    &corev1.SecurityContext{Privileged: ptr.To(true)},
		},
		{
			name:    "a tenant privileged proc Pod stays privileged",
			runtime: v1alpha1.RuntimeProc,
			tenant:  &corev1.SecurityContext{Privileged: ptr.To(true)},
			want: &corev1.SecurityContext{
				Privileged:      ptr.To(true),
				SeccompProfile:  unconfinedSeccomp,
				AppArmorProfile: unconfinedAppArmor,
			},
		},
		{
			name:    "a tenant cannot unprivilege gvisor",
			runtime: v1alpha1.RuntimeGvisor,
			tenant:  &corev1.SecurityContext{Privileged: ptr.To(false), Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_RAW"}}},
			want:    &corev1.SecurityContext{Privileged: ptr.To(true)},
		},
		{
			name:    "tenant capabilities and profiles are replaced on proc",
			runtime: v1alpha1.RuntimeProc,
			tenant: &corev1.SecurityContext{
				Capabilities:    &corev1.Capabilities{Add: []corev1.Capability{"NET_RAW"}},
				SeccompProfile:  &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeRuntimeDefault},
			},
			want: measured(caps.Proc),
		},
		{
			name:    "other tenant fields stay",
			runtime: v1alpha1.RuntimeRunc,
			tenant:  &corev1.SecurityContext{RunAsUser: ptr.To(int64(0)), ReadOnlyRootFilesystem: ptr.To(true)},
			want: func() *corev1.SecurityContext {
				sc := measured(caps.Runc)
				sc.RunAsUser = ptr.To(int64(0))
				sc.ReadOnlyRootFilesystem = ptr.To(true)
				return sc
			}(),
		},
		{
			name:    "a tenant seccomp profile stays on gvisor",
			runtime: v1alpha1.RuntimeGvisor,
			tenant:  &corev1.SecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			want: &corev1.SecurityContext{
				Privileged:     ptr.To(true),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
		},
		{
			name:    "allowPrivilegeEscalation false is dropped",
			runtime: v1alpha1.RuntimeProc,
			tenant:  &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false)},
			want:    measured(caps.Proc),
		},
		{
			name:    "allowPrivilegeEscalation true stays",
			runtime: v1alpha1.RuntimeGvisor,
			tenant:  &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(true)},
			want:    &corev1.SecurityContext{Privileged: ptr.To(true), AllowPrivilegeEscalation: ptr.To(true)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cg := testGrant(func(cg *v1alpha1.CapacityGrant) {
				setRuntime(cg, tc.runtime)
				container(cg).SecurityContext = tc.tenant
			})
			got := mustBuild(t, cg, testOptions()).Spec.Containers[0].SecurityContext
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("securityContext mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestMeasuredCapsHaveNames guards against a fiberd bump that adds a
// capability this package cannot spell.
func TestMeasuredCapsHaveNames(t *testing.T) {
	cases := []struct {
		name    string
		nums    []int
		must    corev1.Capability
		wantErr bool
	}{
		{name: "proc", nums: caps.Proc, must: "SYS_ADMIN"},
		{name: "runc", nums: caps.Runc, must: "SETUID"},
		{name: "a capability without a name", nums: []int{63}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := capabilityNames(tc.nums)
			if tc.wantErr {
				if err == nil {
					t.Errorf("capabilityNames(%v) = %v, want an error", tc.nums, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("capabilityNames: %v", err)
			}
			if !slices.Contains(got, tc.must) {
				t.Errorf("capabilityNames = %v, want it to hold %s", got, tc.must)
			}
			if !slices.IsSorted(got) {
				t.Errorf("capabilityNames = %v, want sorted", got)
			}
		})
	}
}
