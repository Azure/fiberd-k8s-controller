package podtemplate

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

// TestBuildHelper checks the init container, volume and mount that put the
// helper into a hyperlight Pod. The init container is the controller's
// whatever the tenant's Pod security context says, it takes only the
// fiberd container's CPU and memory, and it holds the service account
// token's path so no token is mounted into it.
func TestBuildHelper(t *testing.T) {
	cg := testGrant(func(cg *v1alpha1.CapacityGrant) {
		setRuntime(cg, v1alpha1.RuntimeHyperlight)
		cg.Spec.PodTemplate.Spec.SecurityContext = &corev1.PodSecurityContext{
			RunAsUser:      ptr.To(int64(0)),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
		}
		container(cg).Resources.Limits[corev1.ResourceCPU] = resource.MustParse("2")
		container(cg).Resources.Limits["example.com/gpu"] = resource.MustParse("1")
		container(cg).Resources.Claims = []corev1.ResourceClaim{{Name: "gpu"}}
	})
	spec := mustBuild(t, cg, testOptions()).Spec

	sized := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("2"),
		corev1.ResourceMemory: resource.MustParse("1Gi"),
	}
	wantInit := []corev1.Container{{
		Name:      "fiberd-helper",
		Image:     testHelperImage,
		Command:   []string{"cp", "-R", "/fiberd-helper/.", "/var/run/fiberd/helper"},
		Resources: corev1.ResourceRequirements{Limits: sized, Requests: sized},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "fiberd-helper", MountPath: "/var/run/fiberd/helper"},
			// Taken so the ServiceAccount admission plugin mounts no token.
			{Name: "fiberd-helper", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true},
		},
		SecurityContext: &corev1.SecurityContext{
			Privileged:               ptr.To(false),
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			RunAsNonRoot:             ptr.To(true),
			RunAsUser:                ptr.To(int64(65532)),
			RunAsGroup:               ptr.To(int64(65532)),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	}}
	if diff := cmp.Diff(wantInit, spec.InitContainers); diff != "" {
		t.Errorf("initContainers mismatch (-want +got):\n%s", diff)
	}

	wantVolume := corev1.Volume{Name: "fiberd-helper", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
		SizeLimit: ptr.To(resource.MustParse("128Mi")),
	}}}
	if diff := cmp.Diff(wantVolume, spec.Volumes[len(spec.Volumes)-1]); diff != "" {
		t.Errorf("last volume mismatch (-want +got):\n%s", diff)
	}

	// Read-only, so fiberd does not write the helper.
	c := spec.Containers[0]
	wantMount := corev1.VolumeMount{Name: "fiberd-helper", MountPath: "/var/run/fiberd/helper", ReadOnly: true}
	if diff := cmp.Diff(wantMount, c.VolumeMounts[len(c.VolumeMounts)-1]); diff != "" {
		t.Errorf("last fiberd volumeMount mismatch (-want +got):\n%s", diff)
	}
	if !slices.Contains(c.Args, "-hyperlight-helper="+wantMount.MountPath+"/helper") {
		t.Errorf("fiberd args %q lack -hyperlight-helper at the mounted helper", c.Args)
	}
	if diff := cmp.Diff(cg.Spec.PodTemplate.Spec.SecurityContext, spec.SecurityContext); diff != "" {
		t.Errorf("the tenant's Pod securityContext changed (-want +got):\n%s", diff)
	}
}

// TestBuildHelperOnlyHyperlight checks that no other runtime gets the
// helper's init container, volume, mount or flag.
func TestBuildHelperOnlyHyperlight(t *testing.T) {
	for _, rt := range []v1alpha1.Runtime{v1alpha1.RuntimeProc, v1alpha1.RuntimeRunc, v1alpha1.RuntimeGvisor} {
		t.Run(string(rt), func(t *testing.T) {
			cg := testGrant(func(cg *v1alpha1.CapacityGrant) { setRuntime(cg, rt) })
			spec := mustBuild(t, cg, testOptions()).Spec
			if len(spec.InitContainers) != 0 {
				t.Errorf("initContainers = %v, want none", spec.InitContainers)
			}
			if slices.ContainsFunc(spec.Volumes, func(v corev1.Volume) bool { return v.Name == helperVolume }) {
				t.Errorf("volumes hold %s", helperVolume)
			}
			c := spec.Containers[0]
			if slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == helperVolume }) {
				t.Errorf("fiberd mounts %s", helperVolume)
			}
			if slices.ContainsFunc(c.Args, func(a string) bool { return strings.HasPrefix(a, "-hyperlight-helper") }) {
				t.Errorf("fiberd args %q hold -hyperlight-helper", c.Args)
			}
		})
	}
}
