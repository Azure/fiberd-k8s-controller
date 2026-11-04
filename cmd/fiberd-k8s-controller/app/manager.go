package app

import (
	"errors"
	"fmt"
	"net/http"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/clock"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/pkg/controller"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload/deployment"
)

// Leader election takes a Lease in the controller's namespace and records
// Events next to it. controller-runtime v0.25.2 still writes those through
// the core v1 Events API, unlike the reconciler, which uses events.k8s.io.
// The generated Role lands in fiberd-system, the namespace config/manager
// deploys to.
//
// +kubebuilder:rbac:groups=coordination.k8s.io,namespace=fiberd-system,resources=leases,verbs=get;create;update
// +kubebuilder:rbac:groups="",namespace=fiberd-system,resources=events,verbs=create;patch

const (
	// LeaderElectionID names the Lease the replicas compete for.
	LeaderElectionID = "fiberd-k8s-controller.fiberd.io"

	// EventSource is the component name on the Events the reconciler emits.
	EventSource = "fiberd-k8s-controller"
)

// NewManager returns a manager that runs the CapacityGrant reconciler with
// o, which the caller has validated. It does not start anything.
//
// Every replica syncs its informers, so a standby is warm when it takes
// over, and /readyz holds until that sync is done. Only the leader
// reconciles. The cache drops managed fields, which nothing reads.
// Metrics are off.
func NewManager(cfg *rest.Config, o *Options) (ctrl.Manager, error) {
	scheme, err := newScheme()
	if err != nil {
		return nil, err
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                        scheme,
		LeaderElection:                o.LeaderElect,
		LeaderElectionID:              LeaderElectionID,
		LeaderElectionNamespace:       o.Namespace,
		LeaderElectionReleaseOnCancel: true,
		HealthProbeBindAddress:        o.ProbeAddr,
		Metrics:                       metricsserver.Options{BindAddress: "0"},
		Cache:                         cache.Options{DefaultTransform: cache.TransformStripManagedFields()},
		Controller:                    config.Controller{EnableWarmup: ptr.To(true)},
	})
	if err != nil {
		return nil, fmt.Errorf("create the manager: %w", err)
	}

	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return nil, fmt.Errorf("add the healthz check: %w", err)
	}
	if err := mgr.AddReadyzCheck("cache-sync", cacheSynced(mgr)); err != nil {
		return nil, fmt.Errorf("add the readyz check: %w", err)
	}

	r := &controller.CapacityGrantReconciler{
		Client:   mgr.GetClient(),
		Workload: deployment.New(mgr.GetClient()),
		Template: o.Template,
		Recorder: mgr.GetEventRecorder(EventSource),
		Clock:    clock.RealClock{},
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("set up the CapacityGrant reconciler: %w", err)
	}

	// The issuer's HTTPS listener belongs here, as a Runnable that needs
	// no leadership, so every ready replica serves discovery, the key set
	// and grants. Its readyz check would join the one above.

	return mgr, nil
}

// newScheme holds the built-in types and the fiberd.io types.
func newScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("add the built-in types to the scheme: %w", err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("add the fiberd.io types to the scheme: %w", err)
	}
	return s, nil
}

// cacheSynced is ready once the informers of every kind the reconciler
// watches have synced. WaitForCacheSync alone covers only the informers that
// exist, and a probe can run before the controller has asked for its own. So
// the check gets each informer first, which creates any that is missing.
// Both steps run under the probe request's context, so a probe that times
// out fails instead of hanging.
func cacheSynced(mgr ctrl.Manager) healthz.Checker {
	watched := []client.Object{&v1alpha1.CapacityGrant{}, &appsv1.Deployment{}, &corev1.Service{}}
	return func(req *http.Request) error {
		c := mgr.GetCache()
		for _, obj := range watched {
			if _, err := c.GetInformer(req.Context(), obj, cache.BlockUntilSynced(false)); err != nil {
				return fmt.Errorf("get the %T informer: %w", obj, err)
			}
		}
		if !c.WaitForCacheSync(req.Context()) {
			return errors.New("the informer caches have not synced")
		}
		return nil
	}
}
