package v1alpha1

const (
	// LabelGrant labels every object the controller creates for a grant with the grant name.
	LabelGrant = "fiberd.io/grant"

	// LabelGrantUID labels every object the controller creates for a grant with the grant UID.
	LabelGrantUID = "fiberd.io/grant-uid"

	// LabelManagedBy names the tool that manages an object, as the Kubernetes recommended
	// labels define it.
	LabelManagedBy = "app.kubernetes.io/managed-by"

	// ManagedBy is the value every object the controller creates carries under LabelManagedBy.
	ManagedBy = "fiberd-k8s-controller"

	// DigestPattern is the form of a zygote digest. The CRD marker on spec.zygote.digest
	// holds the same pattern. It must stay equal to fiberd's core.TemplateDigestPattern, and a
	// test checks it.
	DigestPattern = "^sha256:[a-f0-9]{64}$"

	// ConditionProgressing reports whether the grant Pods are being rolled out.
	ConditionProgressing = "Progressing"

	// ConditionReady reports whether the grant has a Pod that serves fibers.
	ConditionReady = "Ready"

	// PodConditionZygoteReady is the Pod condition fiberd sets once its zygote is warm. It must
	// stay equal to the constant of the same name in fiberd's Kubernetes home, and a test checks it.
	PodConditionZygoteReady = "fiberd.io/zygote-ready"

	// ReasonTemplateWarm is the PodConditionZygoteReady reason while the condition is True.
	// fiberd holds a warm zygote template for at least one grant. It must stay equal to the
	// constant of the same name in fiberd's Kubernetes home, and a test checks it.
	ReasonTemplateWarm = "TemplateWarm"

	// ReasonNoWarmTemplate is the PodConditionZygoteReady reason while the condition is False.
	// fiberd holds no warm zygote template yet. It must stay equal to the constant of the same
	// name in fiberd's Kubernetes home, and a test checks it.
	ReasonNoWarmTemplate = "NoWarmTemplate"

	// ReasonPending means the grant's Deployment does not exist yet, or has
	// not yet observed the grant's latest spec.
	ReasonPending = "Pending"

	// ReasonUnavailable means the grant's Deployment exists but has no
	// available Pod.
	ReasonUnavailable = "Unavailable"

	// ReasonAvailable means the grant's Deployment has at least one
	// available Pod.
	ReasonAvailable = "Available"

	// ReasonInvalidSpec means the controller cannot build the grant Pod from
	// the spec. Nothing is applied until the spec changes, so a Deployment
	// built from an earlier spec keeps serving.
	ReasonInvalidSpec = "InvalidSpec"

	// ReasonForbidden means the grant's namespace does not let the
	// controller apply the grant's Deployment or Service. A RoleBinding
	// there grants it.
	ReasonForbidden = "Forbidden"

	// ReasonDeploymentConflict means a Deployment with the grant's object
	// name exists and the grant does not control it. The controller leaves
	// it alone.
	ReasonDeploymentConflict = "DeploymentConflict"

	// ReasonServiceConflict means a Service with the grant's object name
	// exists and the grant does not control it. The controller leaves it
	// alone.
	ReasonServiceConflict = "ServiceConflict"

	// Finalizer keeps a grant until the controller has cleaned up what it created.
	Finalizer = "fiberd.io/finalizer"
)
