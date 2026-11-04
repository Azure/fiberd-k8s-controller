// Package podtemplate turns a CapacityGrant's pod template into the one the
// controller applies as its Deployment's template.
//
// The tenant writes spec.podTemplate as in a Deployment, with one container
// named fiberd. Build lays a fixed overlay on top of a copy of it and
// leaves every other field as the tenant wrote it. The overlay sets only
// what fiberd needs to run as a grant home. That is the labels the
// controller selects on, the service account, the readiness gate, two
// volumes, the image, the Pod identity variables, the gRPC port, the
// fiberd flags, the probes, Guaranteed resources and the security context
// of the runtime. Where a tenant field would collide with the overlay,
// such as a volume of the same name or a probe, Build refuses the grant
// instead of picking a winner silently. It refuses Pod-level resources
// too, since they would break the Guaranteed QoS class.
//
// The Pod runs fiberd's own image, whose entrypoint runs fiberd under tini.
// The first controller flag, -home=kubernetes, selects the Kubernetes home
// inside fiberd. Nothing is added to the image, and the builder sets no
// container command.
//
// proc and runc Pods run with Unconfined seccomp and AppArmor profiles.
// fiberd has no seccomp profile of its own yet.
package podtemplate

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

// The fiberd container and what the overlay adds to the Pod.
const (
	// ContainerName is the one container a grant Pod template holds.
	ContainerName = "fiberd"

	// PortGRPC is the port fiberd serves its Fibers API on.
	PortGRPC int32 = 8484

	// PortNameGRPC names PortGRPC on the container.
	PortNameGRPC = "grpc"

	stateVolume     = "fiberd-state"
	stateDir        = "/var/lib/fiberd"
	grantsVolume    = "fiberd-grants"
	grantsDir       = "/var/run/fiberd/grants"
	grantsSizeLimit = "1Mi"

	// rootfsDir is where the runc and gVisor images keep the rootfs their
	// sandboxes run in.
	rootfsDir = "/usr/share/fiberd/rootfs"

	// fiberdBinary is the fiberd binary the image's entrypoint runs under
	// tini. The liveness probe runs its healthz subcommand.
	fiberdBinary = "/usr/local/bin/fiberd"

	// fiberdHome is the fiberd home a grant Pod selects with -home.
	fiberdHome = "kubernetes"

	envPodName      = "POD_NAME"
	envPodNamespace = "POD_NAMESPACE"
	envPodUID       = "POD_UID"
)

// Build returns the pod template the controller applies for cg, with image
// already resolved by the caller. It never changes cg. When the grant
// cannot run, the error is a field error list that names every field at
// fault, ready for the grant's status.
func Build(cg *v1alpha1.CapacityGrant, image string, opts Options) (corev1.PodTemplateSpec, error) {
	if cg == nil {
		return corev1.PodTemplateSpec{}, errors.New("podtemplate: nil CapacityGrant")
	}
	if image == "" {
		return corev1.PodTemplateSpec{}, fmt.Errorf("podtemplate: grant %s/%s has no resolved image", cg.Namespace, cg.Name)
	}
	opts = opts.withDefaults()
	if err := opts.Validate(); err != nil {
		return corev1.PodTemplateSpec{}, err
	}

	specPath := field.NewPath("spec")
	podPath := specPath.Child("podTemplate", "spec")
	tmpl := cg.Spec.PodTemplate.DeepCopy()

	rt, errs := checkRuntime(specPath.Child("runtime"), cg.Spec.Runtime)
	tflag, tErrs := templateFlag(specPath.Child("zygote"), cg.Spec.Zygote)
	errs = append(errs, tErrs...)
	errs = append(errs, checkShape(podPath, &tmpl.Spec)...)
	if len(errs) > 0 {
		return corev1.PodTemplateSpec{}, errs.ToAggregate()
	}

	// checkShape has made sure there is exactly one container.
	cPath := podPath.Child("containers").Index(0)
	c := &tmpl.Spec.Containers[0]
	errs = append(errs, checkArgs(cPath.Child("args"), c.Args)...)
	errs = append(errs, checkVolumes(podPath, &tmpl.Spec)...)
	errs = append(errs, checkEnv(cPath.Child("env"), c.Env)...)
	errs = append(errs, checkPorts(cPath.Child("ports"), c.Ports)...)
	errs = append(errs, checkProbes(cPath, c)...)
	resources, rErrs := guaranteed(cPath.Child("resources"), c.Resources, opts.DefaultCPU)
	errs = append(errs, rErrs...)
	if len(errs) > 0 {
		return corev1.PodTemplateSpec{}, errs.ToAggregate()
	}

	grantHash, err := Hash(cg, image)
	if err != nil {
		return corev1.PodTemplateSpec{}, err
	}
	overlayMeta(&tmpl.ObjectMeta, cg, grantHash)
	overlayPodSpec(&tmpl.Spec, opts.ServiceAccount)
	addVolumes(&tmpl.Spec)

	c.Image = image
	c.Env = append(c.Env, podEnv()...)
	c.Args = append(c.Args, controllerArgs(rt, tflag, opts)...)
	c.Ports = append(c.Ports, corev1.ContainerPort{Name: PortNameGRPC, ContainerPort: PortGRPC, Protocol: corev1.ProtocolTCP})
	c.VolumeMounts = append(c.VolumeMounts,
		corev1.VolumeMount{Name: stateVolume, MountPath: stateDir},
		corev1.VolumeMount{Name: grantsVolume, MountPath: grantsDir},
	)
	setProbes(c)
	c.Resources = resources
	if c.SecurityContext, err = securityContext(c.SecurityContext, rt); err != nil {
		return corev1.PodTemplateSpec{}, err
	}
	return *tmpl, nil
}

// checkRuntime returns the grant's runtime, proc when unset as the CRD
// defaults it.
func checkRuntime(fld *field.Path, rt v1alpha1.Runtime) (v1alpha1.Runtime, field.ErrorList) {
	rt = runtimeOrDefault(rt)
	switch rt {
	case v1alpha1.RuntimeProc, v1alpha1.RuntimeRunc, v1alpha1.RuntimeGvisor, v1alpha1.RuntimeHyperlight:
		return rt, nil
	}
	return "", field.ErrorList{field.NotSupported(fld, rt, []v1alpha1.Runtime{
		v1alpha1.RuntimeProc, v1alpha1.RuntimeRunc, v1alpha1.RuntimeGvisor, v1alpha1.RuntimeHyperlight,
	})}
}

// checkShape repeats the CRD's validation rules on the pod spec. The API
// server enforces them on admission. The builder checks them again so a
// grant stored before a rule existed, or written past the rules, still
// cannot reach the Pod. It also refuses Pod-level resources, which no CRD
// rule covers.
func checkShape(fld *field.Path, spec *corev1.PodSpec) field.ErrorList {
	var errs field.ErrorList
	if len(spec.Containers) != 1 || spec.Containers[0].Name != ContainerName {
		errs = append(errs, field.Invalid(fld.Child("containers"), containerNames(spec.Containers), "must hold exactly one container named "+ContainerName))
	} else if len(spec.Containers[0].Command) > 0 {
		errs = append(errs, field.Forbidden(fld.Child("containers").Index(0).Child("command"), "the image entrypoint runs fiberd"))
	}
	if len(spec.InitContainers) > 0 {
		errs = append(errs, field.Forbidden(fld.Child("initContainers"), "not allowed in a grant Pod"))
	}
	if len(spec.EphemeralContainers) > 0 {
		errs = append(errs, field.Forbidden(fld.Child("ephemeralContainers"), "not allowed in a grant Pod"))
	}
	if spec.ServiceAccountName != "" {
		errs = append(errs, field.Forbidden(fld.Child("serviceAccountName"), "set by the controller"))
	}
	if spec.DeprecatedServiceAccount != "" {
		errs = append(errs, field.Forbidden(fld.Child("serviceAccount"), "set by the controller"))
	}
	if spec.NodeName != "" {
		errs = append(errs, field.Forbidden(fld.Child("nodeName"), "the scheduler places the Pod"))
	}
	if spec.RestartPolicy != "" && spec.RestartPolicy != corev1.RestartPolicyAlways {
		errs = append(errs, field.NotSupported(fld.Child("restartPolicy"), spec.RestartPolicy, []corev1.RestartPolicy{corev1.RestartPolicyAlways}))
	}
	if spec.HostPID {
		errs = append(errs, field.Forbidden(fld.Child("hostPID"), "set by the controller"))
	}
	if spec.HostNetwork {
		errs = append(errs, field.Forbidden(fld.Child("hostNetwork"), "set by the controller"))
	}
	if spec.HostIPC {
		errs = append(errs, field.Forbidden(fld.Child("hostIPC"), "set by the controller"))
	}
	if spec.ShareProcessNamespace != nil && *spec.ShareProcessNamespace {
		errs = append(errs, field.Forbidden(fld.Child("shareProcessNamespace"), "set by the controller"))
	}
	if spec.HostUsers != nil {
		errs = append(errs, field.Forbidden(fld.Child("hostUsers"), "set by the controller"))
	}
	if spec.Resources != nil {
		errs = append(errs, field.Forbidden(fld.Child("resources"), "the controller sizes the Pod through the fiberd container, and Pod-level resources would break its Guaranteed QoS class"))
	}
	return errs
}

// containerNames lists the names of cs, for an error message.
func containerNames(cs []corev1.Container) []string {
	names := make([]string, 0, len(cs))
	for _, c := range cs {
		names = append(names, c.Name)
	}
	return names
}

// checkEnv refuses tenant variables that would shadow the ones the
// overlay adds. -node-id expands POD_NAME, so a tenant value would change
// fiberd's identity.
func checkEnv(fld *field.Path, env []corev1.EnvVar) field.ErrorList {
	var errs field.ErrorList
	for i, e := range env {
		switch e.Name {
		case envPodName, envPodNamespace, envPodUID:
			errs = append(errs, field.Forbidden(fld.Index(i).Child("name"), e.Name+" is set by the controller"))
		}
	}
	return errs
}

// checkPorts refuses a tenant port that takes the name or number of
// fiberd's gRPC port.
func checkPorts(fld *field.Path, ports []corev1.ContainerPort) field.ErrorList {
	var errs field.ErrorList
	for i, p := range ports {
		if p.Name == PortNameGRPC {
			errs = append(errs, field.Duplicate(fld.Index(i).Child("name"), p.Name))
		}
		if p.ContainerPort == PortGRPC && (p.Protocol == "" || p.Protocol == corev1.ProtocolTCP) {
			errs = append(errs, field.Duplicate(fld.Index(i).Child("containerPort"), p.ContainerPort))
		}
	}
	return errs
}

// overlayMeta adds the grant labels and the hash annotation. They are set
// after the tenant's, so a tenant value for one of these keys loses. Every
// other tenant label and annotation stays.
func overlayMeta(meta *metav1.ObjectMeta, cg *v1alpha1.CapacityGrant, grantHash string) {
	if meta.Labels == nil {
		meta.Labels = map[string]string{}
	}
	meta.Labels[v1alpha1.LabelGrant] = cg.Name
	meta.Labels[v1alpha1.LabelGrantUID] = string(cg.UID)
	meta.Labels[v1alpha1.LabelManagedBy] = v1alpha1.ManagedBy

	if meta.Annotations == nil {
		meta.Annotations = map[string]string{}
	}
	meta.Annotations[AnnotationGrantHash] = grantHash
}

// overlayPodSpec sets the Pod fields the controller owns. Scheduling,
// image pull secrets, priority and every other tenant field pass through.
// The service account token is mounted because the home reads its Pod and
// patches its status through it. fiberd hides that path from fibers by
// default.
func overlayPodSpec(spec *corev1.PodSpec, serviceAccount string) {
	spec.ServiceAccountName = serviceAccount
	spec.AutomountServiceAccountToken = ptr.To(true)
	spec.RestartPolicy = corev1.RestartPolicyAlways
	gate := corev1.PodReadinessGate{ConditionType: v1alpha1.PodConditionZygoteReady}
	if !slices.Contains(spec.ReadinessGates, gate) {
		spec.ReadinessGates = append(spec.ReadinessGates, gate)
	}
}

// podEnv is the Pod's identity through the downward API. The Kubernetes
// home reads it, and -node-id expands POD_NAME.
func podEnv() []corev1.EnvVar {
	from := func(name, path string) corev1.EnvVar {
		return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: path},
		}}
	}
	return []corev1.EnvVar{
		from(envPodName, "metadata.name"),
		from(envPodNamespace, "metadata.namespace"),
		from(envPodUID, "metadata.uid"),
	}
}

// checkVolumes refuses tenant volumes, mounts and devices that collide
// with fiberd's state and grants volumes. A tenant mount of either volume
// would expose fiberd's keys or grants elsewhere. A tenant mount at or
// under either path would replace part of what fiberd keeps there.
func checkVolumes(fld *field.Path, spec *corev1.PodSpec) field.ErrorList {
	var errs field.ErrorList
	for i, v := range spec.Volumes {
		if isOwnedVolume(v.Name) {
			errs = append(errs, field.Duplicate(fld.Child("volumes").Index(i).Child("name"), v.Name))
		}
	}
	cPath := fld.Child("containers").Index(0)
	c := &spec.Containers[0]
	for i, m := range c.VolumeMounts {
		p := cPath.Child("volumeMounts").Index(i)
		if isOwnedVolume(m.Name) {
			errs = append(errs, field.Forbidden(p.Child("name"), m.Name+" is mounted by the controller"))
		}
		if dir, ok := ownedDir(m.MountPath); ok {
			errs = append(errs, field.Forbidden(p.Child("mountPath"), "must not be at or under "+dir))
		}
	}
	for i, d := range c.VolumeDevices {
		p := cPath.Child("volumeDevices").Index(i)
		if isOwnedVolume(d.Name) {
			errs = append(errs, field.Forbidden(p.Child("name"), d.Name+" is mounted by the controller"))
		}
		if dir, ok := ownedDir(d.DevicePath); ok {
			errs = append(errs, field.Forbidden(p.Child("devicePath"), "must not be at or under "+dir))
		}
	}
	return errs
}

// isOwnedVolume reports whether name is one of the overlay's volumes.
func isOwnedVolume(name string) bool { return name == stateVolume || name == grantsVolume }

// ownedDir returns the overlay directory that p is at or under.
func ownedDir(p string) (string, bool) {
	p = path.Clean(p)
	for _, dir := range []string{stateDir, grantsDir} {
		if p == dir || strings.HasPrefix(p, dir+"/") {
			return dir, true
		}
	}
	return "", false
}

// addVolumes adds fiberd's state directory and the in-memory directory the
// home reads grants from. Grants are bearer tokens until mutual TLS binds
// them, so they never touch the node's disk.
func addVolumes(spec *corev1.PodSpec) {
	spec.Volumes = append(spec.Volumes,
		corev1.Volume{Name: stateVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		corev1.Volume{Name: grantsVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			Medium:    corev1.StorageMediumMemory,
			SizeLimit: ptr.To(resource.MustParse(grantsSizeLimit)),
		}}},
	)
}
