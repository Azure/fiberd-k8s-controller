// Package podtemplate turns a CapacityGrant's pod template into the one the
// controller applies as its Deployment's template.
//
// The tenant writes spec.podTemplate as in a Deployment, with one container
// named fiberd. Build lays a fixed overlay on top of a copy of it and
// leaves every other field as the tenant wrote it. The overlay sets only
// what fiberd needs to run as a grant home. That is the labels the
// controller selects on, the service account, the readiness gate, fiberd's
// volumes, service links, the image, the Pod identity, the trusted CA file, the
// gRPC port, the fiberd flags, the probes, the resources, the
// security context of the runtime and, for hyperlight, the init container
// that installs the helper. Where a tenant field would collide
// with the overlay, such as a volume of the same name or a probe, Build
// refuses the grant instead of picking a winner silently. It refuses
// Pod-level resources too, since they would size the Pod and set its QoS
// class in place of the fiberd container.
//
// The Pod runs fiberd's own image, whose entrypoint runs fiberd under tini.
// The first controller flag, -home=kubernetes, selects the Kubernetes home
// inside fiberd. Nothing is added to the image, and the builder sets no
// command on the fiberd container.
//
// proc and runc Pods run with Unconfined seccomp and AppArmor profiles.
// fiberd has no seccomp profile of its own yet.
package podtemplate

import (
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
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
	tlsVolume       = "fiberd-tls"
	tlsDir          = "/var/run/fiberd/tls"
	caFile          = tlsDir + "/ca.crt"
	certFile        = tlsDir + "/tls.crt"
	keyFile         = tlsDir + "/tls.key"

	grantTokenVolume = "fiberd-grant-token"
	grantTokenDir    = "/var/run/secrets/fiberd.io/grant"
	grantTokenPath   = "token"
	grantTokenFile   = grantTokenDir + "/" + grantTokenPath

	// grantTokenSeconds is the shortest life the API server allows. The
	// kubelet refreshes the token well before then, and fiberd rereads the
	// file for every request, so a short life costs nothing.
	grantTokenSeconds int64 = 600

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
	envSSLCertFile  = "SSL_CERT_FILE"
)

// The cert-manager csi-driver contract of the tls volume. The driver issues
// the certificate when the kubelet mounts the volume and writes tls.crt,
// tls.key and ca.crt into it. No fs-group is set while fiberd runs as root.
const (
	// CAIssuerName is the ClusterIssuer every grant Pod's certificate comes
	// from. The controller's own serving certificate comes from it too.
	CAIssuerName = "fiberd-ca"

	csiDriverName = "csi.cert-manager.io"

	attrIssuerName      = "csi.cert-manager.io/issuer-name"
	attrIssuerKind      = "csi.cert-manager.io/issuer-kind"
	attrCommonName      = "csi.cert-manager.io/common-name"
	attrDNSNames        = "csi.cert-manager.io/dns-names"
	attrKeyUsages       = "csi.cert-manager.io/key-usages"
	attrDuration        = "csi.cert-manager.io/duration"
	attrRenewBefore     = "csi.cert-manager.io/renew-before"
	attrReusePrivateKey = "csi.cert-manager.io/reuse-private-key"

	// caIssuerKind is a ClusterIssuer because grant Pods live in tenant
	// namespaces, and a namespaced Issuer serves only its own.
	caIssuerKind = "ClusterIssuer"

	// certCommonName is expanded by the driver, not the kubelet, so each
	// certificate names its own Pod.
	certCommonName = "${POD_NAME}"

	// certKeyUsages is server auth alone, since fiberd never presents this
	// certificate as a client.
	certKeyUsages = "server auth"

	// certDuration and certRenewBefore keep a leaked key useful for a day
	// at most, and leave a third of it to renew in.
	certDuration    = "24h"
	certRenewBefore = "8h"

	// certReusePrivateKey is the driver's default, pinned so a renewal always
	// makes a new key.
	certReusePrivateKey = "false"
)

// EnvNamePattern is the only shape a tenant env name may take. The root is a
// prefix fiberd core owns, which keeps tenant names apart from every name a
// program in the image reads, today's and later ones alike. Only the
// Hyperlight helper reads them, and they never reach the zygote or fibers.
// Values stay free, so valueFrom needs no rule. The admission policy in
// config/policy repeats the pattern, and a test keeps the two equal.
const EnvNamePattern = `^FIBERD_TENANT_[A-Z0-9_]+$`

var envNamePattern = regexp.MustCompile(EnvNamePattern)

// ownedVolumes are the overlay's volumes. A test keeps them equal to the
// policy's ownedVolumes.
var ownedVolumes = []string{stateVolume, grantsVolume, tlsVolume, grantTokenVolume, helperVolume, sealKeyVolume}

// mountRoots are the only directories a tenant may mount in, for config
// files, data volumes and Secrets. fiberd's images ship nothing there and the
// overlay mounts nothing there, so a tenant mount replaces nothing fiberd runs
// or trusts. They are allowed rather than the system paths refused, since a
// list of refused paths stays open for any path it misses. A test keeps them
// equal to the policy's mountRoots.
var mountRoots = []string{"/config", "/data", "/secrets"}

// reservedKeyPrefixes start the template label and annotation keys a tenant
// may not set. fiberd.io keys are the controller's. The AppArmor and seccomp
// annotations would conflict with the fields the controller sets. The rest
// are honoured by node components and reach host networks, host devices or
// the sandbox. A test keeps the list equal to the policy's.
var reservedKeyPrefixes = []string{
	"fiberd.io/",
	"container.apparmor.security.beta.kubernetes.io/",
	"seccomp.security.alpha.kubernetes.io/",
	"container.seccomp.security.alpha.kubernetes.io/",
	"k8s.v1.cni.cncf.io/",
	"cni.projectcalico.org/",
	"io.kubernetes.cri-o.",
	"dev.gvisor.",
	"io.katacontainers.",
}

// Build returns the pod template the controller applies for cg, with image
// already resolved by the caller. serviceName is the grant's Service, which
// the Pod's certificate names. Build never changes cg. When the grant
// cannot run, the error is a field error list that names every field at
// fault, ready for the grant's status.
func Build(cg *v1alpha1.CapacityGrant, image, serviceName string, opts Options) (corev1.PodTemplateSpec, error) {
	if cg == nil {
		return corev1.PodTemplateSpec{}, errors.New("podtemplate: nil CapacityGrant")
	}
	if image == "" {
		return corev1.PodTemplateSpec{}, fmt.Errorf("podtemplate: grant %s/%s has no resolved image", cg.Namespace, cg.Name)
	}
	if serviceName == "" {
		return corev1.PodTemplateSpec{}, fmt.Errorf("podtemplate: grant %s/%s has no Service name", cg.Namespace, cg.Name)
	}
	opts = opts.withDefaults()
	if err := opts.Validate(); err != nil {
		return corev1.PodTemplateSpec{}, err
	}

	specPath := field.NewPath("spec")
	podPath := specPath.Child("podTemplate", "spec")
	tmpl := cg.Spec.PodTemplate.DeepCopy()

	rt, errs := checkRuntime(specPath.Child("runtime"), cg.Spec.Runtime)
	if len(errs) == 0 {
		errs = append(errs, checkHelperImage(specPath.Child("helperImage"), rt, cg.Spec.HelperImage)...)
		errs = append(errs, checkSealKey(specPath.Child("zygote", "sealKeySecretName"), rt, cg.Spec.Zygote)...)
	}
	zArgs, zErrs := zygoteArgs(specPath.Child("zygote"), cg.Spec.Zygote)
	errs = append(errs, zErrs...)
	errs = append(errs, checkShape(podPath, &tmpl.Spec)...)
	metaPath := specPath.Child("podTemplate", "metadata")
	errs = append(errs, checkReservedKeys(metaPath.Child("labels"), tmpl.Labels)...)
	errs = append(errs, checkReservedKeys(metaPath.Child("annotations"), tmpl.Annotations)...)
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
	resources, rErrs := sizeResources(cPath.Child("resources"), c.Resources, opts.DefaultCPU)
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
	addVolumes(&tmpl.Spec, cg, serviceName, opts.IssuerURL)

	c.Image = image
	c.Env = append(c.Env, podEnv()...)
	c.Env = append(c.Env, caEnv())
	c.Args = append(c.Args, controllerArgs(rt, zArgs, opts)...)
	c.Ports = append(c.Ports, corev1.ContainerPort{Name: PortNameGRPC, ContainerPort: PortGRPC, Protocol: corev1.ProtocolTCP})
	c.VolumeMounts = append(c.VolumeMounts,
		corev1.VolumeMount{Name: stateVolume, MountPath: stateDir},
		corev1.VolumeMount{Name: grantsVolume, MountPath: grantsDir},
		corev1.VolumeMount{Name: tlsVolume, MountPath: tlsDir, ReadOnly: true},
		corev1.VolumeMount{Name: grantTokenVolume, MountPath: grantTokenDir, ReadOnly: true},
	)
	if cg.Spec.Zygote.SealKeySecretName != "" {
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: sealKeyVolume, MountPath: sealKeyDir, ReadOnly: true})
	}
	setProbes(c)
	c.Resources = resources
	addHelper(&tmpl.Spec, c, rt, cg.Spec.HelperImage)
	if c.SecurityContext, err = securityContext(c.SecurityContext, rt, c.Args); err != nil {
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
// cannot reach the Pod.
func checkShape(fld *field.Path, spec *corev1.PodSpec) field.ErrorList {
	var errs field.ErrorList
	if len(spec.Containers) != 1 || spec.Containers[0].Name != ContainerName {
		errs = append(errs, field.Invalid(fld.Child("containers"), containerNames(spec.Containers), "must hold exactly one container named "+ContainerName))
	} else {
		// The CRD checks command and lifecycle apart, so a grant breaking
		// both hears about both here too.
		c := &spec.Containers[0]
		cPath := fld.Child("containers").Index(0)
		if len(c.Command) > 0 {
			errs = append(errs, field.Forbidden(cPath.Child("command"), "the image entrypoint runs fiberd"))
		}
		if c.WorkingDir != "" {
			errs = append(errs, field.Forbidden(cPath.Child("workingDir"), "fiberd runs in the image's working directory"))
		}
		if c.Lifecycle != nil {
			errs = append(errs, field.Forbidden(cPath.Child("lifecycle"), "a hook would run a tenant command with fiberd's capabilities"))
		}
		if len(c.EnvFrom) > 0 {
			errs = append(errs, field.Forbidden(cPath.Child("envFrom"), "a source's key names are unknown at admission"))
		}
		if sc := c.SecurityContext; sc != nil {
			errs = append(errs, checkUnconfined(cPath.Child("securityContext"), sc.SELinuxOptions != nil, sc.WindowsOptions != nil)...)
		}
	}
	if sc := spec.SecurityContext; sc != nil {
		errs = append(errs, checkUnconfined(fld.Child("securityContext"), sc.SELinuxOptions != nil, sc.WindowsOptions != nil)...)
	}
	if len(spec.HostAliases) > 0 {
		errs = append(errs, field.Forbidden(fld.Child("hostAliases"), "fiberd resolves its issuer through the cluster DNS"))
	}
	if spec.DNSConfig != nil {
		errs = append(errs, field.Forbidden(fld.Child("dnsConfig"), "fiberd resolves its issuer through the cluster DNS"))
	}
	if spec.RuntimeClassName != nil {
		errs = append(errs, field.Forbidden(fld.Child("runtimeClassName"), "fiberd runs its own runtimes"))
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
		errs = append(errs, field.Forbidden(fld.Child("resources"), "the controller sizes the Pod through the fiberd container, and Pod-level resources would set its QoS class in its place"))
	}
	return errs
}

// checkUnconfined refuses the SELinux and Windows options of the security
// context at fld. The Pod and the container carry the same pair, and either
// could lift fiberd out of the confinement the controller sets.
func checkUnconfined(fld *field.Path, seLinux, windows bool) field.ErrorList {
	const why = "would unconfine a container that holds SYS_ADMIN"
	var errs field.ErrorList
	if seLinux {
		errs = append(errs, field.Forbidden(fld.Child("seLinuxOptions"), why))
	}
	if windows {
		errs = append(errs, field.Forbidden(fld.Child("windowsOptions"), why))
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

// checkEnv refuses a tenant variable whose name does not match
// EnvNamePattern. The name is quoted because it is the tenant's text, and the
// error goes into the grant's status as one line.
func checkEnv(fld *field.Path, env []corev1.EnvVar) field.ErrorList {
	var errs field.ErrorList
	for i, e := range env {
		if !envNamePattern.MatchString(e.Name) {
			errs = append(errs, field.Forbidden(fld.Index(i).Child("name"), fmt.Sprintf("%q must match %s", e.Name, EnvNamePattern)))
		}
	}
	return errs
}

// checkReservedKeys refuses the tenant's template keys that a reserved
// prefix starts or that sit under a fiberd.io subdomain. The keys are
// sorted so the error reads the same on every reconcile.
func checkReservedKeys(fld *field.Path, keys map[string]string) field.ErrorList {
	var errs field.ErrorList
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		reserved := strings.Contains(k, ".fiberd.io/") ||
			slices.ContainsFunc(reservedKeyPrefixes, func(p string) bool { return strings.HasPrefix(k, p) })
		if reserved {
			errs = append(errs, field.Forbidden(fld.Key(k), "is reserved"))
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

// overlayMeta adds the grant labels and the hash annotation. Build has
// refused tenant fiberd.io keys, so only a tenant's managed-by label can
// collide here, and the controller's value wins. Every other tenant label
// and annotation stays.
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
// default. Service links stay off so that Service names pick no env names.
// The kubelet still adds the kubernetes Service's variables, which the
// in-cluster client reads.
func overlayPodSpec(spec *corev1.PodSpec, serviceAccount string) {
	spec.ServiceAccountName = serviceAccount
	spec.AutomountServiceAccountToken = ptr.To(true)
	spec.EnableServiceLinks = ptr.To(false)
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

// caEnv points fiberd's root pool at the tls volume's CA, so fiberd
// trusts the controller's HTTPS. SSL_CERT_FILE replaces Go's default bundle
// file, but Go still loads every file in /etc/ssl/certs, where the image's
// bundle lives, so the public roots stay for registry pulls. SSL_CERT_DIR
// would replace that directory and trust the Pod's own tls.crt as a root.
func caEnv() corev1.EnvVar {
	return corev1.EnvVar{Name: envSSLCertFile, Value: caFile}
}

// checkVolumes refuses tenant volumes, mounts and devices that collide
// with the overlay's volumes, or that sit outside mountRoots. A tenant
// mount of an overlay volume would expose fiberd's keys, grants, certificate
// or token elsewhere. A projected token is minted for the grant
// ServiceAccount, and fibers could read it at a tenant path.
func checkVolumes(fld *field.Path, spec *corev1.PodSpec) field.ErrorList {
	var errs field.ErrorList
	for i, v := range spec.Volumes {
		vPath := fld.Child("volumes").Index(i)
		if isOwnedVolume(v.Name) {
			errs = append(errs, field.Duplicate(vPath.Child("name"), v.Name))
		}
		if !allowedSource(&v.VolumeSource) {
			errs = append(errs, field.Forbidden(vPath, "must be an emptyDir, configMap, secret, downwardAPI, projected, persistentVolumeClaim or ephemeral volume"))
		}
		if v.Projected != nil && slices.ContainsFunc(v.Projected.Sources, projectsToken) {
			errs = append(errs, field.Forbidden(vPath.Child("projected"), "must not project a service account token, which fibers could read"))
		}
	}
	cPath := fld.Child("containers").Index(0)
	c := &spec.Containers[0]
	// The kubelet bind-mounts a node file at the termination message path.
	if c.TerminationMessagePath != "" {
		errs = append(errs, checkMountPath(cPath.Child("terminationMessagePath"), c.TerminationMessagePath)...)
	}
	for i, m := range c.VolumeMounts {
		p := cPath.Child("volumeMounts").Index(i)
		if isOwnedVolume(m.Name) {
			errs = append(errs, field.Forbidden(p.Child("name"), m.Name+" is mounted by the controller"))
		}
		errs = append(errs, checkMountPath(p.Child("mountPath"), m.MountPath)...)
	}
	for i, d := range c.VolumeDevices {
		p := cPath.Child("volumeDevices").Index(i)
		if isOwnedVolume(d.Name) {
			errs = append(errs, field.Forbidden(p.Child("name"), d.Name+" is mounted by the controller"))
		}
		errs = append(errs, checkMountPath(p.Child("devicePath"), d.DevicePath)...)
	}
	return errs
}

// checkMountPath refuses a path that is not clean or not at or under one of
// mountRoots. The policy cannot clean a path, so the path is refused rather
// than cleaned, and the two judge every spelling alike.
func checkMountPath(fld *field.Path, p string) field.ErrorList {
	if !strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return field.ErrorList{field.Invalid(fld, p, "must be a clean absolute path")}
	}
	if !slices.ContainsFunc(mountRoots, func(r string) bool { return p == r || strings.HasPrefix(p, r+"/") }) {
		return field.ErrorList{field.Forbidden(fld, "must be at or under "+strings.Join(mountRoots, ", "))}
	}
	return nil
}

// isOwnedVolume reports whether name is one of the overlay's volumes.
func isOwnedVolume(name string) bool { return slices.Contains(ownedVolumes, name) }

// allowedSource reports whether s is a source the policy allows. Every
// other source reaches the node or pulls with its credentials. The
// admission policy repeats the list, and a test keeps the two equal.
func allowedSource(s *corev1.VolumeSource) bool {
	return s.EmptyDir != nil || s.ConfigMap != nil || s.Secret != nil || s.DownwardAPI != nil ||
		s.Projected != nil || s.PersistentVolumeClaim != nil || s.Ephemeral != nil
}

// projectsToken reports whether p is a service account token projection.
func projectsToken(p corev1.VolumeProjection) bool { return p.ServiceAccountToken != nil }

// addVolumes adds fiberd's state directory, the in-memory directory the
// home reads grants from, the tls volume, the token the home fetches its
// grant with and, when the zygote names one, the seal key Secret. A grant is
// worthless without its caller's key, and it still never touches the node's
// disk.
func addVolumes(spec *corev1.PodSpec, cg *v1alpha1.CapacityGrant, serviceName, issuerURL string) {
	spec.Volumes = append(spec.Volumes,
		corev1.Volume{Name: stateVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		corev1.Volume{Name: grantsVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			Medium:    corev1.StorageMediumMemory,
			SizeLimit: ptr.To(resource.MustParse(grantsSizeLimit)),
		}}},
		tlsCSIVolume(cg, serviceName),
		grantToken(issuerURL),
	)
	if name := cg.Spec.Zygote.SealKeySecretName; name != "" {
		spec.Volumes = append(spec.Volumes, corev1.Volume{Name: sealKeyVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName:  name,
			Items:       []corev1.KeyToPath{{Key: v1alpha1.SealKeySecretKey, Path: v1alpha1.SealKeySecretKey}},
			DefaultMode: ptr.To[int32](0o400),
		}}})
	}
}

// grantToken projects a token bound to the Pod, whose only audience is the
// issuer, so it opens nothing else if it leaks. fiberd hides its directory
// from fibers.
func grantToken(issuerURL string) corev1.Volume {
	return corev1.Volume{Name: grantTokenVolume, VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
		Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
			Audience:          issuerURL,
			ExpirationSeconds: ptr.To(grantTokenSeconds),
			Path:              grantTokenPath,
		}}},
	}}}
}

// ServiceHost is the name of the grant's Service in namespace, which every
// grant Pod's certificate carries. A caller dials a Pod's address and
// verifies this name.
func ServiceHost(serviceName, namespace string) string {
	return serviceName + "." + namespace + ".svc"
}

// tlsCSIVolume asks the csi-driver for a certificate that names the grant's
// Service. fiberd serves mutual TLS with its tls.crt and tls.key, trusts its
// ca.crt for callers and reads the issuer's key set through it too.
func tlsCSIVolume(cg *v1alpha1.CapacityGrant, serviceName string) corev1.Volume {
	host := ServiceHost(serviceName, cg.Namespace)
	return corev1.Volume{Name: tlsVolume, VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{
		Driver:   csiDriverName,
		ReadOnly: ptr.To(true),
		VolumeAttributes: map[string]string{
			attrIssuerName:      CAIssuerName,
			attrIssuerKind:      caIssuerKind,
			attrCommonName:      certCommonName,
			attrDNSNames:        strings.Join([]string{host, host + ".cluster.local"}, ","),
			attrKeyUsages:       certKeyUsages,
			attrDuration:        certDuration,
			attrRenewBefore:     certRenewBefore,
			attrReusePrivateKey: certReusePrivateKey,
		},
	}}}
}
