package podtemplate

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

// The Hyperlight helper. A hyperlight Pod gets the helper from the grant's
// helper image through an init container, since tenants may not add init
// containers of their own.
const (
	// helperContainer is the init container that installs the helper.
	helperContainer = "fiberd-helper"

	// helperImageDir is where a helper image keeps the helper and its
	// optional guest.
	helperImageDir = "/fiberd-helper"

	helperVolume = "fiberd-helper"
	helperDir    = "/var/run/fiberd/helper"

	// helperSizeLimit caps the emptyDir the helper is copied into, so the
	// kubelet evicts a Pod whose helper image copies too much instead of
	// letting it fill the node's disk. 128Mi is a start, to be raised if a
	// helper and its guest need more.
	helperSizeLimit = "128Mi"

	// helperFile is fiberd's -hyperlight-helper.
	helperFile = helperDir + "/helper"

	// serviceAccountTokenDir is where the ServiceAccount admission plugin
	// mounts the Pod's token. It skips a container that already has a mount
	// there.
	serviceAccountTokenDir = "/var/run/secrets/kubernetes.io/serviceaccount"

	// helperUID is the uid the init container copies as. It only reads the
	// image and writes the emptyDir, which the kubelet creates writable by
	// all, so it needs no root.
	helperUID int64 = 65532
)

// checkHelperImage refuses a helper image on a runtime other than
// hyperlight, and a hyperlight grant without one, as the CRD rules do.
// fiberd refuses to start a hyperlight home without its helper.
func checkHelperImage(fld *field.Path, rt v1alpha1.Runtime, image string) field.ErrorList {
	switch {
	case rt == v1alpha1.RuntimeHyperlight && image == "":
		return field.ErrorList{field.Required(fld, "runtime hyperlight needs helperImage, fiberd does not start without its helper")}
	case rt != v1alpha1.RuntimeHyperlight && image != "":
		return field.ErrorList{field.Forbidden(fld, "allowed only for runtime hyperlight")}
	}
	return nil
}

// addHelper installs the helper of a hyperlight grant into the Pod and
// mounts it into the fiberd container read-only, so fiberd does not write
// it. Other runtimes get nothing.
//
// The init container runs the helper image itself, because only a
// container of that image can read its files. The contract puts the image
// on fiberd's base image, so cp is there. It copies the whole directory,
// which brings the guest along when the image has one.
//
// The init container has the fiberd container's CPU and memory, which
// keeps the Pod Guaranteed without raising its requests, since the Pod's
// request is the larger of an init container's and the sum of the others.
// It takes no extended resource or claim, which would hand it the grant's
// devices.
//
// The init container needs no API access, but the Pod mounts its service
// account token, which the ServiceAccount admission plugin would add to
// the init container too. A read-only mount at the token's path keeps it
// out. That mount is the helper volume again rather than a volume of its
// own, so no new volume name joins the owned ones. It holds only the
// helper being copied, nothing secret, and the copy does not read it.
func addHelper(spec *corev1.PodSpec, c *corev1.Container, rt v1alpha1.Runtime, image string) {
	if rt != v1alpha1.RuntimeHyperlight {
		return
	}
	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name: helperVolume,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			SizeLimit: ptr.To(resource.MustParse(helperSizeLimit)),
		}},
	})
	sized := corev1.ResourceList{}
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		sized[name] = c.Resources.Limits[name].DeepCopy()
	}
	spec.InitContainers = append(spec.InitContainers, corev1.Container{
		Name:      helperContainer,
		Image:     image,
		Command:   []string{"cp", "-R", helperImageDir + "/.", helperDir},
		Resources: corev1.ResourceRequirements{Limits: sized, Requests: sized.DeepCopy()},
		VolumeMounts: []corev1.VolumeMount{
			{Name: helperVolume, MountPath: helperDir},
			{Name: helperVolume, MountPath: serviceAccountTokenDir, ReadOnly: true},
		},
		SecurityContext: &corev1.SecurityContext{
			Privileged:               ptr.To(false),
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			RunAsNonRoot:             ptr.To(true),
			RunAsUser:                ptr.To(helperUID),
			RunAsGroup:               ptr.To(helperUID),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	})
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: helperVolume, MountPath: helperDir, ReadOnly: true})
}
