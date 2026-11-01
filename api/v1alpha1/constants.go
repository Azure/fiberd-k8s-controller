package v1alpha1

const (
	// LabelGrant labels every object the controller creates for a grant with the grant name.
	LabelGrant = "fiberd.io/grant"

	// LabelGrantUID labels every object the controller creates for a grant with the grant UID.
	LabelGrantUID = "fiberd.io/grant-uid"

	// ConditionProgressing reports whether the grant Pods are being rolled out.
	ConditionProgressing = "Progressing"

	// ConditionReady reports whether the grant has a Pod that serves fibers.
	ConditionReady = "Ready"

	// PodConditionZygoteReady is the Pod condition fiberd sets once its zygote is warm.
	PodConditionZygoteReady = "fiberd.io/zygote-ready"

	// Finalizer keeps a grant until the controller has cleaned up what it created.
	Finalizer = "fiberd.io/finalizer"
)
