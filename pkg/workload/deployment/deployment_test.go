package deployment

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload"
)

var errRead = errors.New("connection refused")

// objectMeta is the metadata of an object named fiberd-web in team-a,
// controlled by the grant with uid, or by nothing when uid is empty.
func objectMeta(uid string) metav1.ObjectMeta {
	m := metav1.ObjectMeta{Name: "fiberd-web", Namespace: "team-a"}
	if uid != "" {
		m.OwnerReferences = []metav1.OwnerReference{controllerRef(uid)}
	}
	return m
}

// servingDeployment is a Deployment with one available Pod, controlled by
// the grant with uid.
func servingDeployment(uid string) *appsv1.Deployment {
	d := &appsv1.Deployment{ObjectMeta: objectMeta(uid)}
	d.Generation = 1
	d.Status = appsv1.DeploymentStatus{
		ObservedGeneration: 1,
		AvailableReplicas:  1,
		Conditions: []appsv1.DeploymentCondition{{
			Type:               appsv1.DeploymentProgressing,
			Status:             corev1.ConditionTrue,
			Reason:             "NewReplicaSetAvailable",
			Message:            `ReplicaSet "fiberd-web-1" has successfully progressed.`,
			LastTransitionTime: metav1.NewTime(time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)),
		}},
	}
	return d
}

func service(uid string) *corev1.Service {
	return &corev1.Service{ObjectMeta: objectMeta(uid)}
}

// failGet fails every Get of an object of obj's type.
func failGet(obj client.Object) interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
			if reflect.TypeOf(o) == reflect.TypeOf(obj) {
				return errRead
			}
			return c.Get(ctx, key, o, opts...)
		},
	}
}

func newClient(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithInterceptorFuncs(funcs).
		Build()
}

// countGets wraps funcs so that every Get adds to n.
func countGets(funcs interceptor.Funcs, n *int) interceptor.Funcs {
	inner := funcs.Get
	funcs.Get = func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		*n++
		if inner != nil {
			return inner(ctx, c, key, obj, opts...)
		}
		return c.Get(ctx, key, obj, opts...)
	}
	return funcs
}

// TestWorkloadObserve reads through a cache that holds objs and an API
// server that also holds hidden, the objects the cache's filter leaves out.
func TestWorkloadObserve(t *testing.T) {
	serving := workload.Observation{
		Found:     true,
		Available: 1,
		Progressing: metav1.Condition{
			Type:    v1alpha1.ConditionProgressing,
			Status:  metav1.ConditionTrue,
			Reason:  "NewReplicaSetAvailable",
			Message: `ReplicaSet "fiberd-web-1" has successfully progressed.`,
		},
	}
	servingService := serving
	servingService.ServiceFound = true

	cases := []struct {
		name      string
		objs      []client.Object
		hidden    []client.Object
		funcs     interceptor.Funcs
		liveFuncs interceptor.Funcs
		want      workload.Observation
		wantErr   error
		// wantLive is how many reads reach the API server.
		wantLive int
	}{
		{
			name:     "a new grant reads each kind live once",
			want:     workload.Observation{},
			wantLive: 2,
		},
		{
			name: "our Deployment and Service in the cache cost no live read",
			objs: []client.Object{servingDeployment(testUID), service(testUID)},
			want: servingService,
		},
		{
			name:     "our Deployment without its Service still observes as found",
			objs:     []client.Object{servingDeployment(testUID)},
			want:     serving,
			wantLive: 1,
		},
		{
			name:     "our Service without its Deployment observes the Service",
			objs:     []client.Object{service(testUID)},
			want:     workload.Observation{ServiceFound: true},
			wantLive: 1,
		},
		{
			name:     "our objects that lost their label observe as found",
			hidden:   []client.Object{servingDeployment(testUID), service(testUID)},
			want:     servingService,
			wantLive: 2,
		},
		{
			name:   "a Deployment the cache hides is a conflict and stops the pass",
			hidden: []client.Object{servingDeployment("")},
			want: workload.Observation{
				ConflictReason: v1alpha1.ReasonDeploymentConflict,
				Message:        "Deployment fiberd-web already exists and is not controlled by CapacityGrant web.",
			},
			wantLive: 1,
		},
		{
			name:   "a Service the cache hides is a conflict",
			objs:   []client.Object{servingDeployment(testUID)},
			hidden: []client.Object{service("")},
			want: workload.Observation{
				ConflictReason: v1alpha1.ReasonServiceConflict,
				Message:        "Service fiberd-web already exists and is not controlled by CapacityGrant web.",
			},
			wantLive: 1,
		},
		{
			name:   "a Service the cache hides beside no Deployment is a conflict",
			hidden: []client.Object{service("")},
			want: workload.Observation{
				ConflictReason: v1alpha1.ReasonServiceConflict,
				Message:        "Service fiberd-web already exists and is not controlled by CapacityGrant web.",
			},
			wantLive: 2,
		},
		{
			name:      "a live Deployment read error is returned",
			liveFuncs: failGet(&appsv1.Deployment{}),
			wantErr:   errRead,
			wantLive:  1,
		},
		{
			name:      "a live Service read error is returned",
			objs:      []client.Object{servingDeployment(testUID)},
			liveFuncs: failGet(&corev1.Service{}),
			wantErr:   errRead,
			wantLive:  1,
		},
		{
			name: "a Deployment with no controller is a conflict",
			objs: []client.Object{servingDeployment(""), service(testUID)},
			want: workload.Observation{
				ConflictReason: v1alpha1.ReasonDeploymentConflict,
				Message:        "Deployment fiberd-web already exists and is not controlled by CapacityGrant web.",
			},
		},
		{
			name: "a Deployment of an earlier grant of the same name is a conflict",
			objs: []client.Object{servingDeployment(otherUID)},
			want: workload.Observation{
				ConflictReason: v1alpha1.ReasonDeploymentConflict,
				Message:        "Deployment fiberd-web already exists and is controlled by CapacityGrant web.",
			},
		},
		{
			name: "a foreign Service is a conflict and nothing is read from our Deployment",
			objs: []client.Object{servingDeployment(testUID), service("")},
			want: workload.Observation{
				ConflictReason: v1alpha1.ReasonServiceConflict,
				Message:        "Service fiberd-web already exists and is not controlled by CapacityGrant web.",
			},
		},
		{
			name: "when both are foreign the conflict names the Deployment",
			objs: []client.Object{servingDeployment(""), service("")},
			want: workload.Observation{
				ConflictReason: v1alpha1.ReasonDeploymentConflict,
				Message:        "Deployment fiberd-web already exists and is not controlled by CapacityGrant web.",
			},
		},
		{
			name:    "a Deployment read error is returned",
			objs:    []client.Object{servingDeployment(testUID), service(testUID)},
			funcs:   failGet(&appsv1.Deployment{}),
			wantErr: errRead,
		},
		{
			name:    "a Service read error is returned",
			objs:    []client.Object{servingDeployment(testUID), service(testUID)},
			funcs:   failGet(&corev1.Service{}),
			wantErr: errRead,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			live := 0
			liveClient := newClient(t, countGets(tc.liveFuncs, &live), slices.Concat(tc.objs, tc.hidden)...)
			w := New(newClient(t, tc.funcs, tc.objs...), liveClient)
			got, err := w.Observe(context.Background(), testGrant())
			if live != tc.wantLive {
				t.Errorf("live reads = %d, want %d", live, tc.wantLive)
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Observe error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Observe: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Observe mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// forbidApply refuses every apply of the given kind as Forbidden, as the
// API server does in a namespace without the tenant RoleBinding.
func forbidApply(kind, resource string) interceptor.Funcs {
	return interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if k, ok := obj.(interface{ GetKind() *string }); ok && ptr.Deref(k.GetKind(), "") == kind {
				return apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "fiberd-web", errors.New("no tenant RoleBinding"))
			}
			return c.Apply(ctx, obj, opts...)
		},
	}
}

// userService is a Service a user created under the grant's object name,
// with a selector and a port of its own.
func userService(*testing.T) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: objectMeta(""),
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app": "user"},
			Ports:    []corev1.ServicePort{{Name: "http", Protocol: corev1.ProtocolTCP, Port: 80}},
		},
	}
}

// editedService is the grant's own Service after a hand edit changed its
// selector and the target of its grpc port.
func editedService(t *testing.T) *corev1.Service {
	t.Helper()
	svc, err := workload.BuildService(testGrant(), testScheme(t))
	if err != nil {
		t.Fatalf("BuildService: %v", err)
	}
	svc.Spec.Selector = map[string]string{"app": "edited"}
	svc.Spec.Ports[0].TargetPort = intstr.FromInt32(9999)
	return svc
}

// relabelledService is the grant's own Service after another manager
// changed its managed-by label, which takes it out of the cache.
func relabelledService(t *testing.T) *corev1.Service {
	t.Helper()
	svc, err := workload.BuildService(testGrant(), testScheme(t))
	if err != nil {
		t.Fatalf("BuildService: %v", err)
	}
	svc.Labels[v1alpha1.LabelManagedBy] = "helm"
	return svc
}

// recordApplies wraps funcs so that every apply is recorded as its kind,
// with " forced" added when it forces ownership.
func recordApplies(funcs interceptor.Funcs, applied *[]string) interceptor.Funcs {
	inner := funcs.Apply
	funcs.Apply = func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
		rec := "unknown"
		if k, ok := obj.(interface{ GetKind() *string }); ok {
			rec = ptr.Deref(k.GetKind(), "")
		}
		for _, o := range opts {
			if o == client.ForceOwnership {
				rec += " forced"
			}
		}
		*applied = append(*applied, rec)
		if inner != nil {
			return inner(ctx, c, obj, opts...)
		}
		return c.Apply(ctx, obj, opts...)
	}
	return funcs
}

// TestWorkloadApply checks one Apply on the fake client. The fake runs
// server-side apply without defaulting or admission, so the stored spec is
// exactly what Build returned.
func TestWorkloadApply(t *testing.T) {
	// A case's existing Service is created by another field manager before
	// Apply. With wantUnchanged it must come out exactly as it went in. obs
	// is what the pass observed.
	ours := workload.Observation{ServiceFound: true}
	cases := []struct {
		name           string
		tmpl           func(*corev1.PodTemplateSpec)
		existing       func(*testing.T) *corev1.Service
		obs            workload.Observation
		funcs          interceptor.Funcs
		wantErr        func(error) bool
		wantApplies    []string
		wantService    bool
		wantUnchanged  bool
		wantDeployment bool
	}{
		{
			name:           "an absent Service is created without forcing",
			wantApplies:    []string{"Service", "Deployment forced"},
			wantService:    true,
			wantDeployment: true,
		},
		{
			name:           "a hand edit to our own Service is restored",
			existing:       editedService,
			obs:            ours,
			wantApplies:    []string{"Service forced", "Deployment forced"},
			wantService:    true,
			wantDeployment: true,
		},
		{
			name:           "a managed-by label another manager changed is restored",
			existing:       relabelledService,
			obs:            ours,
			wantApplies:    []string{"Service forced", "Deployment forced"},
			wantService:    true,
			wantDeployment: true,
		},
		{
			name:          "a foreign Service created since the read refuses the apply",
			existing:      userService,
			wantErr:       apierrors.IsConflict,
			wantApplies:   []string{"Service"},
			wantUnchanged: true,
		},
		{
			name:    "a template that misses the selector sends nothing",
			tmpl:    func(p *corev1.PodTemplateSpec) { delete(p.Labels, v1alpha1.LabelGrant) },
			wantErr: func(err error) bool { return err != nil },
		},
		{
			name:        "a forbidden Service apply sends no Deployment",
			funcs:       forbidApply("Service", "services"),
			wantErr:     apierrors.IsForbidden,
			wantApplies: []string{"Service"},
		},
		{
			name:        "a forbidden Deployment apply keeps the Service",
			funcs:       forbidApply("Deployment", "deployments"),
			wantErr:     apierrors.IsForbidden,
			wantApplies: []string{"Service", "Deployment forced"},
			wantService: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var applied []string
			c := newClient(t, recordApplies(tc.funcs, &applied))
			cg := testGrant()
			tmpl := testTemplate(t, cg)
			if tc.tmpl != nil {
				tc.tmpl(&tmpl)
			}
			key := client.ObjectKey{Namespace: "team-a", Name: "fiberd-web"}
			before := &corev1.Service{}
			if tc.existing != nil {
				if err := c.Create(context.Background(), tc.existing(t), client.FieldOwner("kubectl-edit")); err != nil {
					t.Fatalf("create the existing Service: %v", err)
				}
				if err := c.Get(context.Background(), key, before); err != nil {
					t.Fatalf("get the existing Service: %v", err)
				}
			}

			err := New(c, c).Apply(context.Background(), cg, tmpl, tc.obs)
			if tc.wantErr != nil {
				if !tc.wantErr(err) {
					t.Fatalf("Apply error = %v, not the error wanted", err)
				}
			} else if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if diff := cmp.Diff(tc.wantApplies, applied, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("applies mismatch (-want +got):\n%s", diff)
			}

			svc := &corev1.Service{}
			switch err := c.Get(context.Background(), key, svc); {
			case tc.wantUnchanged && err != nil:
				t.Errorf("get the existing Service: %v", err)
			case tc.wantUnchanged:
				if diff := cmp.Diff(before, svc); diff != "" {
					t.Errorf("Apply changed the existing Service (-before +after):\n%s", diff)
				}
			case !tc.wantService && !apierrors.IsNotFound(err):
				t.Errorf("get Service = %v, want NotFound", err)
			case tc.wantService && err != nil:
				t.Errorf("get Service: %v", err)
			case tc.wantService:
				want, err := workload.BuildService(cg, c.Scheme())
				if err != nil {
					t.Fatalf("BuildService: %v", err)
				}
				if diff := cmp.Diff(want.Spec, svc.Spec, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("stored Service spec mismatch (-want +got):\n%s", diff)
				}
				if diff := cmp.Diff(want.OwnerReferences, svc.OwnerReferences); diff != "" {
					t.Errorf("stored Service owner references mismatch (-want +got):\n%s", diff)
				}
				if diff := cmp.Diff(want.Labels, svc.Labels); diff != "" {
					t.Errorf("stored Service labels mismatch (-want +got):\n%s", diff)
				}
			}

			d := &appsv1.Deployment{}
			switch err := c.Get(context.Background(), key, d); {
			case !tc.wantDeployment && !apierrors.IsNotFound(err):
				t.Errorf("get Deployment = %v, want NotFound", err)
			case tc.wantDeployment && err != nil:
				t.Errorf("get Deployment: %v", err)
			case tc.wantDeployment:
				want, err := Build(cg, tmpl, c.Scheme())
				if err != nil {
					t.Fatalf("Build: %v", err)
				}
				if diff := cmp.Diff(want.Spec, d.Spec, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("stored Deployment spec mismatch (-want +got):\n%s", diff)
				}
				if diff := cmp.Diff(want.OwnerReferences, d.OwnerReferences); diff != "" {
					t.Errorf("stored Deployment owner references mismatch (-want +got):\n%s", diff)
				}
				if diff := cmp.Diff(want.Labels, d.Labels); diff != "" {
					t.Errorf("stored Deployment labels mismatch (-want +got):\n%s", diff)
				}
			}
		})
	}
}

// TestWorkloadApplyTwice applies one grant and then another version of it.
// The fake bumps resourceVersion on every write, even one that changes
// nothing, so whether a second identical apply keeps the resourceVersion is
// left to envtest. Here the stored objects are compared without it.
func TestWorkloadApplyTwice(t *testing.T) {
	cases := []struct {
		name         string
		first        *v1alpha1.CapacityGrant
		second       *v1alpha1.CapacityGrant
		wantStrategy appsv1.DeploymentStrategy
		wantSame     bool
	}{
		{
			name:         "the same input changes nothing",
			first:        testGrant(),
			second:       testGrant(),
			wantStrategy: rollingUpdate,
			wantSame:     true,
		},
		{
			name:         "a device flips the strategy and drops rollingUpdate",
			first:        testGrant(),
			second:       testGrant(withGPU),
			wantStrategy: recreate,
		},
		{
			name:         "removing the device flips the strategy back to a rolling update",
			first:        testGrant(withGPU),
			second:       testGrant(),
			wantStrategy: rollingUpdate,
		},
	}
	ignoreVersion := cmpopts.IgnoreFields(metav1.ObjectMeta{}, "ResourceVersion", "ManagedFields")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, interceptor.Funcs{})
			w := New(c, c)
			ctx := context.Background()
			key := client.ObjectKey{Namespace: "team-a", Name: "fiberd-web"}
			// The template is built from the grant without a device, so
			// only the strategy can differ between the two applies.
			tmpl := testTemplate(t, testGrant())

			if err := w.Apply(ctx, tc.first, tmpl, workload.Observation{}); err != nil {
				t.Fatalf("first Apply: %v", err)
			}
			firstD, firstSvc := &appsv1.Deployment{}, &corev1.Service{}
			if err := c.Get(ctx, key, firstD); err != nil {
				t.Fatalf("get Deployment: %v", err)
			}
			if err := c.Get(ctx, key, firstSvc); err != nil {
				t.Fatalf("get Service: %v", err)
			}

			if err := w.Apply(ctx, tc.second, tmpl, workload.Observation{ServiceFound: true}); err != nil {
				t.Fatalf("second Apply: %v", err)
			}
			d, svc := &appsv1.Deployment{}, &corev1.Service{}
			if err := c.Get(ctx, key, d); err != nil {
				t.Fatalf("get Deployment: %v", err)
			}
			if err := c.Get(ctx, key, svc); err != nil {
				t.Fatalf("get Service: %v", err)
			}

			if diff := cmp.Diff(tc.wantStrategy, d.Spec.Strategy); diff != "" {
				t.Errorf("strategy mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(firstSvc, svc, ignoreVersion); diff != "" {
				t.Errorf("the second apply changed the Service (-first +second):\n%s", diff)
			}
			if tc.wantSame {
				if diff := cmp.Diff(firstD, d, ignoreVersion); diff != "" {
					t.Errorf("the second apply changed the Deployment (-first +second):\n%s", diff)
				}
			}
		})
	}
}
