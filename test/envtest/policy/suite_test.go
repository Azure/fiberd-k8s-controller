//go:build envtest

package policy_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/pkg/testutil"
)

// repoRoot is the repository root seen from this package.
const repoRoot = "../../.."

var (
	crdDir                   = filepath.Join(repoRoot, "config", "crd", "bases")
	grantsPolicyFile         = filepath.Join(repoRoot, "config", "policy", "grants_policy.yaml")
	grantsBindingFile        = filepath.Join(repoRoot, "config", "policy", "grants_policy_binding.yaml")
	grantPoliciesPolicyFile  = filepath.Join(repoRoot, "config", "policy", "grantpolicies_policy.yaml")
	grantPoliciesBindingFile = filepath.Join(repoRoot, "config", "policy", "grantpolicies_policy_binding.yaml")
	grantPolicySampleFile    = filepath.Join(repoRoot, "config", "samples", "fiberd_v1alpha1_grantpolicy.yaml")
	grantSampleFile          = filepath.Join(repoRoot, "config", "samples", "fiberd_v1alpha1_capacitygrant.yaml")
	authorRoleFile           = filepath.Join(repoRoot, "config", "rbac", "tenant", "grant_author_role.yaml")
	authorBindingFile        = filepath.Join(repoRoot, "config", "tenant", "author_role_binding.yaml")

	caRequestsPolicyFile      = filepath.Join(repoRoot, "config", "certs", "ca_requests_policy.yaml")
	caRequestsBindingFile     = filepath.Join(repoRoot, "config", "certs", "ca_requests_policy_binding.yaml")
	caCertificatesPolicyFile  = filepath.Join(repoRoot, "config", "certs", "ca_certificates_policy.yaml")
	caCertificatesBindingFile = filepath.Join(repoRoot, "config", "certs", "ca_certificates_policy_binding.yaml")
	caPodsPolicyFile          = filepath.Join(repoRoot, "config", "certs", "ca_pods_policy.yaml")
	caPodsBindingFile         = filepath.Join(repoRoot, "config", "certs", "ca_pods_policy_binding.yaml")
	caCertificateFile         = filepath.Join(repoRoot, "config", "certs", "ca_certificate.yaml")
	servingCertificateFile    = filepath.Join(repoRoot, "config", "certmanager", "certificate.yaml")

	// certManagerCRDs are the CertificateRequest and Certificate CRDs the
	// fiberd-ca policies match.
	certManagerCRDs = filepath.Join("testdata", "cert-manager-crds.yaml")

	// policyFiles are what config/policy and the fiberd-ca policies of
	// config/certs install, in an order where each object finds what it
	// names. GrantPolicy default is not among them, because
	// baselineGrantPolicy is the one way the suite installs it.
	policyFiles = []string{
		grantsPolicyFile, grantsBindingFile, grantPoliciesPolicyFile, grantPoliciesBindingFile,
		caRequestsPolicyFile, caRequestsBindingFile, caCertificatesPolicyFile, caCertificatesBindingFile, caPodsPolicyFile, caPodsBindingFile,
	}
)

const (
	grantsPolicy        = "fiberd-grants"
	grantPoliciesPolicy = "fiberd-grantpolicies"
	caRequestsPolicy    = "fiberd-ca-requests"
	caCertsPolicy       = "fiberd-ca-certificates"
	caPodsPolicy        = "fiberd-ca-pods"

	// controllerNamespace is where the controller and its serving
	// Certificate run.
	controllerNamespace = "fiberd-system"

	// grantPolicyName is the one GrantPolicy the fiberd-grants binding reads.
	grantPolicyName = "default"

	// baselineRuntime is the runtime the CapacityGrant sample asks for. The
	// suite adds it to the ceiling and labels its namespaces for it.
	baselineRuntime = v1alpha1.RuntimeProc

	// The messages below are cut from config/policy, so a reworded rule
	// fails here rather than passing on another rule's refusal.
	envNameMessage     = "must match ^FIBERD_TENANT_[A-Z0-9_]+$"
	volumeMessage      = "is not an emptyDir, configMap"
	imageListMessage   = "is not under a repository listed"
	deleteGuardMessage = "update it instead of deleting it"
	uncleanMessage     = "is not a clean absolute path"
	outsideMessage     = "must be at or under /config, /data, /secrets"

	// registryListMessage is how the spec.registries rule ends its refusal.
	registryListMessage = imageListMessage + " in GrantPolicy default spec.registries"

	// runtimeListMessage is how the ceiling rule ends its refusal.
	runtimeListMessage = "is not listed in GrantPolicy default spec.runtimes"

	// runtimeLabelMessage is how the exception rule starts naming the label.
	runtimeLabelMessage = "needs the label"

	// privilegedFieldMessage is cut from the CRD rule on podTemplate. The API
	// server runs that rule before any admission policy, in every namespace.
	privilegedFieldMessage = "podTemplate.spec.containers[0].securityContext.privileged is not allowed"

	// paramNotFoundMessage is the stable part of the API server's refusal
	// when a binding's parameterNotFoundAction is Deny and the parameter is
	// missing. The API server treats that as a configuration error with no
	// reason, so it comes back as Invalid, not Forbidden.
	paramNotFoundMessage = "no params found"
)

var (
	// admin is a cluster-admin client for every case.
	admin client.Client

	// restConfig is admin's config, from which a case impersonates
	// another user.
	restConfig *rest.Config
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

// run starts one control plane for the package. The API server's own
// ValidatingAdmissionPolicy plugin does the admitting, so no manager runs.
func run(m *testing.M) int {
	logf.SetLogger(klog.NewKlogr())

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{crdDir, certManagerCRDs},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "start envtest, run make test-envtest to set KUBEBUILDER_ASSETS: %v\n", err)
		return 1
	}
	code := 1
	if err := setUp(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "set up the policies: %v\n", err)
	} else {
		code = m.Run()
	}
	if err := env.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stop envtest: %v\n", err)
	}
	return code
}

// setUp installs the policies and returns once the API server enforces
// them, so the first case never races the policy informers.
func setUp(cfg *rest.Config) error {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return err
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		return err
	}
	var err error
	if admin, err = client.New(cfg, client.Options{Scheme: s}); err != nil {
		return fmt.Errorf("create the admin client: %w", err)
	}
	restConfig = cfg

	ctx := context.Background()
	// The controller's serving Certificate is the one server auth
	// Certificate fiberd-ca-certificates admits, and only here.
	if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: controllerNamespace}}); err != nil {
		return fmt.Errorf("create namespace %s: %w", controllerNamespace, err)
	}
	objs, err := testutil.ReadObjects(policyFiles...)
	if err != nil {
		return err
	}
	baseline, err := baselineGrantPolicy()
	if err != nil {
		return err
	}
	for _, obj := range append(objs, baseline) {
		if err := admin.Create(ctx, obj); err != nil {
			return fmt.Errorf("create %s %s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}
	return waitEnforced(ctx)
}

// waitEnforced waits until each policy refuses with one of its own rules.
// Before the GrantPolicy informer syncs, fiberd-grants refuses every grant
// with the missing parameter message, so the probe asks for the volume
// rule's message, which needs the parameter loaded. The policy's
// typeChecking status is written by kube-controller-manager, which envtest
// does not run, so the cases that follow are the type check.
func waitEnforced(ctx context.Context) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "policy-probe-", Labels: runtimeLabels(baselineRuntime)}}
	if err := admin.Create(ctx, ns); err != nil {
		return fmt.Errorf("create the probe namespace: %w", err)
	}
	probe, err := testutil.ReadObject(grantSampleFile)
	if err != nil {
		return err
	}
	placeSample(probe, ns.Name)
	if err := unstructured.SetNestedSlice(probe.Object, []any{imageVolume()}, slices.Concat(testutil.PodSpecPath, []string{"volumes"})...); err != nil {
		return err
	}
	var last error
	err = wait.PollUntilContextTimeout(ctx, testutil.PollInterval, testutil.PollTimeout, true, func(ctx context.Context) (bool, error) {
		last = dryRunCreate(ctx, probe)
		return apierrors.IsForbidden(last) && strings.Contains(last.Error(), grantsPolicy) && strings.Contains(last.Error(), volumeMessage), nil
	})
	if err != nil {
		return fmt.Errorf("waiting for %s to refuse an image volume, the last answer was %w: %w", grantsPolicy, last, err)
	}
	err = wait.PollUntilContextTimeout(ctx, testutil.PollInterval, testutil.PollTimeout, true, func(ctx context.Context) (bool, error) {
		last = admin.Delete(ctx, grantPolicyRef(), client.DryRunAll)
		return apierrors.IsForbidden(last) && strings.Contains(last.Error(), grantPoliciesPolicy) && strings.Contains(last.Error(), deleteGuardMessage), nil
	})
	if err != nil {
		return fmt.Errorf("waiting for %s to refuse deleting GrantPolicy %s, the last answer was %w: %w", grantPoliciesPolicy, grantPolicyName, last, err)
	}
	// The fiberd-ca policies have no parameter, so any of their refusals
	// shows the policy loaded. admin is neither ServiceAccount that may ask.
	probes := []struct {
		policy, substr string
		obj            *unstructured.Unstructured
	}{
		{policy: caRequestsPolicy, substr: requesterMessage, obj: certificateRequest(ns.Name, clientRequestSpec())},
		{policy: caCertsPolicy, substr: fieldsMessage, obj: withSpec(certificate(ns.Name, clientCertificateSpec()), "isCA", true)},
		{policy: caPodsPolicy, substr: issuerKindMessage, obj: csiPod(ns.Name, withAttribute(clientAttributes(), attrIssuerKind, "Issuer"))},
	}
	for _, p := range probes {
		err = wait.PollUntilContextTimeout(ctx, testutil.PollInterval, testutil.PollTimeout, true, func(ctx context.Context) (bool, error) {
			last = dryRunCreate(ctx, p.obj)
			return apierrors.IsForbidden(last) && strings.Contains(last.Error(), p.policy) && strings.Contains(last.Error(), p.substr), nil
		})
		if err != nil {
			return fmt.Errorf("waiting for %s to refuse a %s, the last answer was %w: %w", p.policy, p.obj.GetKind(), last, err)
		}
	}
	return nil
}

// baselineGrantPolicy returns what the suite runs under, config/samples'
// GrantPolicy with baselineRuntime added to its ceiling. The bare sample
// refuses the CapacityGrant sample, so the bootstrap and every restore go
// through here. A case that wants the bare sample reads it by name.
func baselineGrantPolicy() (*unstructured.Unstructured, error) {
	u, err := testutil.ReadObject(grantPolicySampleFile)
	if err != nil {
		return nil, err
	}
	runtimes, _, err := unstructured.NestedStringSlice(u.Object, "spec", "runtimes")
	if err != nil {
		return nil, err
	}
	if err := unstructured.SetNestedStringSlice(u.Object, append(runtimes, string(baselineRuntime)), "spec", "runtimes"); err != nil {
		return nil, err
	}
	return u, nil
}

// runtimeLabels returns the namespace labels that add rt as an exception.
func runtimeLabels(rt v1alpha1.Runtime) map[string]string {
	return map[string]string{v1alpha1.RuntimeLabel(rt): v1alpha1.LabelRuntimeValue}
}

// needsLabel is the exception rule's whole refusal of rt in ns.
func needsLabel(rt v1alpha1.Runtime, ns string) string {
	return fmt.Sprintf("runtime %s %s %s=%s on namespace %s", rt, runtimeLabelMessage, v1alpha1.RuntimeLabel(rt), v1alpha1.LabelRuntimeValue, ns)
}

// readSample returns config/samples' CapacityGrant in ns with a generated
// name.
func readSample(t *testing.T, ns string) *unstructured.Unstructured {
	t.Helper()
	u := testutil.ReadSample(t, grantSampleFile)
	placeSample(u, ns)
	return u
}

// placeSample moves a sample grant to ns and has the API server generate
// its name, so each create owns its name.
func placeSample(u *unstructured.Unstructured, ns string) {
	u.SetNamespace(ns)
	u.SetGenerateName(u.GetName() + "-")
	u.SetName("")
}

// imageVolume is a volume source outside the policy's allowlist.
func imageVolume() map[string]any {
	return map[string]any{"name": "img", "image": map[string]any{"reference": "registry.example/data:1"}}
}

// dryRunCreate asks the API server to admit u without storing it. It
// sends a copy, since a create writes the generated name back.
func dryRunCreate(ctx context.Context, u *unstructured.Unstructured) error {
	return admin.Create(ctx, u.DeepCopy(), client.DryRunAll)
}

// grantPolicyRef names GrantPolicy default for a Get or Delete.
func grantPolicyRef() *v1alpha1.GrantPolicy {
	return &v1alpha1.GrantPolicy{ObjectMeta: metav1.ObjectMeta{Name: grantPolicyName}}
}
