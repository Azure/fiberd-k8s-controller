//go:build envtest

package envtest_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/cmd/fiberd-k8s-controller/app"
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

	// issuerURL is what config/manager passes, the issuer Service and its
	// port in fiberd-system.
	issuerURL = "https://fiberd-k8s-controller.fiberd-system.svc:8443"

	pollInterval = 100 * time.Millisecond
	pollTimeout  = 30 * time.Second
)

// admin is a cluster-admin client for setting up and checking each case.
var admin client.Client

// probeAddr is where the manager serves /healthz and /readyz.
var probeAddr string

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

// run starts the control plane and one manager for the whole package,
// since a process can register a controller name only once.
func run(m *testing.M) int {
	ctrl.SetLogger(klog.NewKlogr())

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
	code := runWithControlPlane(m, env, cfg)
	if err := env.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stop envtest: %v\n", err)
	}
	return code
}

// runWithControlPlane runs the manager and the tests against a started
// control plane.
func runWithControlPlane(m *testing.M, env *envtest.Environment, cfg *rest.Config) int {
	var err error
	admin, err = client.New(cfg, client.Options{Scheme: newScheme()})
	if err != nil {
		fmt.Fprintf(os.Stderr, "create the admin client: %v\n", err)
		return 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done, err := startManager(ctx, env)
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
func startManager(ctx context.Context, env *envtest.Environment) (<-chan error, error) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: controllerNamespace}}
	if err := admin.Create(ctx, ns); err != nil {
		return nil, fmt.Errorf("create namespace %s: %w", controllerNamespace, err)
	}
	files, err := rbacFiles()
	if err != nil {
		return nil, err
	}
	objs, err := readObjects(files...)
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
	o, err := controllerOptions()
	if err != nil {
		return nil, err
	}
	if err := o.Validate(); err != nil {
		return nil, err
	}
	mgr, err := app.NewManager(user.Config(), o)
	if err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	return done, nil
}

// controllerOptions parses the flags the way main does, so the defaults
// are the shipped ones. The probe port is a free one on the loopback
// address, since two packages may run at once.
func controllerOptions() (*app.Options, error) {
	addr, err := freeLoopbackAddr()
	if err != nil {
		return nil, err
	}
	probeAddr = addr
	fs := flag.NewFlagSet("fiberd-k8s-controller", flag.ContinueOnError)
	o := app.NewOptions()
	o.AddFlags(fs)
	err = fs.Parse([]string{
		"--issuer-url=" + issuerURL,
		"--namespace=" + controllerNamespace,
		"--health-probe-bind-address=" + probeAddr,
	})
	return o, err
}

// freeLoopbackAddr returns a loopback address with a port nothing listens
// on right now.
func freeLoopbackAddr() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("find a free probe port: %w", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String(), nil
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

// readObjects reads every YAML document in paths. Documents that hold
// only comments are skipped.
func readObjects(paths ...string) ([]*unstructured.Unstructured, error) {
	var objs []*unstructured.Unstructured
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
		for {
			doc, err := r.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", p, err)
			}
			u := &unstructured.Unstructured{}
			if err := yaml.Unmarshal(doc, &u.Object); err != nil {
				return nil, fmt.Errorf("parse %s: %w", p, err)
			}
			if len(u.Object) == 0 {
				continue
			}
			objs = append(objs, u)
		}
	}
	return objs, nil
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

// eventually polls cond until it holds. A NotFound error from cond counts
// as not yet, so a cond can Get an object that is still being created.
func eventually(t *testing.T, what string, cond func(ctx context.Context) (bool, error)) {
	t.Helper()
	err := wait.PollUntilContextTimeout(t.Context(), pollInterval, pollTimeout, true, func(ctx context.Context) (bool, error) {
		ok, err := cond(ctx)
		return ok, client.IgnoreNotFound(err)
	})
	if err != nil {
		t.Fatalf("waiting for %s: %v", what, err)
	}
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
	objs, err := readObjects(filepath.Join(repoRoot, "config", "tenant", "controller_role_binding.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range objs {
		obj.SetNamespace(ns.Name)
		if err := admin.Create(ctx, obj); err != nil {
			t.Fatalf("create %s %s in %s: %v", obj.GetKind(), obj.GetName(), ns.Name, err)
		}
	}
	eventually(t, "the tenant RoleBinding to take effect", func(ctx context.Context) (bool, error) {
		sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
			User: controllerUser,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: ns.Name,
				Verb:      "patch",
				Group:     "apps",
				Resource:  "deployments",
			},
		}}
		if err := admin.Create(ctx, sar); err != nil {
			return false, err
		}
		return sar.Status.Allowed, nil
	})
	return ns.Name
}
