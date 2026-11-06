package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Runtime is the fiberd backend that runs the fibers of a grant.
// The values are the names fiberd's -runtime flag takes.
//
// +enum
// +kubebuilder:validation:Enum=proc;runc;gvisor;hyperlight
type Runtime string

const (
	// RuntimeProc runs each fiber as a plain cloned process.
	RuntimeProc Runtime = "proc"
	// RuntimeRunc runs each fiber in a runc container.
	RuntimeRunc Runtime = "runc"
	// RuntimeGvisor runs each fiber in a gVisor sandbox.
	RuntimeGvisor Runtime = "gvisor"
	// RuntimeHyperlight runs each fiber in a Hyperlight micro VM.
	RuntimeHyperlight Runtime = "hyperlight"
)

// CapacityGrantSpec describes the capacity a tenant asks for and the Pod that holds it.
//
// +kubebuilder:validation:XValidation:rule="!has(self.helperImage) || (has(self.runtime) && self.runtime == 'hyperlight')",message="helperImage is allowed only for runtime hyperlight",fieldPath=".helperImage",reason="FieldValueForbidden"
// +kubebuilder:validation:XValidation:rule="!has(self.runtime) || self.runtime != 'hyperlight' || has(self.helperImage)",message="runtime hyperlight needs helperImage, fiberd does not start without its helper",fieldPath=".helperImage",reason="FieldValueRequired"
type CapacityGrantSpec struct {
	// zygote is the program every fiber of the grant is cloned from.
	// +required
	Zygote ZygoteSpec `json:"zygote"`

	// fibers bounds how many fibers the grant may hold.
	// +required
	Fibers FiberBudget `json:"fibers"`

	// writeBudget is the most memory one fiber may dirty, such as 512Mi.
	// Unset means no per-fiber limit.
	// +optional
	WriteBudget *resource.Quantity `json:"writeBudget,omitempty"`

	// callerThumbprint is the x5t#S256 thumbprint of the one client certificate allowed to
	// present the grant. A home serving mutual TLS refuses a grant without it.
	// +optional
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_-]{43}$`
	CallerThumbprint string `json:"callerThumbprint,omitempty"`

	// runtime is the fiberd backend that runs the fibers.
	// +optional
	// +kubebuilder:default="proc"
	Runtime Runtime `json:"runtime,omitempty"`

	// helperImage is the image the Hyperlight helper comes from. Runtime hyperlight needs it,
	// and no other runtime accepts it. The image holds an executable /fiberd-helper/helper,
	// and optionally /fiberd-helper/guest. An init container copies /fiberd-helper into the
	// Pod, and fiberd runs the helper from /var/run/fiberd/helper/helper in the fiberd
	// container, not on the image's base. The init container runs the image itself, so cp
	// must be on the image's PATH, and everything under /fiberd-helper must be readable by
	// other users, since the copy runs as uid 65532.
	// The helper must be static, or linked against glibc 2.36 or older with only libc, libm
	// and libgcc_s. It must answer helper --version and speak the protocol in fiberd's
	// hack/hyperlight/PROTOCOL.md. A guest is passed as -hyperlight-guest in the fiberd
	// container's args, such as -hyperlight-guest=/var/run/fiberd/helper/guest. It is at
	// most 456 characters, the longest reference a registry accepts: a 255-character
	// repository, a 128-character tag and a sha256 digest.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=456
	HelperImage string `json:"helperImage,omitempty"`

	// podTemplate is the template of the grant Pod, written as in a Deployment.
	// It holds exactly one container named fiberd.
	// The validation rules list the fields a tenant may not set. The fiberd container must
	// also set a memory limit.
	// +required
	// +kubebuilder:validation:XValidation:rule="has(self.spec)",message="podTemplate.spec is required",fieldPath=".spec",reason="FieldValueRequired"
	// +kubebuilder:validation:XValidation:rule="!has(self.spec) || (self.spec.containers.size() == 1 && self.spec.containers[0].name == 'fiberd')",message="podTemplate.spec.containers must hold exactly one container named fiberd",fieldPath=".spec.containers",reason="FieldValueInvalid"
	// +kubebuilder:validation:XValidation:rule="!has(self.spec) || self.spec.containers.size() == 0 || !has(self.spec.containers[0].command)",message="the fiberd container's command is the image entrypoint, which runs fiberd",fieldPath=".spec.containers",reason="FieldValueForbidden"
	// +kubebuilder:validation:XValidation:rule="!has(self.spec) || self.spec.containers.size() == 0 || !has(self.spec.containers[0].workingDir)",message="podTemplate.spec.containers[0].workingDir is not allowed, fiberd runs in the image's working directory",fieldPath=".spec.containers",reason="FieldValueForbidden"
	// +kubebuilder:validation:XValidation:rule="!has(self.spec) || (!has(self.spec.initContainers) && !has(self.spec.ephemeralContainers))",message="podTemplate.spec.initContainers and ephemeralContainers are not allowed",fieldPath=".spec.initContainers",reason="FieldValueForbidden"
	// +kubebuilder:validation:XValidation:rule="!has(self.spec) || (!has(self.spec.serviceAccountName) && !has(self.spec.serviceAccount))",message="podTemplate.spec.serviceAccountName is set by the controller",fieldPath=".spec.serviceAccountName",reason="FieldValueForbidden"
	// +kubebuilder:validation:XValidation:rule="!has(self.spec) || !has(self.spec.nodeName)",message="podTemplate.spec.nodeName is not allowed, the scheduler places the Pod",fieldPath=".spec.nodeName",reason="FieldValueForbidden"
	// +kubebuilder:validation:XValidation:rule="!has(self.spec) || !has(self.spec.restartPolicy) || self.spec.restartPolicy == 'Always'",message="podTemplate.spec.restartPolicy must be Always",fieldPath=".spec.restartPolicy",reason="FieldValueInvalid"
	// +kubebuilder:validation:XValidation:rule="!has(self.spec) || ((!has(self.spec.hostPID) || !self.spec.hostPID) && (!has(self.spec.hostNetwork) || !self.spec.hostNetwork) && (!has(self.spec.hostIPC) || !self.spec.hostIPC) && (!has(self.spec.shareProcessNamespace) || !self.spec.shareProcessNamespace) && !has(self.spec.hostUsers))",message="podTemplate.spec hostPID, hostNetwork, hostIPC, shareProcessNamespace and hostUsers are set by the controller",fieldPath=".spec",reason="FieldValueForbidden"
	// +kubebuilder:validation:XValidation:rule="!has(self.spec) || self.spec.containers.size() == 0 || (!has(self.spec.containers[0].startupProbe) && !has(self.spec.containers[0].readinessProbe) && !has(self.spec.containers[0].livenessProbe))",message="podTemplate.spec.containers[0] startupProbe, readinessProbe and livenessProbe are set by the controller",fieldPath=".spec.containers",reason="FieldValueForbidden"
	// +kubebuilder:validation:XValidation:rule="!has(self.spec) || !has(self.spec.resources)",message="podTemplate.spec.resources is not allowed, the controller sizes the Pod through the fiberd container",fieldPath=".spec.resources",reason="FieldValueForbidden"
	// +kubebuilder:validation:XValidation:rule="!has(self.spec) || self.spec.containers.size() == 0 || (has(self.spec.containers[0].resources) && has(self.spec.containers[0].resources.limits) && 'memory' in self.spec.containers[0].resources.limits)",message="the fiberd container must set resources.limits.memory, fiberd carves fibers from it",fieldPath=".spec.containers",reason="FieldValueRequired"
	// +kubebuilder:validation:XValidation:rule="!has(self.spec) || self.spec.containers.size() == 0 || !has(self.spec.containers[0].lifecycle)",message="podTemplate.spec.containers[0].lifecycle is not allowed, a hook would run a tenant command with fiberd's capabilities",fieldPath=".spec.containers",reason="FieldValueForbidden"
	// +kubebuilder:validation:XValidation:rule="!has(self.spec) || self.spec.containers.size() == 0 || !has(self.spec.containers[0].envFrom)",message="podTemplate.spec.containers[0].envFrom is not allowed, a source's key names are unknown at admission",fieldPath=".spec.containers",reason="FieldValueForbidden"
	// +kubebuilder:validation:XValidation:rule="!has(self.spec) || (!has(self.spec.hostAliases) && !has(self.spec.dnsConfig) && !has(self.spec.runtimeClassName))",message="podTemplate.spec.hostAliases, dnsConfig and runtimeClassName are not allowed, fiberd resolves its issuer through the cluster DNS and runs its own runtimes",fieldPath=".spec",reason="FieldValueForbidden"
	// +kubebuilder:validation:XValidation:rule="!has(self.spec) || ((!has(self.spec.securityContext) || (!has(self.spec.securityContext.seLinuxOptions) && !has(self.spec.securityContext.windowsOptions))) && (self.spec.containers.size() == 0 || !has(self.spec.containers[0].securityContext) || (!has(self.spec.containers[0].securityContext.seLinuxOptions) && !has(self.spec.containers[0].securityContext.windowsOptions))))",message="podTemplate seLinuxOptions and windowsOptions are not allowed, they would unconfine a container that already holds SYS_ADMIN",fieldPath=".spec",reason="FieldValueForbidden"
	PodTemplate corev1.PodTemplateSpec `json:"podTemplate"`
}

// ZygoteSpec names the zygote of a grant and how fiberd starts it. Exactly one of command
// and registry says where the zygote comes from.
//
// +kubebuilder:validation:XValidation:rule="has(self.command) != has(self.registry)",message="exactly one of zygote.command and zygote.registry must be set"
type ZygoteSpec struct {
	// digest names what every fiber is cloned from. fiberd refuses a grant whose digest it
	// does not serve, and parked sessions resume only on a home with the same digest.
	// +required
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	Digest string `json:"digest"`

	// command is the zygote program inside the fiberd container, path first then its
	// arguments. The controller passes digest and command to fiberd as its template mapping.
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +listType=atomic
	Command []string `json:"command,omitempty"`

	// registry is the OCI repository fiberd pulls the zygote artifact from by digest, such
	// as ghcr.io/team-a/zygotes, or http://registry.local:5000/zygotes for plain HTTP. It
	// is the value of fiberd's -registry flag.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^(https?://)?(localhost(:[0-9]{1,5})?|[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+(:[0-9]{1,5})?|[a-z0-9]([a-z0-9-]*[a-z0-9])?:[0-9]{1,5})(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)+$`
	Registry string `json:"registry,omitempty"`

	// sealKeySecretName names a Secret in the grant's namespace that holds the fleet's seal
	// key under the key seal-key.json. fiberd seals the grant's deltas with it, and opens
	// sealed author images of registry artifacts with it.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	SealKeySecretName string `json:"sealKeySecretName,omitempty"`
}

// FiberBudget bounds the fibers of a grant.
//
// +kubebuilder:validation:XValidation:rule="!has(self.warm) || self.warm <= self.max",message="fibers.warm must not exceed fibers.max"
type FiberBudget struct {
	// max is the most fibers the grant may hold at once, warm ones included.
	// +required
	// +kubebuilder:validation:Minimum=1
	Max int32 `json:"max"`

	// warm is how many sandboxes a gVisor home keeps restored ahead of clones.
	// They count inside max, and other runtimes ignore it.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Warm int32 `json:"warm,omitempty"`
}

// CapacityGrantStatus is what the controller observed about a grant.
type CapacityGrantStatus struct {
	// observedGeneration is the generation of the spec the status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions describe the state of the grant. The types are Ready and Progressing.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// resolvedImage is the image the grant Pods run after defaulting.
	// +optional
	ResolvedImage string `json:"resolvedImage,omitempty"`
}

// CapacityGrant is a tenant's request for fiber capacity.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cg
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 56",message="the name is at most 56 characters so that fiberd-<name> fits a Service name"
type CapacityGrant struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the desired state of the grant.
	// +required
	Spec CapacityGrantSpec `json:"spec"`

	// status is the observed state of the grant.
	// +optional
	Status CapacityGrantStatus `json:"status,omitempty"`
}

// CapacityGrantList is a list of CapacityGrant.
//
// +kubebuilder:object:root=true
type CapacityGrantList struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard list metadata.
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`

	// items are the grants in the list.
	Items []CapacityGrant `json:"items"`
}
