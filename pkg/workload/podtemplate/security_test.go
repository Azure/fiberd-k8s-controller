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

// capUnion holds each number of sets once, for a want value.
func capUnion(sets ...[]int) []int {
	var out []int
	for _, n := range slices.Concat(sets...) {
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

func TestBuildSecurityContext(t *testing.T) {
	unconfinedSeccomp := &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}
	unconfinedAppArmor := &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined}
	unprivileged := func(nums []int) *corev1.SecurityContext {
		return &corev1.SecurityContext{
			Privileged:      ptr.To(false),
			Capabilities:    &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: capNames(t, nums)},
			SeccompProfile:  unconfinedSeccomp,
			AppArmorProfile: unconfinedAppArmor,
		}
	}
	netRaw := &corev1.Capabilities{Add: []corev1.Capability{"NET_RAW"}}
	engine := []string{"-engine=x"}
	hyperlight := capUnion(caps.Hyperlight, []int{caps.SysAdmin, caps.SetPCAP})
	cases := []struct {
		name    string
		runtime v1alpha1.Runtime
		args    []string
		tenant  *corev1.SecurityContext
		want    *corev1.SecurityContext
	}{
		{
			name:    "proc drops all and adds its measured set",
			runtime: v1alpha1.RuntimeProc,
			want:    unprivileged(caps.Proc),
		},
		{
			name:    "proc with an engine adds the engine set",
			runtime: v1alpha1.RuntimeProc,
			args:    engine,
			want:    unprivileged(capUnion(caps.Proc, caps.Engine)),
		},
		{
			name:    "runc drops all and adds its measured set",
			runtime: v1alpha1.RuntimeRunc,
			want:    unprivileged(caps.Runc),
		},
		{
			name:    "runc with an engine adds the engine set",
			runtime: v1alpha1.RuntimeRunc,
			args:    engine,
			want:    unprivileged(capUnion(caps.Runc, caps.Engine)),
		},
		{
			name:    "gvisor drops all and adds its measured set",
			runtime: v1alpha1.RuntimeGvisor,
			want:    unprivileged(caps.GVisor),
		},
		{
			name:    "gvisor with an engine adds the engine set",
			runtime: v1alpha1.RuntimeGvisor,
			args:    engine,
			want:    unprivileged(capUnion(caps.GVisor, caps.Engine)),
		},
		{
			name:    "hyperlight drops all and adds what fiberd needs before it narrows",
			runtime: v1alpha1.RuntimeHyperlight,
			want:    unprivileged(hyperlight),
		},
		{
			name:    "hyperlight with an engine adds the engine set",
			runtime: v1alpha1.RuntimeHyperlight,
			args:    engine,
			want:    unprivileged(capUnion(hyperlight, caps.Engine)),
		},
		{
			name:    "a tenant's privileged false stays false",
			runtime: v1alpha1.RuntimeProc,
			tenant:  &corev1.SecurityContext{Privileged: ptr.To(false)},
			want:    unprivileged(caps.Proc),
		},
		{
			name:    "tenant capabilities and profiles are replaced on proc",
			runtime: v1alpha1.RuntimeProc,
			tenant: &corev1.SecurityContext{
				Capabilities:    netRaw,
				SeccompProfile:  &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeRuntimeDefault},
			},
			want: unprivileged(caps.Proc),
		},
		{
			name:    "tenant capabilities are replaced on runc",
			runtime: v1alpha1.RuntimeRunc,
			tenant:  &corev1.SecurityContext{Capabilities: netRaw},
			want:    unprivileged(caps.Runc),
		},
		{
			name:    "tenant capabilities are replaced on gvisor",
			runtime: v1alpha1.RuntimeGvisor,
			tenant:  &corev1.SecurityContext{Privileged: ptr.To(false), Capabilities: netRaw},
			want:    unprivileged(caps.GVisor),
		},
		{
			name:    "tenant capabilities are replaced on hyperlight",
			runtime: v1alpha1.RuntimeHyperlight,
			tenant:  &corev1.SecurityContext{Capabilities: netRaw},
			want:    unprivileged(hyperlight),
		},
		{
			name:    "other tenant fields stay",
			runtime: v1alpha1.RuntimeRunc,
			tenant:  &corev1.SecurityContext{RunAsUser: ptr.To(int64(0)), ReadOnlyRootFilesystem: ptr.To(true)},
			want: func() *corev1.SecurityContext {
				sc := unprivileged(caps.Runc)
				sc.RunAsUser = ptr.To(int64(0))
				sc.ReadOnlyRootFilesystem = ptr.To(true)
				return sc
			}(),
		},
		{
			name:    "a tenant seccomp profile is replaced on gvisor",
			runtime: v1alpha1.RuntimeGvisor,
			tenant:  &corev1.SecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			want:    unprivileged(caps.GVisor),
		},
		{
			name:    "allowPrivilegeEscalation false is dropped",
			runtime: v1alpha1.RuntimeProc,
			tenant:  &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false)},
			want:    unprivileged(caps.Proc),
		},
		{
			name:    "allowPrivilegeEscalation true stays",
			runtime: v1alpha1.RuntimeGvisor,
			tenant:  &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(true)},
			want: func() *corev1.SecurityContext {
				sc := unprivileged(caps.GVisor)
				sc.AllowPrivilegeEscalation = ptr.To(true)
				return sc
			}(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cg := testGrant(func(cg *v1alpha1.CapacityGrant) {
				setRuntime(cg, tc.runtime)
				container(cg).Args = tc.args
				container(cg).SecurityContext = tc.tenant
			})
			got := mustBuild(t, cg, testOptions()).Spec.Containers[0].SecurityContext
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("securityContext mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestUnmeasuredRuntimeIsAnError keeps a runtime fiberd has not measured
// from becoming a Pod.
func TestUnmeasuredRuntimeIsAnError(t *testing.T) {
	if sc, err := securityContext(nil, "unmeasured", nil); err == nil {
		t.Errorf("securityContext = %v, want an error", sc)
	}
}

// TestMeasuredCapsHaveNames guards against a fiberd bump that adds a
// capability this package cannot spell.
func TestMeasuredCapsHaveNames(t *testing.T) {
	added := func(rt v1alpha1.Runtime, engine bool) []int {
		t.Helper()
		nums, err := addedCaps(rt, engine)
		if err != nil {
			t.Fatalf("addedCaps(%s, %t): %v", rt, engine, err)
		}
		return nums
	}
	cases := []struct {
		name    string
		nums    []int
		must    corev1.Capability
		wantErr bool
	}{
		{name: "proc", nums: caps.Proc, must: "SYS_ADMIN"},
		{name: "runc", nums: caps.Runc, must: "SETUID"},
		{name: "gvisor", nums: caps.GVisor, must: "SETFCAP"},
		{name: "hyperlight", nums: added(v1alpha1.RuntimeHyperlight, false), must: "SYS_ADMIN"},
		{name: "the engine", nums: caps.Engine, must: "CHOWN"},
		{name: "proc with an engine holds SETPCAP once", nums: added(v1alpha1.RuntimeProc, true), must: "SETPCAP"},
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
			if !slices.Equal(got, slices.Compact(slices.Clone(got))) {
				t.Errorf("capabilityNames = %v, want no repeats", got)
			}
		})
	}
}
