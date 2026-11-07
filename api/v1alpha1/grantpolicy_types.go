package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GrantPolicySpec is what the cluster lets a CapacityGrant ask for.
//
// +kubebuilder:validation:XValidation:rule="!has(self.defaultRuntimes) || self.defaultRuntimes.all(r, r == 'gvisor' || r == 'hyperlight')",message="defaultRuntimes holds only gvisor and hyperlight, proc and runc need a namespace label",fieldPath=".defaultRuntimes",reason="FieldValueForbidden"
// +kubebuilder:validation:XValidation:rule="!has(self.defaultRuntimes) || (has(self.runtimes) && self.defaultRuntimes.all(r, r in self.runtimes))",message="defaultRuntimes must sit inside runtimes",fieldPath=".defaultRuntimes",reason="FieldValueInvalid"
type GrantPolicySpec struct {
	// images lists the repositories the fiberd container image may come from, each fully
	// qualified such as ghcr.io/azure/fiberd. An entry matches an image that equals it or
	// continues with /, : or @, so ghcr.io/azure/fiberd matches
	// ghcr.io/azure/fiberd:0.4.0@sha256:... and not ghcr.io/azure/fiberd-runc. Every image
	// must also carry a digest. A helperImage is not checked against this list.
	// +required
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=255
	// +kubebuilder:validation:items:Pattern=`^(localhost(:[0-9]{1,5})?|[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+(:[0-9]{1,5})?|[a-z0-9]([a-z0-9-]*[a-z0-9])?:[0-9]{1,5})(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)+$`
	Images []string `json:"images"`

	// helperImages lists the repositories the helperImage of a hyperlight grant may come
	// from, matched as images entries are, and a helperImage must also carry a digest. It is
	// a list of its own because the helper runs under fiberd with far less than the fiberd
	// container holds, so a repository trusted for helpers does not qualify as a fiberd
	// image. While it is unset no hyperlight grant is admitted.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=255
	// +kubebuilder:validation:items:Pattern=`^(localhost(:[0-9]{1,5})?|[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+(:[0-9]{1,5})?|[a-z0-9]([a-z0-9-]*[a-z0-9])?:[0-9]{1,5})(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)+$`
	HelperImages []string `json:"helperImages,omitempty"`

	// registries lists the repositories a registry zygote may come from, spelled as
	// spec.zygote.registry spells them, such as ghcr.io/team-a or
	// http://registry.local:5000/zygotes. An entry matches a registry that equals it or
	// continues with /, so ghcr.io/team-a matches ghcr.io/team-a/zygotes and not
	// ghcr.io/team-a-tools. The scheme is compared too, so plain HTTP passes only under an
	// entry that spells http://. A registry zygote is code this policy never sees, so while
	// the list is unset no registry grant is admitted.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=255
	// +kubebuilder:validation:items:Pattern=`^(http://)?(localhost(:[0-9]{1,5})?|[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+(:[0-9]{1,5})?|[a-z0-9]([a-z0-9-]*[a-z0-9])?:[0-9]{1,5})(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)+$`
	Registries []string `json:"registries,omitempty"`

	// runtimes is the ceiling, the runtimes any grant in the cluster may ask for. A namespace
	// can never pass it.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=4
	// +kubebuilder:default={"hyperlight"}
	Runtimes []Runtime `json:"runtimes,omitempty"`

	// defaultRuntimes is what a namespace with no exception label may use. It holds only
	// gvisor and hyperlight, because proc and runc share the host kernel and need the
	// namespace's exception, and it must sit inside runtimes. A namespace labelled
	// runtimes.fiberd.io/<runtime>: "true" may also use that runtime, if runtimes holds it.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=2
	DefaultRuntimes []Runtime `json:"defaultRuntimes,omitempty"`
}

// GrantPolicy is the cluster's policy for CapacityGrants. The fiberd-grants
// admission policy reads the one named default and refuses a grant that
// breaks it.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default'",message="the fiberd-grants binding reads only the GrantPolicy named default",reason="FieldValueInvalid"
type GrantPolicy struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the desired policy.
	// +required
	Spec GrantPolicySpec `json:"spec"`
}

// GrantPolicyList is a list of GrantPolicy.
//
// +kubebuilder:object:root=true
type GrantPolicyList struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard list metadata.
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`

	// items are the policies in the list.
	Items []GrantPolicy `json:"items"`
}
