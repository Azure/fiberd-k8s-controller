package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupName is the API group of the types in this package.
const GroupName = "fiberd.io"

var (
	// GroupVersion is the group and version of the API types in this package.
	GroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1alpha1"}

	// SchemeBuilder collects the types of this package for a runtime scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme registers the types of this package in a runtime scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// addKnownTypes registers the root types and the group's meta types.
func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &CapacityGrant{}, &CapacityGrantList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
