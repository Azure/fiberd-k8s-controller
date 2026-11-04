// Package controller reconciles each CapacityGrant into the workload that
// holds its capacity, and reports the workload's progress in the grant's
// status.
package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/pkg/issuer"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload/podtemplate"
)

// blockedRequeue is how long a pass blocked by Forbidden or a conflict
// waits before it looks again. What clears those blocks is a RoleBinding,
// or an object of another owner going away, and the controller watches
// neither.
const blockedRequeue = time.Minute

// The actions of the Events the reconciler records, named for the step of
// the pass that found the block.
const (
	actionObserve = "Observe"
	actionApply   = "Apply"
)

// tenantClusterRole is the ClusterRole that a RoleBinding in each tenant
// namespace grants the controller, so it may apply the grant's objects
// there.
const tenantClusterRole = "fiberd-k8s-controller-tenant"

// +kubebuilder:rbac:groups=fiberd.io,resources=capacitygrants,verbs=get;list;watch
// +kubebuilder:rbac:groups=fiberd.io,resources=capacitygrants/status,verbs=patch
// +kubebuilder:rbac:groups=fiberd.io,resources=capacitygrants/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// CapacityGrantReconciler applies one workload per CapacityGrant and writes
// the grant's status from what it observes.
type CapacityGrantReconciler struct {
	// Client reads grants from the cache and patches their status.
	Client client.Client

	// Workload observes and applies the objects that run a grant.
	Workload workload.Workload

	// Template is what the controller decides for every grant Pod. Its
	// Lease is also the lease a spec is checked against.
	Template podtemplate.Options

	// Recorder records a Warning Event for each pass that Forbidden or a
	// conflict blocks.
	Recorder events.EventRecorder

	// Clock gives the time of each pass. SetupWithManager sets the real
	// clock when it is nil.
	Clock clock.PassiveClock
}

var _ reconcile.Reconciler = &CapacityGrantReconciler{}

// SetupWithManager registers the reconciler with mgr. It runs on grant spec
// and label changes, so Kueue's label edits reach it and its own status
// writes do not, and on changes to the Deployments and Services a grant
// controls.
func (r *CapacityGrantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	var missing []string
	if r.Client == nil {
		missing = append(missing, "Client")
	}
	if r.Workload == nil {
		missing = append(missing, "Workload")
	}
	if r.Recorder == nil {
		missing = append(missing, "Recorder")
	}
	if n := len(missing); n > 0 {
		list := missing[0]
		if n > 1 {
			list = strings.Join(missing[:n-1], ", ") + " and " + missing[n-1]
		}
		return fmt.Errorf("CapacityGrantReconciler is missing its %s", list)
	}
	if r.Clock == nil {
		r.Clock = clock.RealClock{}
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.CapacityGrant{}, builder.WithPredicates(predicate.Or[client.Object](
			predicate.GenerationChangedPredicate{},
			predicate.LabelChangedPredicate{},
		))).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Complete(r)
}

// Reconcile brings the workload of one grant to its spec and writes what
// it observed to the grant's status.
//
// A spec the controller cannot build, an object of the grant's name that
// the grant does not control, and a namespace that forbids the apply each
// block the pass. A blocked pass applies nothing, so a workload built from
// an earlier spec keeps serving. Each blocked pass is logged at V(2).
// Forbidden and the conflicts also record a Warning Event and look again
// after a minute.
//
// Any other error is returned and leaves the status as it was.
func (r *CapacityGrantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cg := &v1alpha1.CapacityGrant{}
	if err := r.Client.Get(ctx, req.NamespacedName, cg); err != nil {
		// Garbage collection removes the objects of a deleted grant.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !cg.DeletionTimestamp.IsZero() {
		// Garbage collection removes the objects of a grant being deleted.
		// A finalize step for what it cannot remove belongs here.
		return ctrl.Result{}, nil
	}
	now := r.Clock.Now()

	tmpl, specErr := r.buildTemplate(cg, now)
	obs, err := r.Workload.Observe(ctx, cg)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("observe the workload of CapacityGrant %s: %w", req.NamespacedName, err)
	}
	blk := blocked(obs, specErr)
	if blk == nil {
		if err := r.Workload.Apply(ctx, cg, tmpl); err != nil {
			if !apierrors.IsForbidden(err) {
				return ctrl.Result{}, fmt.Errorf("apply the workload of CapacityGrant %s: %w", req.NamespacedName, err)
			}
			blk = &blockage{reason: v1alpha1.ReasonForbidden, action: actionApply, message: forbiddenMessage(cg.Namespace, err)}
		}
	}

	var result ctrl.Result
	if blk != nil {
		log.FromContext(ctx).V(2).Info("Blocked reconcile", "reason", blk.reason, "action", blk.action)
		if blk.reason != v1alpha1.ReasonInvalidSpec {
			r.Recorder.Eventf(cg, nil, corev1.EventTypeWarning, blk.reason, blk.action, "%s", blk.message)
			result.RequeueAfter = blockedRequeue
		}
	}

	status := computeStatus(cg.Generation, obs, blk, now, cg.Status)
	if equality.Semantic.DeepEqual(status, cg.Status) {
		return result, nil
	}
	before := cg.DeepCopy()
	cg.Status = status
	if err := r.Client.Status().Patch(ctx, cg, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch the status of CapacityGrant %s: %w", req.NamespacedName, err)
	}
	return result, nil
}

// buildTemplate checks the spec of cg and builds its grant Pod template.
// The error lists every fault it found, for the grant's status.
func (r *CapacityGrantReconciler) buildTemplate(cg *v1alpha1.CapacityGrant, now time.Time) (corev1.PodTemplateSpec, error) {
	var errs []error
	image := tenantImage(cg)
	if image == "" {
		errs = append(errs, field.Required(
			field.NewPath("spec", "podTemplate", "spec", "containers").Key(podtemplate.ContainerName).Child("image"),
			"the fiberd container needs an image"))
	}
	if err := issuer.ValidateSpec(cg, now, r.Template.Lease); err != nil {
		errs = append(errs, err)
	}
	// Build refuses an empty image before it checks anything else, so it
	// would only repeat the fault above.
	var tmpl corev1.PodTemplateSpec
	if image != "" {
		var err error
		if tmpl, err = podtemplate.Build(cg, image, r.Template); err != nil {
			errs = append(errs, err)
		}
	}
	if err := joinFaults(errs...); err != nil {
		return corev1.PodTemplateSpec{}, err
	}
	return tmpl, nil
}

// joinFaults returns one error that lists each fault of errs once.
// issuer.ValidateSpec and podtemplate.Build check some fields alike, such
// as the runtime, and each reports the fault in the same words. An
// aggregate, even one wrapped in another error, is opened into its faults
// so that such a fault is listed once. It returns nil when errs holds no
// error.
func joinFaults(errs ...error) error {
	var all faults
	seen := sets.New[string]()
	add := func(err error) {
		if msg := err.Error(); !seen.Has(msg) {
			seen.Insert(msg)
			all = append(all, err)
		}
	}
	for _, err := range errs {
		if err == nil {
			continue
		}
		var agg utilerrors.Aggregate
		if !errors.As(err, &agg) {
			add(err)
			continue
		}
		for _, fault := range utilerrors.Flatten(agg).Errors() {
			add(fault)
		}
	}
	if len(all) == 0 {
		return nil
	}
	return all
}

// faults is the error of a spec with one or more faults. Its message gives
// each fault as a sentence, so it reads as prose in a condition message.
type faults []error

// Error returns the message of each fault as a sentence, separated by a
// space.
func (f faults) Error() string {
	msgs := make([]string, len(f))
	for i, err := range f {
		msgs[i] = sentence(err.Error())
	}
	return strings.Join(msgs, " ")
}

// Unwrap returns the faults, so errors.Is and errors.As see each of them.
func (f faults) Unwrap() []error {
	return f
}

// sentence returns msg ending with a period. A message that already ends
// with a period, a question mark or an exclamation mark is returned
// unchanged.
func sentence(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" || strings.HasSuffix(msg, ".") || strings.HasSuffix(msg, "?") || strings.HasSuffix(msg, "!") {
		return msg
	}
	return msg + "."
}

// tenantImage returns the image the tenant set on the fiberd container of
// cg, or an empty string when there is none.
func tenantImage(cg *v1alpha1.CapacityGrant) string {
	for _, c := range cg.Spec.PodTemplate.Spec.Containers {
		if c.Name == podtemplate.ContainerName {
			return c.Image
		}
	}
	return ""
}

// forbiddenMessage explains a Forbidden apply in namespace. A missing
// tenant RoleBinding is the usual cause, but quota and a terminating
// namespace are refused as Forbidden too, so the API error follows as a
// second sentence.
func forbiddenMessage(namespace string, err error) string {
	return fmt.Sprintf("The controller may lack the tenant RoleBinding for ClusterRole %s in namespace %s. The apply failed with %s",
		tenantClusterRole, namespace, sentence(err.Error()))
}
