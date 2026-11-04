//go:build envtest

package envtest_test

import (
	"context"
	"crypto/x509"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/cmd/fiberd-k8s-controller/app"
	"github.com/Azure/fiberd-k8s-controller/pkg/issuer"
	"github.com/Azure/fiberd-k8s-controller/pkg/testcert"
	"github.com/Azure/fiberd-k8s-controller/test/envtest/internal/testutil"
)

const (
	// repoRoot is the repository root seen from this package.
	repoRoot = "../.."

	// controllerNamespace is where config/manager deploys the controller
	// and where the generated Role lives.
	controllerNamespace = "fiberd-system"

	// controllerServiceAccount is the controller's ServiceAccount in
	// config/rbac/controller.
	controllerServiceAccount = "fiberd-k8s-controller"

	// controllerUser is the user name the API server gives
	// controllerServiceAccount, so the hand-written bindings apply to the
	// envtest user as written.
	controllerUser = "system:serviceaccount:" + controllerNamespace + ":" + controllerServiceAccount

	// issuerHost is the issuer Service in fiberd-system, the name its
	// serving certificate carries.
	issuerHost = "fiberd-k8s-controller.fiberd-system.svc"

	// issuerURL is what config/manager passes, the issuer Service and its
	// port.
	issuerURL = "https://" + issuerHost + ":8443"
)

// admin is a cluster-admin client for setting up and checking each case.
var admin client.Client

// probeAddr is where the manager serves /healthz and /readyz.
var probeAddr string

// issuerAddr is where the manager serves discovery and the JWKS.
var issuerAddr string

// issuerRoots trusts only the issuer's self-signed serving certificate, so
// a client verifies the listener as a grant Pod would.
var issuerRoots *x509.CertPool

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

// run starts the control plane and one manager for the whole package,
// since a process can register a controller name only once.
func run(m *testing.M) int {
	ctrl.SetLogger(klog.NewKlogr())

	// TestMain has no t.TempDir, so the directory is removed here, after
	// the manager has stopped.
	tlsDir, err := os.MkdirTemp("", "fiberd-envtest-tls-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create the serving pair directory: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(tlsDir) }()

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(repoRoot, "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	// A real cluster enables OwnerReferencesPermissionEnforcement, and the
	// envtest API server does not. With it on, setting blockOwnerDeletion
	// on a grant's objects needs update on capacitygrants/finalizers, so a
	// Role without that rule fails here as it would in production.
	env.ControlPlane.GetAPIServer().Configure().Append("enable-admission-plugins", "OwnerReferencesPermissionEnforcement")
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "start envtest, run make test-envtest to set KUBEBUILDER_ASSETS: %v\n", err)
		return 1
	}
	code := runWithControlPlane(m, env, cfg, tlsDir)
	if err := env.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stop envtest: %v\n", err)
	}
	return code
}

// runWithControlPlane runs the manager and the tests against a started
// control plane. tlsDir holds the issuer's serving pair.
func runWithControlPlane(m *testing.M, env *envtest.Environment, cfg *rest.Config, tlsDir string) int {
	var err error
	admin, err = client.New(cfg, client.Options{Scheme: newScheme()})
	if err != nil {
		fmt.Fprintf(os.Stderr, "create the admin client: %v\n", err)
		return 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done, err := startManager(ctx, env, tlsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start the manager: %v\n", err)
		return 1
	}

	code := m.Run()
	cancel()
	if err := <-done; err != nil {
		fmt.Fprintf(os.Stderr, "the manager stopped with an error: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// startManager installs config/rbac, then runs app.NewManager as the
// controller's ServiceAccount. That user holds exactly the generated roles
// through the hand-written bindings.
func startManager(ctx context.Context, env *envtest.Environment, tlsDir string) (<-chan error, error) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: controllerNamespace}}
	if err := admin.Create(ctx, ns); err != nil {
		return nil, fmt.Errorf("create namespace %s: %w", controllerNamespace, err)
	}
	files, err := rbacFiles()
	if err != nil {
		return nil, err
	}
	objs, err := testutil.ReadObjects(files...)
	if err != nil {
		return nil, err
	}
	for _, obj := range objs {
		if err := admin.Create(ctx, obj); err != nil {
			return nil, fmt.Errorf("create %s %s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}

	user, err := env.AddUser(envtest.User{
		Name:   controllerUser,
		Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + controllerNamespace},
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("add the controller user: %w", err)
	}
	// The API server authorizes from an informer cache, so the Role created
	// above can still answer Forbidden, and NewManager reads the key Secret
	// once without retrying.
	err = wait.PollUntilContextTimeout(ctx, testutil.PollInterval, testutil.PollTimeout, true, func(ctx context.Context) (bool, error) {
		for _, attrs := range []*authorizationv1.ResourceAttributes{
			{Namespace: controllerNamespace, Verb: "get", Resource: "secrets", Name: issuer.KeySecretName},
			{Namespace: controllerNamespace, Verb: "create", Resource: "secrets"},
		} {
			ok, err := allowed(ctx, attrs)
			if err != nil || !ok {
				return false, err
			}
		}
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("wait for the controller Role to take effect: %w", err)
	}
	o, err := controllerOptions(tlsDir)
	if err != nil {
		return nil, err
	}
	if err := o.Validate(); err != nil {
		return nil, err
	}
	mgr, err := app.NewManager(ctx, user.Config(), o)
	if err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	return done, nil
}

// controllerOptions parses the flags the way main does, so the defaults
// are the shipped ones. The probe and issuer ports are free ones on the
// loopback address, since two packages may run at once. The serving pair
// is self-signed for issuerHost and written to tlsDir. issuerRoots trusts
// only that pair.
func controllerOptions(tlsDir string) (*app.Options, error) {
	addrs, err := freeLoopbackAddrs(2)
	if err != nil {
		return nil, err
	}
	probeAddr, issuerAddr = addrs[0], addrs[1]
	pair, err := testcert.New(issuerHost)
	if err != nil {
		return nil, err
	}
	issuerRoots = testcert.Pool(pair)
	certFile, keyFile := filepath.Join(tlsDir, "tls.crt"), filepath.Join(tlsDir, "tls.key")
	if err := pair.Write(certFile, keyFile); err != nil {
		return nil, err
	}
	fs := flag.NewFlagSet("fiberd-k8s-controller", flag.ContinueOnError)
	o := app.NewOptions()
	o.AddFlags(fs)
	if err := fs.Parse([]string{
		"--issuer-url=" + issuerURL,
		"--namespace=" + controllerNamespace,
		"--health-probe-bind-address=" + probeAddr,
		"--issuer-bind-address=" + issuerAddr,
		"--tls-cert-file=" + certFile,
		"--tls-private-key-file=" + keyFile,
	}); err != nil {
		return nil, err
	}
	return o, nil
}

// freeLoopbackAddrs returns n loopback addresses with ports nothing listens
// on right now. Every listener stays open until all addresses are read, so
// no port is handed out twice.
func freeLoopbackAddrs(n int) ([]string, error) {
	listeners := make([]net.Listener, 0, n)
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	for range n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("find a free loopback port: %w", err)
		}
		listeners = append(listeners, l)
	}
	addrs := make([]string, 0, n)
	for _, l := range listeners {
		addrs = append(addrs, l.Addr().String())
	}
	return addrs, nil
}

// rbacFiles lists every manifest under config/rbac/controller and
// config/rbac/tenant except the kustomizations.
func rbacFiles() ([]string, error) {
	var files []string
	for _, dir := range []string{"controller", "tenant"} {
		matches, err := filepath.Glob(filepath.Join(repoRoot, "config", "rbac", dir, "*.yaml"))
		if err != nil {
			return nil, err
		}
		matches = slices.DeleteFunc(matches, func(p string) bool { return filepath.Base(p) == "kustomization.yaml" })
		if len(matches) == 0 {
			return nil, fmt.Errorf("config/rbac/%s holds no manifests, run make manifests", dir)
		}
		files = append(files, matches...)
	}
	return files, nil
}

// newScheme holds the built-in types and the fiberd.io types.
func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		panic(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		panic(err)
	}
	return s
}

// tenantNamespace creates a namespace for one case. When bound is true it
// also creates the controller's RoleBinding from config/tenant and waits
// until the API server authorizes the controller there, so the first pass
// never sees a stale Forbidden.
func tenantNamespace(t *testing.T, bound bool) string {
	t.Helper()
	ctx := t.Context()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "tenant-"}}
	if err := admin.Create(ctx, ns); err != nil {
		t.Fatalf("create a tenant namespace: %v", err)
	}
	if !bound {
		return ns.Name
	}
	objs, err := testutil.ReadObjects(filepath.Join(repoRoot, "config", "tenant", "controller_role_binding.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range objs {
		obj.SetNamespace(ns.Name)
		if err := admin.Create(ctx, obj); err != nil {
			t.Fatalf("create %s %s in %s: %v", obj.GetKind(), obj.GetName(), ns.Name, err)
		}
	}
	testutil.Eventually(t, "the tenant RoleBinding to take effect", func(ctx context.Context) (bool, error) {
		return allowed(ctx, &authorizationv1.ResourceAttributes{
			Namespace: ns.Name,
			Verb:      "patch",
			Group:     "apps",
			Resource:  "deployments",
		})
	})
	return ns.Name
}

// allowed reports whether the API server lets controllerUser act on attrs
// right now.
func allowed(ctx context.Context, attrs *authorizationv1.ResourceAttributes) (bool, error) {
	sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User:               controllerUser,
		ResourceAttributes: attrs,
	}}
	if err := admin.Create(ctx, sar); err != nil {
		return false, err
	}
	return sar.Status.Allowed, nil
}
