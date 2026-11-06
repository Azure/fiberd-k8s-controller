package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/clock"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/Azure/fiberd/pkg/grant"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/pkg/controller"
	"github.com/Azure/fiberd-k8s-controller/pkg/issuer"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload/deployment"
)

// Leader election takes a Lease in the controller's namespace and records
// Events next to it. controller-runtime v0.25.2 still writes those through
// the core v1 Events API, unlike the reconciler, which uses events.k8s.io.
// The generated Role lands in fiberd-system, the namespace config/manager
// deploys to.
//
// The grant signing keys live in one Secret there. RBAC resource names do
// not restrict create, so create is granted on secrets and get only on that
// Secret. A marker cannot name a constant, so the name repeats
// issuer.KeySecretName and a test pins the two together.
//
// The grant route reviews a Pod's token and walks its owners. Those rules
// sit here because the Makefile generates the controller's roles from this
// package and pkg/controller. A marker in pkg/workload would land on the
// tenant role, and one in pkg/issuer would land nowhere.
//
// +kubebuilder:rbac:groups=coordination.k8s.io,namespace=fiberd-system,resources=leases,verbs=get;create;update
// +kubebuilder:rbac:groups="",namespace=fiberd-system,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",namespace=fiberd-system,resources=secrets,verbs=create
// +kubebuilder:rbac:groups="",namespace=fiberd-system,resources=secrets,resourceNames=grant-issuer-key,verbs=get
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;watch

const (
	// LeaderElectionID names the Lease the replicas compete for.
	LeaderElectionID = "fiberd-k8s-controller.fiberd.io"

	// EventSource is the component name on the Events the reconciler emits.
	EventSource = "fiberd-k8s-controller"

	// A standby takes over electionLeaseDuration after the leader last
	// renewed. A leader that cannot renew for electionRenewDeadline stops,
	// so it is gone before a standby may take over. electionRetryPeriod is
	// how often each replica tries.
	electionLeaseDuration = 15 * time.Second
	electionRenewDeadline = 10 * time.Second
	electionRetryPeriod   = 2 * time.Second

	// startupTimeout bounds the startup read of the grant key ring. A
	// stalled API server must not hold a Pod with no probes serving until
	// the kubelet kills it.
	startupTimeout = 30 * time.Second
)

// NewManager returns a manager that runs the CapacityGrant reconciler and
// the issuer's HTTPS listener with o, which the caller has validated. It
// does not start anything. It loads the grant key ring and reads the
// serving certificate before it returns, so a missing Secret permission or
// certificate fails startup with a clear error.
//
// Every replica syncs its informers, so a standby is warm when it takes
// over, and /readyz holds until that sync is done. Only the leader
// reconciles. Metrics are off.
func NewManager(ctx context.Context, cfg *rest.Config, o *Options) (ctrl.Manager, error) {
	scheme, err := newScheme()
	if err != nil {
		return nil, err
	}
	cacheOpts, err := cacheOptions()
	if err != nil {
		return nil, err
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                        scheme,
		LeaderElection:                o.LeaderElect,
		LeaderElectionID:              LeaderElectionID,
		LeaderElectionNamespace:       o.Namespace,
		LeaderElectionReleaseOnCancel: true,
		LeaseDuration:                 ptr.To(electionLeaseDuration),
		RenewDeadline:                 ptr.To(electionRenewDeadline),
		RetryPeriod:                   ptr.To(electionRetryPeriod),
		HealthProbeBindAddress:        o.ProbeAddr,
		Metrics:                       metricsserver.Options{BindAddress: "0"},
		Cache:                         cacheOpts,
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

	clk := clock.RealClock{}
	workload := deployment.New(mgr.GetClient())
	r := &controller.CapacityGrantReconciler{
		Client:   mgr.GetClient(),
		Workload: workload,
		Template: o.Template,
		Recorder: mgr.GetEventRecorder(EventSource),
		Clock:    clk,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("set up the CapacityGrant reconciler: %w", err)
	}

	// The manager's client reads from informers, which can miss a Secret
	// another replica created a moment ago. The ring is read once here, so
	// a direct client costs one request.
	c, err := client.New(cfg, client.Options{Scheme: scheme, HTTPClient: mgr.GetHTTPClient(), Mapper: mgr.GetRESTMapper()})
	if err != nil {
		return nil, fmt.Errorf("create the issuer key client: %w", err)
	}
	loadCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	ring, err := issuer.LoadOrCreate(loadCtx, c, o.Namespace)
	if err != nil {
		return nil, fmt.Errorf("load the grant key ring: %w", err)
	}
	cw, err := certwatcher.New(o.TLSCertFile, o.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("read the serving certificate: %w", err)
	}
	handler := issuer.NewHandler(issuer.Config{
		Issuer:         &grant.Issuer{Ring: ring, URL: o.Template.IssuerURL, Now: clk.Now},
		Reviewer:       &issuer.APITokenReviewer{Client: mgr.GetClient(), IssuerURL: o.Template.IssuerURL},
		Cache:          mgr.GetCache(),
		Live:           mgr.GetAPIReader(),
		Workload:       workload,
		Limits:         o.Limits,
		ServiceAccount: o.Template.ServiceAccount,
		Lease:          o.Template.Lease,
		Clock:          clk,
		Log:            ctrl.Log.WithName("issuer"),
	})
	srv, err := issuer.NewServer(ctx, o.IssuerAddr, handler.Routes(), cw.GetCertificate)
	if err != nil {
		return nil, fmt.Errorf("create the issuer server: %w", err)
	}
	if err := mgr.Add(cw); err != nil {
		return nil, fmt.Errorf("add the certificate watcher: %w", err)
	}
	if err := mgr.Add(srv); err != nil {
		return nil, fmt.Errorf("add the issuer server: %w", err)
	}
	if err := mgr.AddReadyzCheck("issuer", srv.Ready); err != nil {
		return nil, fmt.Errorf("add the issuer readyz check: %w", err)
	}

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

// cacheOptions drops managed fields, which nothing reads, and caches only
// the Pods and ReplicaSets of grants, so the rest of a busy cluster costs
// this process no memory. The grant route reads an object the filter hides
// from the API server.
func cacheOptions() (cache.Options, error) {
	ofGrant, err := labels.NewRequirement(v1alpha1.LabelGrant, selection.Exists, nil)
	if err != nil {
		return cache.Options{}, fmt.Errorf("select the objects of a grant: %w", err)
	}
	selector := labels.NewSelector().Add(*ofGrant)
	return cache.Options{
		DefaultTransform: cache.TransformStripManagedFields(),
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Pod{}:        {Label: selector},
			&appsv1.ReplicaSet{}: {Label: selector},
		},
	}, nil
}

// cacheSynced is ready once the informers of every kind the reconciler
// watches or the grant route reads have synced, so a replica takes grant
// traffic only with a warm cache. WaitForCacheSync alone covers only the
// informers that exist, and a probe can run before anyone has asked for
// them. So the check gets each informer first, which creates any that is
// missing. Both steps run under the probe request's context, so a probe that
// times out fails instead of hanging.
func cacheSynced(mgr ctrl.Manager) healthz.Checker {
	watched := []client.Object{
		&v1alpha1.CapacityGrant{}, &appsv1.Deployment{}, &corev1.Service{},
		&corev1.Pod{}, &appsv1.ReplicaSet{},
	}
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
