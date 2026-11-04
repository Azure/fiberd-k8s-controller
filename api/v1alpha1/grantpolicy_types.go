package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GrantPolicySpec is what the cluster lets a CapacityGrant ask for.
type GrantPolicySpec struct {
	// images lists the repositories a grant Pod image may come from, each fully qualified
	// such as ghcr.io/azure/fiberd. An entry matches an image that equals it or continues
	// with /, : or @, so ghcr.io/azure/fiberd matches ghcr.io/azure/fiberd:0.4.0@sha256:...
	// and not ghcr.io/azure/fiberd-runc. Every image must also carry a digest.
	// +required
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=255
	// +kubebuilder:validation:items:Pattern=`^(localhost(:[0-9]{1,5})?|[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+(:[0-9]{1,5})?|[a-z0-9]([a-z0-9-]*[a-z0-9])?:[0-9]{1,5})(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)+$`
	Images []string `json:"images"`

	// privilegedNamespaces are the namespaces whose grants may set securityContext.privileged
	// on the fiberd container, or ask for the gvisor or hyperlight runtime, which run
	// privileged until their capability sets are measured.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:MaxLength=63
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	PrivilegedNamespaces []string `json:"privilegedNamespaces,omitempty"`

	// runtimes are the fiberd runtimes a grant may ask for. gvisor and hyperlight also need the
	// grant's namespace in privilegedNamespaces.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=4
	// +kubebuilder:default={"proc","runc"}
	Runtimes []Runtime `json:"runtimes,omitempty"`
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
