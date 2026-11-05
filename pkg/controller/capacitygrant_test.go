package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload/deployment"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload/podtemplate"
)

const (
	testNamespace = "team-a"
	testName      = "web"
	testImage     = "ghcr.io/azure/fiberd:v0.4.0"
)

var (
	testKey     = types.NamespacedName{Namespace: testNamespace, Name: testName}
	objectKey   = types.NamespacedName{Namespace: testNamespace, Name: "fiberd-web"}
	testOptions = podtemplate.Options{IssuerURL: "https://fiberd-k8s-controller.fiberd-system.svc", Lease: 10 * time.Minute}
	errBoom     = errors.New("boom")

	// forbidden is what a namespace without the tenant RoleBinding answers
	// an apply of the Service with.
	forbidden = forbiddenFor("Service")

	// lastStatus is a status an earlier pass wrote. A failed pass must leave
	// it as it is.
	lastStatus = v1alpha1.CapacityGrantStatus{
		ObservedGeneration: 3,
		Conditions: []metav1.Condition{
			cond(v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonAvailable, msgAvailable, earlier),
			cond(v1alpha1.ConditionProgressing, metav1.ConditionTrue, rolledOut.Reason, rolledOut.Message, earlier),
		},
	}

	// ignoreVersion leaves out what the fake changes on every write, even
	// one that changes nothing.
	ignoreVersion = cmp.Options{
		cmpopts.IgnoreFields(metav1.ObjectMeta{}, "ResourceVersion", "ManagedFields"),
		cmpopts.IgnoreTypes(metav1.TypeMeta{}),
	}
)

// forbiddenFor is the Forbidden error the API server answers an apply of a
// kind with when the tenant RoleBinding is missing.
func forbiddenFor(kind string) error {
	return apierrors.NewForbidden(schema.GroupResource{Resource: strings.ToLower(kind) + "s"}, "fiberd-web", errors.New("no tenant RoleBinding"))
}

// wantForbiddenMessage is the Forbidden message for an apply error.
func wantForbiddenMessage(err string) string {
	return "The controller may lack the tenant RoleBinding for ClusterRole fiberd-k8s-controller-tenant in namespace team-a. The apply failed with " + err + "."
}

// fakeWorkload is a Workload whose methods call its function fields and
// record their calls. A nil field observes nothing and applies without an
// error.
type fakeWorkload struct {
	observe func(ctx context.Context, cg *v1alpha1.CapacityGrant) (workload.Observation, error)
	apply   func(ctx context.Context, cg *v1alpha1.CapacityGrant, tmpl corev1.PodTemplateSpec) error

	observed int
	applied  []corev1.PodTemplateSpec
}

var _ workload.Workload = &fakeWorkload{}

func (w *fakeWorkload) Observe(ctx context.Context, cg *v1alpha1.CapacityGrant) (workload.Observation, error) {
	w.observed++
	if w.observe == nil {
		return workload.Observation{}, nil
	}
	return w.observe(ctx, cg)
}

func (w *fakeWorkload) Apply(ctx context.Context, cg *v1alpha1.CapacityGrant, tmpl corev1.PodTemplateSpec) error {
	w.applied = append(w.applied, *tmpl.DeepCopy())
	if w.apply == nil {
		return nil
	}
	return w.apply(ctx, cg, tmpl)
}

// observes returns an observe function that reports obs.
func observes(obs workload.Observation) func(context.Context, *v1alpha1.CapacityGrant) (workload.Observation, error) {
	return func(context.Context, *v1alpha1.CapacityGrant) (workload.Observation, error) { return obs, nil }
}

// testGrant is a valid grant of generation 3 with no status. Each mod
// changes it in place.
func testGrant(mods ...func(*v1alpha1.CapacityGrant)) *v1alpha1.CapacityGrant {
	cg := &v1alpha1.CapacityGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testName,
			Namespace:  testNamespace,
			UID:        "3f2a6c1e-8b9d-4e5f-a1b2-c3d4e5f6a7b8",
			Generation: 3,
		},
		Spec: v1alpha1.CapacityGrantSpec{
			Zygote:  v1alpha1.ZygoteSpec{Digest: "sha256:" + strings.Repeat("ab", 32), Command: []string{"/app/zygote"}},
			Fibers:  v1alpha1.FiberBudget{Max: 4},
			Runtime: v1alpha1.RuntimeProc,
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  podtemplate.ContainerName,
					Image: testImage,
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
					},
				}}},
			},
		},
	}
	for _, mod := range mods {
		mod(cg)
	}
	return cg
}

func withStatus(status v1alpha1.CapacityGrantStatus) func(*v1alpha1.CapacityGrant) {
	return func(cg *v1alpha1.CapacityGrant) { cg.Status = *status.DeepCopy() }
}

func withoutImage(cg *v1alpha1.CapacityGrant) { cg.Spec.PodTemplate.Spec.Containers[0].Image = "" }

// failures are the requests the fake client of a test refuses.
type failures struct {
	// get fails every Get of a grant.
	get error
	// statusPatch fails every status patch.
	statusPatch error
	// applyKind is a kind whose every apply is Forbidden.
	applyKind string
}

// testEnv is the reconciler of one test and what it talks to.
type testEnv struct {
	r        *CapacityGrantReconciler
	base     client.Client
	recorder *events.FakeRecorder
	clock    *clocktesting.FakePassiveClock
	patches  *int
}

// newTestEnv stores objs in a fake client that refuses what fail names and
// counts every status patch. The reconciler's Workload is left for the
// test to set.
func newTestEnv(t *testing.T, objs []client.Object, fail failures) *testEnv {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add the client-go scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add the v1alpha1 scheme: %v", err)
	}
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.CapacityGrant{}).
		WithObjects(objs...).
		Build()
	patches := 0
	c := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*v1alpha1.CapacityGrant); ok && fail.get != nil {
				return fail.get
			}
			return c.Get(ctx, key, obj, opts...)
		},
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if k, ok := obj.(interface{ GetKind() *string }); ok && fail.applyKind != "" && ptr.Deref(k.GetKind(), "") == fail.applyKind {
				return forbiddenFor(fail.applyKind)
			}
			return c.Apply(ctx, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			patches++
			if fail.statusPatch != nil {
				return fail.statusPatch
			}
			return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
		},
	})
	env := &testEnv{
		base:     base,
		recorder: events.NewFakeRecorder(10),
		clock:    clocktesting.NewFakePassiveClock(later),
		patches:  &patches,
	}
	env.r = &CapacityGrantReconciler{
		Client:   c,
		Template: testOptions,
		Recorder: env.recorder,
		Clock:    env.clock,
	}
	return env
}

// reconcile runs one pass at time at.
func (e *testEnv) reconcile(at time.Time) (ctrl.Result, error) {
	e.clock.SetTime(at)
	return e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: testKey})
}

// events drains the Events recorded so far.
func (e *testEnv) events() []string {
	var got []string
	for {
		select {
		case ev := <-e.recorder.Events:
			got = append(got, ev)
		default:
			return got
		}
	}
}

// stored returns the grant as the fake client holds it.
func (e *testEnv) stored(t *testing.T) *v1alpha1.CapacityGrant {
	t.Helper()
	cg := &v1alpha1.CapacityGrant{}
	if err := e.base.Get(context.Background(), testKey, cg); err != nil {
		t.Fatalf("get the stored grant: %v", err)
	}
	return cg
}

// checkReadyMessage fails the test unless the Ready condition of cg holds
// want exactly once.
func checkReadyMessage(t *testing.T, cg *v1alpha1.CapacityGrant, want string) {
	t.Helper()
	ready := meta.FindStatusCondition(cg.Status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || strings.Count(ready.Message, want) != 1 {
		t.Errorf("Ready condition = %+v, want a message with %q once", ready, want)
	}
}

func TestReconcile(t *testing.T) {
	msgForbidden := wantForbiddenMessage(forbidden.Error())
	deleting := func(cg *v1alpha1.CapacityGrant) {
		now := metav1.NewTime(earlier)
		cg.DeletionTimestamp = &now
		cg.Finalizers = []string{v1alpha1.Finalizer}
	}
	foreign := workload.Observation{ConflictReason: v1alpha1.ReasonDeploymentConflict, Message: msgConflict}
	serving := workload.Observation{Found: true, Available: 1, Progressing: rolledOut}

	cases := []struct {
		name string
		// grant is the stored grant. Nil means there is none.
		grant   *v1alpha1.CapacityGrant
		observe func(context.Context, *v1alpha1.CapacityGrant) (workload.Observation, error)
		apply   func(context.Context, *v1alpha1.CapacityGrant, corev1.PodTemplateSpec) error
		fail    failures

		wantErr      error
		wantResult   ctrl.Result
		wantObserves int
		wantApplies  int
		wantPatches  int
		// wantStatus is the stored status after the pass, messages aside.
		// Nil means the grant's status before the pass.
		wantStatus *v1alpha1.CapacityGrantStatus
		// wantMessage appears exactly once in the Ready message.
		wantMessage string
		wantEvents  []string
	}{
		{
			name: "a missing grant writes nothing",
		},
		{
			name:  "a grant being deleted writes nothing",
			grant: testGrant(deleting, withStatus(lastStatus)),
		},
		{
			name:         "a new grant is applied and pending",
			grant:        testGrant(),
			wantObserves: 1,
			wantApplies:  1,
			wantPatches:  1,
			wantStatus: &v1alpha1.CapacityGrantStatus{
				ObservedGeneration: 3,
				Conditions: []metav1.Condition{
					cond(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonPending, "", later),
					cond(v1alpha1.ConditionProgressing, metav1.ConditionUnknown, v1alpha1.ReasonPending, "", later),
				},
			},
			wantMessage: msgPending,
		},
		{
			name:         "a grant with an available Deployment is ready",
			grant:        testGrant(),
			observe:      observes(serving),
			wantObserves: 1,
			wantApplies:  1,
			wantPatches:  1,
			wantStatus: &v1alpha1.CapacityGrantStatus{
				ObservedGeneration: 3,
				Conditions: []metav1.Condition{
					cond(v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonAvailable, "", later),
					cond(v1alpha1.ConditionProgressing, metav1.ConditionTrue, rolledOut.Reason, "", later),
				},
			},
			wantMessage: msgAvailable,
		},
		{
			name:         "a status that is already current is not patched",
			grant:        testGrant(withStatus(lastStatus)),
			observe:      observes(serving),
			wantObserves: 1,
			wantApplies:  1,
		},
		{
			name:         "a grant without an image is InvalidSpec and applies nothing",
			grant:        testGrant(withoutImage),
			wantObserves: 1,
			wantPatches:  1,
			wantStatus: &v1alpha1.CapacityGrantStatus{
				ObservedGeneration: 3,
				Conditions: []metav1.Condition{
					cond(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonInvalidSpec, "", later),
					cond(v1alpha1.ConditionProgressing, metav1.ConditionUnknown, v1alpha1.ReasonInvalidSpec, "", later),
				},
			},
			wantMessage: "spec.podTemplate.spec.containers[fiberd].image: Required value",
		},
		{
			name:         "a zero fibers.max is InvalidSpec",
			grant:        testGrant(func(cg *v1alpha1.CapacityGrant) { cg.Spec.Fibers.Max = 0 }),
			wantObserves: 1,
			wantPatches:  1,
			wantStatus: &v1alpha1.CapacityGrantStatus{
				ObservedGeneration: 3,
				Conditions: []metav1.Condition{
					cond(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonInvalidSpec, "", later),
					cond(v1alpha1.ConditionProgressing, metav1.ConditionUnknown, v1alpha1.ReasonInvalidSpec, "", later),
				},
			},
			wantMessage: "spec.fibers.max",
		},
		{
			name: "an arg the controller owns is InvalidSpec",
			grant: testGrant(func(cg *v1alpha1.CapacityGrant) {
				cg.Spec.PodTemplate.Spec.Containers[0].Args = []string{"-issuer=https://elsewhere.example"}
			}),
			wantObserves: 1,
			wantPatches:  1,
			wantStatus: &v1alpha1.CapacityGrantStatus{
				ObservedGeneration: 3,
				Conditions: []metav1.Condition{
					cond(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonInvalidSpec, "", later),
					cond(v1alpha1.ConditionProgressing, metav1.ConditionUnknown, v1alpha1.ReasonInvalidSpec, "", later),
				},
			},
			wantMessage: "spec.podTemplate.spec.containers[0].args",
		},
		{
			// issuer.ValidateSpec and podtemplate.Build both refuse an
			// unknown runtime, in the same words.
			name:         "a fault both checks find is listed once",
			grant:        testGrant(func(cg *v1alpha1.CapacityGrant) { cg.Spec.Runtime = "kvm" }),
			wantObserves: 1,
			wantPatches:  1,
			wantStatus: &v1alpha1.CapacityGrantStatus{
				ObservedGeneration: 3,
				Conditions: []metav1.Condition{
					cond(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonInvalidSpec, "", later),
					cond(v1alpha1.ConditionProgressing, metav1.ConditionUnknown, v1alpha1.ReasonInvalidSpec, "", later),
				},
			},
			wantMessage: `spec.runtime: Unsupported value: "kvm"`,
		},
		{
			name:         "several faults read as separate sentences",
			grant:        testGrant(withoutImage, func(cg *v1alpha1.CapacityGrant) { cg.Spec.Fibers.Max = 0 }),
			wantObserves: 1,
			wantPatches:  1,
			wantStatus: &v1alpha1.CapacityGrantStatus{
				ObservedGeneration: 3,
				Conditions: []metav1.Condition{
					cond(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonInvalidSpec, "", later),
					cond(v1alpha1.ConditionProgressing, metav1.ConditionUnknown, v1alpha1.ReasonInvalidSpec, "", later),
				},
			},
			wantMessage: "the fiberd container needs an image. spec.fibers.max: Invalid value: 0: must be at least 1.",
		},
		{
			name:         "InvalidSpec keeps the serving Deployment and its progress",
			grant:        testGrant(withoutImage, withStatus(lastStatus)),
			observe:      observes(serving),
			wantObserves: 1,
			wantPatches:  1,
			wantStatus: &v1alpha1.CapacityGrantStatus{
				ObservedGeneration: 3,
				Conditions: []metav1.Condition{
					cond(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonInvalidSpec, "", later),
					cond(v1alpha1.ConditionProgressing, metav1.ConditionTrue, rolledOut.Reason, "", earlier),
				},
			},
			wantMessage: "Required value",
		},
		{
			name:         "a foreign Deployment is left alone",
			grant:        testGrant(),
			observe:      observes(foreign),
			wantResult:   ctrl.Result{RequeueAfter: time.Minute},
			wantObserves: 1,
			wantPatches:  1,
			wantStatus: &v1alpha1.CapacityGrantStatus{
				ObservedGeneration: 3,
				Conditions: []metav1.Condition{
					cond(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonDeploymentConflict, "", later),
					cond(v1alpha1.ConditionProgressing, metav1.ConditionUnknown, v1alpha1.ReasonDeploymentConflict, "", later),
				},
			},
			wantMessage: msgConflict,
			wantEvents:  []string{"Warning DeploymentConflict " + msgConflict},
		},
		{
			name:         "a foreign Service is left alone",
			grant:        testGrant(),
			observe:      observes(workload.Observation{ConflictReason: v1alpha1.ReasonServiceConflict, Message: msgServiceConflict}),
			wantResult:   ctrl.Result{RequeueAfter: time.Minute},
			wantObserves: 1,
			wantPatches:  1,
			wantStatus: &v1alpha1.CapacityGrantStatus{
				ObservedGeneration: 3,
				Conditions: []metav1.Condition{
					cond(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonServiceConflict, "", later),
					cond(v1alpha1.ConditionProgressing, metav1.ConditionUnknown, v1alpha1.ReasonServiceConflict, "", later),
				},
			},
			wantMessage: msgServiceConflict,
			wantEvents:  []string{"Warning ServiceConflict " + msgServiceConflict},
		},
		{
			name:         "a conflict wins over an invalid spec",
			grant:        testGrant(withoutImage),
			observe:      observes(foreign),
			wantResult:   ctrl.Result{RequeueAfter: time.Minute},
			wantObserves: 1,
			wantPatches:  1,
			wantStatus: &v1alpha1.CapacityGrantStatus{
				ObservedGeneration: 3,
				Conditions: []metav1.Condition{
					cond(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonDeploymentConflict, "", later),
					cond(v1alpha1.ConditionProgressing, metav1.ConditionUnknown, v1alpha1.ReasonDeploymentConflict, "", later),
				},
			},
			wantMessage: msgConflict,
			wantEvents:  []string{"Warning DeploymentConflict " + msgConflict},
		},
		{
			name:         "a forbidden apply blocks the pass as Forbidden",
			grant:        testGrant(withStatus(lastStatus)),
			observe:      observes(serving),
			apply:        func(context.Context, *v1alpha1.CapacityGrant, corev1.PodTemplateSpec) error { return forbidden },
			wantResult:   ctrl.Result{RequeueAfter: time.Minute},
			wantObserves: 1,
			wantApplies:  1,
			wantPatches:  1,
			wantStatus: &v1alpha1.CapacityGrantStatus{
				ObservedGeneration: 3,
				Conditions: []metav1.Condition{
					cond(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonForbidden, "", later),
					cond(v1alpha1.ConditionProgressing, metav1.ConditionUnknown, v1alpha1.ReasonForbidden, "", later),
				},
			},
			wantMessage: msgForbidden,
			wantEvents:  []string{"Warning Forbidden " + msgForbidden},
		},
		{
			name:    "a failed grant read keeps the last status",
			grant:   testGrant(withStatus(lastStatus)),
			fail:    failures{get: errBoom},
			wantErr: errBoom,
		},
		{
			name:  "a failed observe keeps the last status",
			grant: testGrant(withStatus(lastStatus)),
			observe: func(context.Context, *v1alpha1.CapacityGrant) (workload.Observation, error) {
				return workload.Observation{}, errBoom
			},
			wantErr:      errBoom,
			wantObserves: 1,
		},
		{
			name:         "a failed apply keeps the last status",
			grant:        testGrant(withStatus(lastStatus)),
			apply:        func(context.Context, *v1alpha1.CapacityGrant, corev1.PodTemplateSpec) error { return errBoom },
			wantErr:      errBoom,
			wantObserves: 1,
			wantApplies:  1,
		},
		{
			name:         "a failed status patch keeps the last status",
			grant:        testGrant(withStatus(lastStatus)),
			fail:         failures{statusPatch: errBoom},
			wantErr:      errBoom,
			wantObserves: 1,
			wantApplies:  1,
			wantPatches:  1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var objs []client.Object
			if tc.grant != nil {
				objs = append(objs, tc.grant.DeepCopy())
			}
			env := newTestEnv(t, objs, tc.fail)
			w := &fakeWorkload{observe: tc.observe, apply: tc.apply}
			env.r.Workload = w

			got, err := env.reconcile(later)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Reconcile error = %v, want %v", err, tc.wantErr)
			}
			if got != tc.wantResult {
				t.Errorf("Reconcile result = %+v, want %+v", got, tc.wantResult)
			}
			if w.observed != tc.wantObserves {
				t.Errorf("Observe calls = %d, want %d", w.observed, tc.wantObserves)
			}
			if len(w.applied) != tc.wantApplies {
				t.Errorf("Apply calls = %d, want %d", len(w.applied), tc.wantApplies)
			}
			if *env.patches != tc.wantPatches {
				t.Errorf("status patches = %d, want %d", *env.patches, tc.wantPatches)
			}
			if diff := cmp.Diff(tc.wantEvents, env.events(), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Events (-want +got):\n%s", diff)
			}
			if tc.grant == nil {
				return
			}

			stored := env.stored(t)
			if tc.wantStatus == nil {
				if diff := cmp.Diff(tc.grant.Status, stored.Status); diff != "" {
					t.Errorf("stored status changed (-before +after):\n%s", diff)
				}
				return
			}
			if diff := cmp.Diff(*tc.wantStatus, stored.Status, cmpopts.IgnoreFields(metav1.Condition{}, "Message")); diff != "" {
				t.Errorf("stored status (-want +got):\n%s", diff)
			}
			checkReadyMessage(t, stored, tc.wantMessage)
		})
	}
}

// pass is one Reconcile of a grant and what the grant holds after it.
type pass struct {
	at  time.Time
	obs workload.Observation
	// wantPatches is the number of status patches up to and with this pass.
	wantPatches int
	// wantTransitions are the LastTransitionTime of each condition type.
	wantTransitions map[string]time.Time
}

func TestReconcileAcrossPasses(t *testing.T) {
	t0 := later
	t1 := later.Add(time.Minute)
	t2 := later.Add(2 * time.Minute)
	starting := workload.Observation{Found: true, Progressing: metav1.Condition{
		Type: v1alpha1.ConditionProgressing, Status: metav1.ConditionTrue, Reason: "ReplicaSetUpdated", Message: `ReplicaSet "fiberd-web-7d9f8c6b5" is progressing.`,
	}}
	serving := workload.Observation{Found: true, Available: 1, Progressing: rolledOut}

	cases := []struct {
		name   string
		passes []pass
	}{
		{
			name: "a second identical pass patches no status",
			passes: []pass{
				{at: t0, wantPatches: 1, wantTransitions: map[string]time.Time{v1alpha1.ConditionReady: t0, v1alpha1.ConditionProgressing: t0}},
				{at: t1, wantPatches: 1, wantTransitions: map[string]time.Time{v1alpha1.ConditionReady: t0, v1alpha1.ConditionProgressing: t0}},
			},
		},
		{
			name: "transition times survive the passes where a status holds",
			passes: []pass{
				{at: t0, wantPatches: 1, wantTransitions: map[string]time.Time{v1alpha1.ConditionReady: t0, v1alpha1.ConditionProgressing: t0}},
				{at: t1, obs: starting, wantPatches: 2, wantTransitions: map[string]time.Time{v1alpha1.ConditionReady: t0, v1alpha1.ConditionProgressing: t1}},
				{at: t2, obs: serving, wantPatches: 3, wantTransitions: map[string]time.Time{v1alpha1.ConditionReady: t2, v1alpha1.ConditionProgressing: t1}},
				{at: t2.Add(time.Minute), obs: serving, wantPatches: 3, wantTransitions: map[string]time.Time{v1alpha1.ConditionReady: t2, v1alpha1.ConditionProgressing: t1}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cg := testGrant()
			want, err := podtemplate.Build(cg, testImage, workload.ObjectName(cg), testOptions)
			if err != nil {
				t.Fatalf("podtemplate.Build: %v", err)
			}
			var obs workload.Observation
			w := &fakeWorkload{observe: func(context.Context, *v1alpha1.CapacityGrant) (workload.Observation, error) { return obs, nil }}
			env := newTestEnv(t, []client.Object{cg}, failures{})
			env.r.Workload = w

			for i, p := range tc.passes {
				obs = p.obs
				if _, err := env.reconcile(p.at); err != nil {
					t.Fatalf("pass %d: Reconcile: %v", i, err)
				}
				if *env.patches != p.wantPatches {
					t.Errorf("pass %d: status patches = %d, want %d", i, *env.patches, p.wantPatches)
				}
				stored := env.stored(t)
				for conditionType, at := range p.wantTransitions {
					c := meta.FindStatusCondition(stored.Status.Conditions, conditionType)
					if c == nil || !c.LastTransitionTime.Time.Equal(at) {
						t.Errorf("pass %d: %s condition = %+v, want LastTransitionTime %s", i, conditionType, c, at)
					}
				}
			}
			if len(w.applied) != len(tc.passes) {
				t.Fatalf("Apply calls = %d, want %d", len(w.applied), len(tc.passes))
			}
			for i, got := range w.applied {
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("pass %d applied another template (-want +got):\n%s", i, diff)
				}
			}
		})
	}
}

// TestReconcileWithDeployment runs the reconciler with the real
// deployment.Workload on the fake client, which runs server-side apply
// without defaulting or admission. The fake bumps resourceVersion on every
// write, even one that changes nothing, so stored objects are compared
// without it, and whether an identical apply keeps it is left to envtest.
func TestReconcileWithDeployment(t *testing.T) {
	msgForbidden := wantForbiddenMessage("apply Service team-a/fiberd-web: " + forbiddenFor("Service").Error())
	foreignDeployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "fiberd-web", Namespace: testNamespace, Labels: map[string]string{"app": "other"}},
	}

	cases := []struct {
		name string
		// foreign is a Deployment of the grant's object name that the grant
		// does not control. Nil means there is none.
		foreign *appsv1.Deployment
		fail    failures
		// wantPatches is the number of status patches after each pass.
		wantPatches []int
		wantResult  ctrl.Result
		wantReady   string
		// wantMessage appears exactly once in the Ready message, when set.
		wantMessage string
		wantEvents  []string
		// wantOwned says the grant controls a stored Service and Deployment.
		// Otherwise neither is stored, apart from foreign.
		wantOwned bool
	}{
		{
			name:        "a new grant gets its Service and Deployment",
			wantPatches: []int{1},
			wantReady:   v1alpha1.ReasonPending,
			wantMessage: msgPending,
			wantOwned:   true,
		},
		{
			name:        "a second identical pass patches no status",
			wantPatches: []int{1, 2, 2},
			wantReady:   v1alpha1.ReasonUnavailable,
			wantMessage: msgUnavailable,
			wantOwned:   true,
		},
		{
			name:        "a forbidden Service apply blocks the pass as Forbidden",
			fail:        failures{applyKind: "Service"},
			wantPatches: []int{1},
			wantResult:  ctrl.Result{RequeueAfter: time.Minute},
			wantReady:   v1alpha1.ReasonForbidden,
			wantMessage: msgForbidden,
			wantEvents:  []string{"Warning Forbidden " + msgForbidden},
		},
		{
			name:        "a foreign Deployment is left alone",
			foreign:     foreignDeployment,
			wantPatches: []int{1, 1},
			wantResult:  ctrl.Result{RequeueAfter: time.Minute},
			wantReady:   v1alpha1.ReasonDeploymentConflict,
			wantMessage: msgConflict,
			wantEvents:  []string{"Warning DeploymentConflict " + msgConflict, "Warning DeploymentConflict " + msgConflict},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []client.Object{testGrant()}
			if tc.foreign != nil {
				objs = append(objs, tc.foreign.DeepCopy())
			}
			env := newTestEnv(t, objs, tc.fail)
			env.r.Workload = deployment.New(env.r.Client)
			ctx := context.Background()

			var last ctrl.Result
			var prevD *appsv1.Deployment
			var prevSvc *corev1.Service
			for i, wantPatches := range tc.wantPatches {
				var err error
				if last, err = env.reconcile(later.Add(time.Duration(i) * time.Minute)); err != nil {
					t.Fatalf("pass %d: Reconcile: %v", i, err)
				}
				if *env.patches != wantPatches {
					t.Errorf("pass %d: status patches = %d, want %d", i, *env.patches, wantPatches)
				}
				if !tc.wantOwned {
					continue
				}
				d, svc := &appsv1.Deployment{}, &corev1.Service{}
				if err := env.base.Get(ctx, objectKey, d); err != nil {
					t.Fatalf("pass %d: get the Deployment: %v", i, err)
				}
				if err := env.base.Get(ctx, objectKey, svc); err != nil {
					t.Fatalf("pass %d: get the Service: %v", i, err)
				}
				if prevD != nil {
					if diff := cmp.Diff(prevD, d, ignoreVersion); diff != "" {
						t.Errorf("pass %d changed the Deployment (-before +after):\n%s", i, diff)
					}
					if diff := cmp.Diff(prevSvc, svc, ignoreVersion); diff != "" {
						t.Errorf("pass %d changed the Service (-before +after):\n%s", i, diff)
					}
				}
				prevD, prevSvc = d, svc
			}
			if last != tc.wantResult {
				t.Errorf("last Reconcile result = %+v, want %+v", last, tc.wantResult)
			}
			if diff := cmp.Diff(tc.wantEvents, env.events(), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Events (-want +got):\n%s", diff)
			}

			stored := env.stored(t)
			ready := meta.FindStatusCondition(stored.Status.Conditions, v1alpha1.ConditionReady)
			if ready == nil || ready.Reason != tc.wantReady {
				t.Errorf("Ready condition = %+v, want reason %s", ready, tc.wantReady)
			}
			checkReadyMessage(t, stored, tc.wantMessage)

			d, svc := &appsv1.Deployment{}, &corev1.Service{}
			dErr := env.base.Get(ctx, objectKey, d)
			svcErr := env.base.Get(ctx, objectKey, svc)
			switch {
			case tc.wantOwned:
				if dErr != nil || svcErr != nil {
					t.Fatalf("get the Deployment: %v, get the Service: %v", dErr, svcErr)
				}
				if !metav1.IsControlledBy(d, stored) || !metav1.IsControlledBy(svc, stored) {
					t.Errorf("owner references are %v and %v, want the grant as controller of both", d.OwnerReferences, svc.OwnerReferences)
				}
			case tc.foreign != nil:
				if dErr != nil {
					t.Fatalf("get the Deployment: %v", dErr)
				}
				if diff := cmp.Diff(tc.foreign, d, ignoreVersion); diff != "" {
					t.Errorf("the foreign Deployment changed (-before +after):\n%s", diff)
				}
			default:
				if !apierrors.IsNotFound(dErr) {
					t.Errorf("get the Deployment = %v, want NotFound", dErr)
				}
			}
			if !tc.wantOwned && !apierrors.IsNotFound(svcErr) {
				t.Errorf("get the Service = %v, want NotFound", svcErr)
			}
		})
	}
}

func TestJoinFaults(t *testing.T) {
	a, b, c := errors.New("a"), errors.New("b"), errors.New("c")
	cases := []struct {
		name string
		errs []error
		// want is the message of the joined error. Empty means nil.
		want string
	}{
		{
			name: "only nil errors join to nil",
			errs: []error{nil, nil},
		},
		{
			name: "one plain error becomes one sentence",
			errs: []error{a},
			want: "a.",
		},
		{
			name: "a fault that ends with a period gets no second one",
			errs: []error{errors.New("a.")},
			want: "a.",
		},
		{
			name: "a wrapped aggregate is opened into sentences",
			errs: []error{fmt.Errorf("issuer: CapacityGrant team-a/web: %w", utilerrors.NewAggregate([]error{a, b}))},
			want: "a. b.",
		},
		{
			name: "a fault in two lists is listed once, in first order",
			errs: []error{fmt.Errorf("issuer: %w", utilerrors.NewAggregate([]error{a, b})), utilerrors.NewAggregate([]error{b, c}), c},
			want: "a. b. c.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := joinFaults(tc.errs...)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("joinFaults = %v, want nil", err)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Fatalf("joinFaults = %v, want %s", err, tc.want)
			}
		})
	}
}

func TestSetupWithManagerNeedsFields(t *testing.T) {
	cases := []struct {
		name string
		r    *CapacityGrantReconciler
		want []string
	}{
		{
			name: "an empty reconciler names every missing field",
			r:    &CapacityGrantReconciler{},
			want: []string{"Client", "Workload", "Recorder"},
		},
		{
			name: "a reconciler without a Recorder names only the Recorder",
			r:    &CapacityGrantReconciler{Client: fake.NewClientBuilder().Build(), Workload: &fakeWorkload{}},
			want: []string{"Recorder"},
		},
	}
	all := []string{"Client", "Workload", "Recorder"}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The fields are checked before the manager is used, so none is
			// needed.
			err := tc.r.SetupWithManager(nil)
			if err == nil {
				t.Fatal("SetupWithManager returned nil, want an error")
			}
			for _, msg := range all {
				if got, want := strings.Contains(err.Error(), msg), slices.Contains(tc.want, msg); got != want {
					t.Errorf("SetupWithManager error %q names %q: %t, want %t", err, msg, got, want)
				}
			}
		})
	}
}
