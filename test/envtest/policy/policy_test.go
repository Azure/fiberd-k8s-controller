//go:build envtest

package policy_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/test/envtest/internal/testutil"
)

// testDigest is a well-formed digest, so an image case fails only on the
// rule it is about.
var testDigest = "sha256:" + strings.Repeat("0123456789abcdef", 4)

// mutation edits a sample object for one case.
type mutation func(t *testing.T, u *unstructured.Unstructured)

// createOwned creates obj and deletes it when t ends. envtest runs no
// garbage collector or namespace controller, so nothing else would.
func createOwned(t *testing.T, obj client.Object) {
	t.Helper()
	if err := admin.Create(t.Context(), obj); err != nil {
		t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
	}
	t.Cleanup(func() {
		if err := admin.Delete(testutil.CleanupContext(t), obj); client.IgnoreNotFound(err) != nil {
			t.Errorf("delete %T %s: %v", obj, obj.GetName(), err)
		}
	})
}

// namespace creates a tenant namespace for one test.
func namespace(t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "tenant-"}}
	createOwned(t, ns)
	return ns.Name
}

// restore creates the objects in path that are missing. It is how a test
// that removes a shared policy object puts it back, even from a cleanup.
func restore(t *testing.T, path string) {
	t.Helper()
	objs, err := testutil.ReadObjects(path)
	if err != nil {
		t.Errorf("read %s: %v", path, err)
		return
	}
	for _, obj := range objs {
		if err := admin.Create(testutil.CleanupContext(t), obj); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Errorf("recreate %s %s: %v", obj.GetKind(), obj.GetName(), err)
		}
	}
}

// expectDenied fails t unless err is an API error with the given reason
// and a message holding substr. Checking both means a refusal by some
// other rule fails the case instead of passing it.
func expectDenied(t *testing.T, err error, reason metav1.StatusReason, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("the request was admitted, want %s naming %q", reason, substr)
	}
	if got := apierrors.ReasonForError(err); got != reason || !strings.Contains(err.Error(), substr) {
		t.Fatalf("error %q has reason %q, want %s naming %q", err.Error(), got, reason, substr)
	}
}

// accepted holds once the API server admits u. A timeout reports the last
// refusal.
func accepted(u *unstructured.Unstructured) func(ctx context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		err := dryRunCreate(ctx, u)
		return err == nil, testutil.NotYet(err)
	}
}

// refused holds once the API server refuses u with reason and a message
// naming substr. A timeout reports the last answer.
func refused(u *unstructured.Unstructured, reason metav1.StatusReason, substr string) func(ctx context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		err := dryRunCreate(ctx, u)
		if err == nil {
			return false, testutil.NotYet(errors.New("the request was admitted"))
		}
		if apierrors.ReasonForError(err) == reason && strings.Contains(err.Error(), substr) {
			return true, nil
		}
		return false, testutil.NotYet(err)
	}
}

// guardRefusesDelete holds once fiberd-grantpolicies refuses deleting
// GrantPolicy default. It is a dry run, so a guard that is not back yet
// leaves the parameter in place.
func guardRefusesDelete(ctx context.Context) (bool, error) {
	err := admin.Delete(ctx, grantPolicyRef(), client.DryRunAll)
	if apierrors.IsForbidden(err) && strings.Contains(err.Error(), deleteGuardMessage) {
		return true, nil
	}
	return false, testutil.NotYet(err)
}

// editGrantPolicy patches GrantPolicy default and puts its spec back when t
// ends. The policy reads its parameter through an informer, so the cleanup
// waits for restored before the next test sees the shipped policy.
func editGrantPolicy(t *testing.T, edit func(*v1alpha1.GrantPolicySpec), restored func(ctx context.Context) (bool, error)) {
	t.Helper()
	gp := grantPolicyRef()
	if err := admin.Get(t.Context(), client.ObjectKeyFromObject(gp), gp); err != nil {
		t.Fatalf("get GrantPolicy %s: %v", grantPolicyName, err)
	}
	shipped := gp.Spec.DeepCopy()
	t.Cleanup(func() {
		ctx := testutil.CleanupContext(t)
		cur := grantPolicyRef()
		if err := admin.Get(ctx, client.ObjectKeyFromObject(cur), cur); err != nil {
			t.Errorf("get GrantPolicy %s to restore it: %v", grantPolicyName, err)
			return
		}
		// An update replaces the whole spec, so a list the edit added and
		// the shipped spec omits is removed.
		cur.Spec = *shipped
		if err := admin.Update(ctx, cur); err != nil {
			t.Errorf("restore GrantPolicy %s: %v", grantPolicyName, err)
			return
		}
		testutil.Eventually(t, "the shipped GrantPolicy "+grantPolicyName+" to be enforced again", restored)
	})
	base := gp.DeepCopy()
	edit(&gp.Spec)
	if err := admin.Patch(t.Context(), gp, client.MergeFrom(base)); err != nil {
		t.Fatalf("patch GrantPolicy %s: %v", grantPolicyName, err)
	}
}

func setArgs(args ...any) mutation {
	return func(t *testing.T, u *unstructured.Unstructured) { testutil.SetContainerField(t, u, "args", args) }
}

func setImage(image string) mutation {
	return func(t *testing.T, u *unstructured.Unstructured) { testutil.SetContainerField(t, u, "image", image) }
}

func setEnv(name string) mutation {
	return func(t *testing.T, u *unstructured.Unstructured) {
		testutil.SetContainerField(t, u, "env", []any{map[string]any{"name": name, "value": "x"}})
	}
}

// setTemplateKey sets a key in podTemplate.metadata, where kind is labels
// or annotations.
func setTemplateKey(kind, key string) mutation {
	return func(t *testing.T, u *unstructured.Unstructured) {
		if err := unstructured.SetNestedField(u.Object, "x", "spec", "podTemplate", "metadata", kind, key); err != nil {
			t.Fatal(err)
		}
	}
}

func setVolume(v map[string]any) mutation {
	return func(t *testing.T, u *unstructured.Unstructured) { testutil.SetPodField(t, u, "volumes", []any{v}) }
}

// setMount mounts an emptyDir at path, so the volume itself is allowed and
// only the path decides.
func setMount(path string) mutation {
	return func(t *testing.T, u *unstructured.Unstructured) {
		testutil.SetPodField(t, u, "volumes", []any{map[string]any{"name": "data", "emptyDir": map[string]any{}}})
		testutil.SetContainerField(t, u, "volumeMounts", []any{map[string]any{"name": "data", "mountPath": path}})
	}
}

func setRuntime(runtime string) mutation {
	return func(t *testing.T, u *unstructured.Unstructured) {
		if err := unstructured.SetNestedField(u.Object, runtime, "spec", "runtime"); err != nil {
			t.Fatal(err)
		}
	}
}

// setHelperImage asks for runtime hyperlight with image as its helper, so
// the CRD rules on helperImage pass and only the policy decides.
func setHelperImage(image string) mutation {
	return func(t *testing.T, u *unstructured.Unstructured) {
		setRuntime("hyperlight")(t, u)
		if err := unstructured.SetNestedField(u.Object, image, "spec", "helperImage"); err != nil {
			t.Fatal(err)
		}
	}
}

func setPrivileged(t *testing.T, u *unstructured.Unstructured) {
	testutil.SetContainerField(t, u, "securityContext", map[string]any{"privileged": true})
}

// TestPolicyRefusals runs one or more cases per fiberd-grants rule, beside
// the nearest objects the rule must admit. reason is empty for a case the
// policy admits.
func TestPolicyRefusals(t *testing.T) {
	ns := namespace(t)
	cases := []struct {
		name   string
		mutate mutation
		reason metav1.StatusReason
		substr string
	}{
		{
			name:   "refuses an arg that is not flag-shaped",
			mutate: setArgs("verbose"),
			reason: metav1.StatusReasonInvalid,
			substr: "arg verbose is not one flag such as -delta-registry=example.com",
		},
		{
			name:   "refuses an empty arg",
			mutate: setArgs(""),
			reason: metav1.StatusReasonInvalid,
			substr: "is not one flag such as -delta-registry=example.com",
		},
		{
			name:   "refuses a controller flag with a value",
			mutate: setArgs("-issuer=https://evil.example"),
			reason: metav1.StatusReasonForbidden,
			substr: "arg -issuer=https://evil.example names a flag a tenant may not set",
		},
		{
			name:   "refuses a controller flag in the two-dash form",
			mutate: setArgs("--insecure-plaintext"),
			reason: metav1.StatusReasonForbidden,
			substr: "arg --insecure-plaintext names a flag a tenant may not set",
		},
		{
			name:   "refuses all-caps",
			mutate: setArgs("-all-caps"),
			reason: metav1.StatusReasonForbidden,
			substr: "arg -all-caps names a flag a tenant may not set",
		},
		{
			name:   "refuses the test-hooks flag",
			mutate: setArgs("-admin-unsafe"),
			reason: metav1.StatusReasonForbidden,
			substr: "arg -admin-unsafe names a flag a tenant may not set",
		},
		{
			name:   "refuses an operator flag",
			mutate: setArgs("-userns-pool=0:65536"),
			reason: metav1.StatusReasonForbidden,
			substr: "arg -userns-pool=0:65536 names a flag a tenant may not set",
		},
		{
			name:   "refuses the zygote registry flag, which spec.zygote sets",
			mutate: setArgs("-registry=example.com/zygotes"),
			reason: metav1.StatusReasonForbidden,
			substr: "arg -registry=example.com/zygotes names a flag a tenant may not set",
		},
		{
			name:   "refuses the seal key flag, which spec.zygote sets",
			mutate: setArgs("-delta-seal-key=/k"),
			reason: metav1.StatusReasonForbidden,
			substr: "arg -delta-seal-key=/k names a flag a tenant may not set",
		},
		{
			name:   "admits a tenant flag with its value",
			mutate: setArgs("-delta-registry=example.com/deltas"),
		},
		{
			name:   "refuses a bare flag that takes a value",
			mutate: setArgs("-delta-registry"),
			reason: metav1.StatusReasonInvalid,
			substr: "arg -delta-registry must carry its value as -name=value",
		},
		{
			name:   "admits a bare boolean flag",
			mutate: setArgs("-gvisor-templates-on-disk"),
		},
		{
			name:   "refuses an image without a digest",
			mutate: setImage("ghcr.io/azure/fiberd:0.4.0"),
			reason: metav1.StatusReasonInvalid,
			substr: "the fiberd container image must carry a digest",
		},
		{
			name:   "refuses an image from an unlisted repository",
			mutate: setImage("registry.example/fiberd@" + testDigest),
			reason: metav1.StatusReasonForbidden,
			substr: "image registry.example/fiberd@" + testDigest + " " + imageListMessage,
		},
		{
			name:   "refuses an image whose repository only starts with a listed one",
			mutate: setImage("ghcr.io/azure/fiberd-evil@" + testDigest),
			reason: metav1.StatusReasonForbidden,
			substr: "image ghcr.io/azure/fiberd-evil@" + testDigest + " " + imageListMessage,
		},
		{
			name:   "admits a listed image by digest without a tag",
			mutate: setImage("ghcr.io/azure/fiberd@" + testDigest),
		},
		{
			name:   "refuses the trust root env variable",
			mutate: setEnv("SSL_CERT_FILE"),
			reason: metav1.StatusReasonForbidden,
			substr: "env SSL_CERT_FILE is not a variable a tenant may set",
		},
		{
			name:   "refuses a Pod identity env variable",
			mutate: setEnv("POD_NAME"),
			reason: metav1.StatusReasonForbidden,
			substr: "env POD_NAME is not a variable a tenant may set",
		},
		{
			name:   "refuses a loader env variable",
			mutate: setEnv("LD_PRELOAD"),
			reason: metav1.StatusReasonForbidden,
			substr: "env LD_PRELOAD is not a variable a tenant may set",
		},
		{
			name:   "admits a harmless env variable",
			mutate: setEnv("LOG_LEVEL"),
		},
		{
			name:   "refuses a fiberd.io label",
			mutate: setTemplateKey("labels", "fiberd.io/grant"),
			reason: metav1.StatusReasonForbidden,
			substr: "podTemplate key fiberd.io/grant is reserved",
		},
		{
			name:   "refuses a fiberd.io annotation",
			mutate: setTemplateKey("annotations", "fiberd.io/grant-hash"),
			reason: metav1.StatusReasonForbidden,
			substr: "podTemplate key fiberd.io/grant-hash is reserved",
		},
		{
			name:   "refuses an AppArmor annotation",
			mutate: setTemplateKey("annotations", "container.apparmor.security.beta.kubernetes.io/fiberd"),
			reason: metav1.StatusReasonForbidden,
			substr: "podTemplate key container.apparmor.security.beta.kubernetes.io/fiberd is reserved",
		},
		{
			name:   "refuses a Multus network annotation",
			mutate: setTemplateKey("annotations", "k8s.v1.cni.cncf.io/networks"),
			reason: metav1.StatusReasonForbidden,
			substr: "podTemplate key k8s.v1.cni.cncf.io/networks is reserved",
		},
		{
			name:   "admits a well-known label",
			mutate: setTemplateKey("labels", "app.kubernetes.io/part-of"),
		},
		{
			name:   "refuses a hostPath volume",
			mutate: setVolume(map[string]any{"name": "host", "hostPath": map[string]any{"path": "/"}}),
			reason: metav1.StatusReasonForbidden,
			substr: "volume host " + volumeMessage,
		},
		{
			name:   "refuses an image volume",
			mutate: setVolume(imageVolume()),
			reason: metav1.StatusReasonForbidden,
			substr: "volume img " + volumeMessage,
		},
		{
			name:   "admits an emptyDir volume",
			mutate: setMount("/data"),
		},
		{
			name: "refuses a projected service account token",
			mutate: setVolume(map[string]any{"name": "token", "projected": map[string]any{"sources": []any{
				map[string]any{"serviceAccountToken": map[string]any{"path": "token"}},
			}}}),
			reason: metav1.StatusReasonForbidden,
			substr: "volume token projects a service account token",
		},
		{
			name: "admits a projected configMap",
			mutate: setVolume(map[string]any{"name": "config", "projected": map[string]any{"sources": []any{
				map[string]any{"configMap": map[string]any{"name": "config"}},
			}}}),
		},
		{
			name:   "refuses a volume named like the controller's state volume",
			mutate: setVolume(map[string]any{"name": "fiberd-state", "emptyDir": map[string]any{}}),
			reason: metav1.StatusReasonForbidden,
			substr: "volume fiberd-state is mounted by the controller",
		},
		{
			name:   "refuses a volume named like the controller's TLS volume",
			mutate: setVolume(map[string]any{"name": "fiberd-tls", "emptyDir": map[string]any{}}),
			reason: metav1.StatusReasonForbidden,
			substr: "volume fiberd-tls is mounted by the controller",
		},
		{
			name:   "refuses a volume named like the controller's grant token volume",
			mutate: setVolume(map[string]any{"name": "fiberd-grant-token", "emptyDir": map[string]any{}}),
			reason: metav1.StatusReasonForbidden,
			substr: "volume fiberd-grant-token is mounted by the controller",
		},
		{
			name:   "refuses a volume named like the controller's helper volume",
			mutate: setVolume(map[string]any{"name": "fiberd-helper", "emptyDir": map[string]any{}}),
			reason: metav1.StatusReasonForbidden,
			substr: "volume fiberd-helper is mounted by the controller",
		},
		{
			name:   "refuses a volume named like the controller's seal key volume",
			mutate: setVolume(map[string]any{"name": "fiberd-seal-key", "emptyDir": map[string]any{}}),
			reason: metav1.StatusReasonForbidden,
			substr: "volume fiberd-seal-key is mounted by the controller",
		},
		{
			// No volume of that name, since the volumes list is checked first
			// and would then decide instead of the mount.
			name: "refuses a mount naming the controller's grants volume",
			mutate: func(t *testing.T, u *unstructured.Unstructured) {
				testutil.SetContainerField(t, u, "volumeMounts", []any{map[string]any{"name": "fiberd-grants", "mountPath": "/grants"}})
			},
			reason: metav1.StatusReasonForbidden,
			substr: "volume fiberd-grants is mounted by the controller",
		},
		{
			name:   "refuses a mount under the grants directory",
			mutate: setMount("/var/run/fiberd/grants"),
			reason: metav1.StatusReasonForbidden,
			substr: "mount path /var/run/fiberd/grants is at or under a directory the controller mounts",
		},
		{
			name:   "refuses a mount under the state directory",
			mutate: setMount("/var/lib/fiberd/private"),
			reason: metav1.StatusReasonForbidden,
			substr: "mount path /var/lib/fiberd/private is at or under a directory the controller mounts",
		},
		{
			name:   "refuses a mount under the /run alias",
			mutate: setMount("/run/fiberd/grants"),
			reason: metav1.StatusReasonForbidden,
			substr: "mount path /run/fiberd/grants is at or under a directory the controller mounts",
		},
		{
			name:   "refuses a mount under the secrets directory",
			mutate: setMount("/var/run/secrets/fiberd.io/grant"),
			reason: metav1.StatusReasonForbidden,
			substr: "mount path /var/run/secrets/fiberd.io/grant is at or under a directory the controller mounts",
		},
		{
			name:   "refuses a mount over the system trust roots",
			mutate: setMount("/etc/ssl/certs"),
			reason: metav1.StatusReasonForbidden,
			substr: "mount path /etc/ssl/certs would replace the trust roots or the resolver files",
		},
		{
			name:   "refuses a mount above the system trust roots",
			mutate: setMount("/etc"),
			reason: metav1.StatusReasonForbidden,
			substr: "mount path /etc would replace the trust roots or the resolver files",
		},
		{
			name:   "refuses a mount at the resolver file",
			mutate: setMount("/etc/resolv.conf"),
			reason: metav1.StatusReasonForbidden,
			substr: "mount path /etc/resolv.conf would replace the trust roots or the resolver files",
		},
		{
			name:   "refuses a mount at the root",
			mutate: setMount("/"),
			reason: metav1.StatusReasonForbidden,
			substr: "mount path / would replace the trust roots or the resolver files",
		},
		{
			name:   "admits a mount beside the trust roots",
			mutate: setMount("/etc/fiberd"),
		},
		{
			name:   "admits a mount beside an owned directory",
			mutate: setMount("/var/run/fiberdx"),
		},
		{
			name:   "refuses a mount path with a double slash",
			mutate: setMount("/var/run//fiberd"),
			reason: metav1.StatusReasonInvalid,
			substr: "mount path /var/run//fiberd is not a clean absolute path",
		},
		{
			name:   "refuses a relative mount path",
			mutate: setMount("var/lib/fiberd"),
			reason: metav1.StatusReasonInvalid,
			substr: "mount path var/lib/fiberd is not a clean absolute path",
		},
		{
			name: "refuses a termination message path under the state directory",
			mutate: func(t *testing.T, u *unstructured.Unstructured) {
				testutil.SetContainerField(t, u, "terminationMessagePath", "/var/lib/fiberd/private/x")
			},
			reason: metav1.StatusReasonForbidden,
			substr: "mount path /var/lib/fiberd/private/x is at or under a directory the controller mounts",
		},
		{
			name:   "refuses a privileged container in an unlisted namespace",
			mutate: setPrivileged,
			reason: metav1.StatusReasonForbidden,
			substr: privilegedMessage,
		},
		{
			// gvisor also breaks the runtimes rule, and the privileged rule
			// comes first. TestPolicyPrivilegedNamespace reaches the
			// runtimes rule.
			name:   "refuses a privileged runtime in an unlisted namespace",
			mutate: setRuntime("gvisor"),
			reason: metav1.StatusReasonForbidden,
			substr: privilegedMessage,
		},
		{
			name:   "admits a listed runtime",
			mutate: setRuntime("runc"),
		},
		{
			name:   "admits the sample",
			mutate: func(*testing.T, *unstructured.Unstructured) {},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := readSample(t, ns)
			tc.mutate(t, u)
			err := dryRunCreate(t.Context(), u)
			if tc.reason == "" {
				if err != nil {
					t.Fatalf("create: %v, want it admitted", err)
				}
				return
			}
			expectDenied(t, err, tc.reason, tc.substr)
		})
	}
}

// TestPolicyPrivilegedNamespace lists one namespace in privilegedNamespaces
// and checks that it, and no other, may run a privileged container, and
// that listing it does not also admit a runtime GrantPolicy default leaves
// out.
func TestPolicyPrivilegedNamespace(t *testing.T) {
	listed, unlisted := namespace(t), namespace(t)
	inListed := readSample(t, listed)
	setPrivileged(t, inListed)

	editGrantPolicy(t, func(s *v1alpha1.GrantPolicySpec) {
		s.PrivilegedNamespaces = append(s.PrivilegedNamespaces, listed)
	}, refused(inListed, metav1.StatusReasonForbidden, privilegedMessage))
	testutil.Eventually(t, "a privileged grant to be admitted in "+listed, accepted(inListed))

	cases := []struct {
		name   string
		ns     string
		mutate mutation
		substr string
	}{
		{
			name:   "refuses a privileged container in a namespace GrantPolicy default does not list",
			ns:     unlisted,
			mutate: setPrivileged,
			substr: privilegedMessage,
		},
		{
			// The listed namespace passes the privileged rule, so only the
			// runtimes rule can refuse gvisor here.
			name:   "refuses an unlisted runtime in the listed namespace",
			ns:     listed,
			mutate: setRuntime("gvisor"),
			substr: "runtime gvisor is not listed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := readSample(t, tc.ns)
			tc.mutate(t, u)
			expectDenied(t, dryRunCreate(t.Context(), u), metav1.StatusReasonForbidden, tc.substr)
		})
	}
}

// TestPolicyHelperImage lets one namespace run hyperlight and lists a
// helper repository in helperImages, so only the image rules can refuse a
// grant there.
func TestPolicyHelperImage(t *testing.T) {
	const (
		helperRepo = "ghcr.io/azure/fiberd-helper"
		// fiberdRepo is listed in the shipped images.
		fiberdRepo = "ghcr.io/azure/fiberd"
	)
	helperListMessage := imageListMessage + " in GrantPolicy default spec.helperImages"
	ns := namespace(t)
	listed := readSample(t, ns)
	setHelperImage(helperRepo+"@"+testDigest)(t, listed)

	editGrantPolicy(t, func(s *v1alpha1.GrantPolicySpec) {
		s.PrivilegedNamespaces = append(s.PrivilegedNamespaces, ns)
		s.Runtimes = append(s.Runtimes, v1alpha1.RuntimeHyperlight)
		s.HelperImages = []string{helperRepo}
	}, refused(listed, metav1.StatusReasonForbidden, "helper image "+helperRepo+"@"+testDigest+" "+helperListMessage))
	testutil.Eventually(t, "a hyperlight grant to be admitted in "+ns, accepted(listed))

	cases := []struct {
		name   string
		mutate mutation
		reason metav1.StatusReason
		substr string
	}{
		{
			name:   "refuses a helper image without a digest",
			mutate: setHelperImage(helperRepo + ":0.4.0"),
			reason: metav1.StatusReasonInvalid,
			substr: "spec.helperImage must carry a digest",
		},
		{
			name:   "refuses a helper image from an unlisted repository",
			mutate: setHelperImage("registry.example/helper@" + testDigest),
			reason: metav1.StatusReasonForbidden,
			substr: "helper image registry.example/helper@" + testDigest + " " + helperListMessage,
		},
		{
			name:   "refuses a helper image whose repository only starts with a listed one",
			mutate: setHelperImage(helperRepo + "-evil@" + testDigest),
			reason: metav1.StatusReasonForbidden,
			substr: "helper image " + helperRepo + "-evil@" + testDigest + " " + helperListMessage,
		},
		{
			name:   "refuses a helper image whose repository is listed only in images",
			mutate: setHelperImage(fiberdRepo + "@" + testDigest),
			reason: metav1.StatusReasonForbidden,
			substr: "helper image " + fiberdRepo + "@" + testDigest + " " + helperListMessage,
		},
		{
			name: "refuses a fiberd image whose repository is listed only in helperImages",
			mutate: func(t *testing.T, u *unstructured.Unstructured) {
				setHelperImage(helperRepo+"@"+testDigest)(t, u)
				setImage(helperRepo+"@"+testDigest)(t, u)
			},
			reason: metav1.StatusReasonForbidden,
			substr: "image " + helperRepo + "@" + testDigest + " " + imageListMessage + " in GrantPolicy default spec.images",
		},
		{
			name:   "admits a listed helper image with a tag and a digest",
			mutate: setHelperImage(helperRepo + ":0.4.0@" + testDigest),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := readSample(t, ns)
			tc.mutate(t, u)
			err := dryRunCreate(t.Context(), u)
			if tc.reason == "" {
				if err != nil {
					t.Fatalf("create: %v, want it admitted", err)
				}
				return
			}
			expectDenied(t, err, tc.reason, tc.substr)
		})
	}
}

// TestPolicyUnchangedSpecUpdate tightens GrantPolicy default under a stored
// grant. A metadata-only update must still pass, since the controller
// removes its finalizer that way, and a spec change must not.
func TestPolicyUnchangedSpecUpdate(t *testing.T) {
	ns := namespace(t)
	grant := readSample(t, ns)
	createOwned(t, grant)

	probe := readSample(t, ns)
	editGrantPolicy(t, func(s *v1alpha1.GrantPolicySpec) {
		s.Images = []string{"registry.invalid/none"}
	}, accepted(probe))
	testutil.Eventually(t, "the sample image to be refused under the tightened GrantPolicy", refused(probe, metav1.StatusReasonForbidden, imageListMessage))

	cases := []struct {
		name   string
		mutate mutation
		reason metav1.StatusReason
		substr string
	}{
		{
			name: "admits a label-only update",
			mutate: func(_ *testing.T, u *unstructured.Unstructured) {
				labels := u.GetLabels()
				if labels == nil {
					labels = map[string]string{}
				}
				labels["example.com/team"] = "a"
				u.SetLabels(labels)
			},
		},
		{
			name: "refuses a fibers.max change under the tightened images",
			mutate: func(t *testing.T, u *unstructured.Unstructured) {
				if err := unstructured.SetNestedField(u.Object, int64(16), "spec", "fibers", "max"); err != nil {
					t.Fatal(err)
				}
			},
			reason: metav1.StatusReasonForbidden,
			substr: imageListMessage,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(grant.GroupVersionKind())
			if err := admin.Get(t.Context(), client.ObjectKeyFromObject(grant), u); err != nil {
				t.Fatalf("get the stored grant: %v", err)
			}
			tc.mutate(t, u)
			err := admin.Update(t.Context(), u)
			if tc.reason == "" {
				if err != nil {
					t.Fatalf("update: %v, want it admitted since the spec is unchanged", err)
				}
				return
			}
			expectDenied(t, err, tc.reason, tc.substr)
		})
	}
}

// TestGrantPolicyDeleteGuard deletes GrantPolicy default as a dry run, so a
// guard that fails leaves the parameter in place for the other tests.
func TestGrantPolicyDeleteGuard(t *testing.T) {
	cases := []struct {
		name   string
		substr string
	}{
		{name: "names the refusing policy", substr: "ValidatingAdmissionPolicy '" + grantPoliciesPolicy + "'"},
		{name: "advises an update instead", substr: deleteGuardMessage},
	}
	err := admin.Delete(t.Context(), grantPolicyRef(), client.DryRunAll)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectDenied(t, err, metav1.StatusReasonForbidden, tc.substr)
		})
	}
}

// TestGrantPolicySchema checks the GrantPolicy CRD's own rules, which the
// API server applies before any admission policy.
func TestGrantPolicySchema(t *testing.T) {
	cases := []struct {
		name   string
		mutate mutation
		// stored is true for a case that keeps the name default. A create
		// would then meet the stored object, so it is an update instead.
		stored bool
		// substrs must all appear, the field and the rule it broke.
		substrs []string
	}{
		{
			name:    "refuses a name other than default",
			mutate:  func(_ *testing.T, u *unstructured.Unstructured) { u.SetName("other") },
			substrs: []string{"reads only the GrantPolicy named default"},
		},
		{
			name:    "refuses an empty images list",
			mutate:  setGrantPolicyList("images"),
			stored:  true,
			substrs: []string{"spec.images", "should have at least 1 items"},
		},
		{
			name:    "refuses an images entry with a tag",
			mutate:  setGrantPolicyList("images", "ghcr.io/azure/fiberd:0.4.0"),
			stored:  true,
			substrs: []string{"spec.images", "should match"},
		},
		{
			name:    "refuses an empty helperImages list",
			mutate:  setGrantPolicyList("helperImages"),
			stored:  true,
			substrs: []string{"spec.helperImages", "should have at least 1 items"},
		},
		{
			name:    "refuses a helperImages entry with a tag",
			mutate:  setGrantPolicyList("helperImages", "ghcr.io/azure/fiberd-helper:0.4.0"),
			stored:  true,
			substrs: []string{"spec.helperImages", "should match"},
		},
		{
			name:    "refuses an empty runtimes list",
			mutate:  setGrantPolicyList("runtimes"),
			stored:  true,
			substrs: []string{"spec.runtimes", "should have at least 1 items"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.stored {
				u := &unstructured.Unstructured{}
				u.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("GrantPolicy"))
				if err := admin.Get(t.Context(), client.ObjectKey{Name: grantPolicyName}, u); err != nil {
					t.Fatalf("get GrantPolicy %s: %v", grantPolicyName, err)
				}
				tc.mutate(t, u)
				err = admin.Update(t.Context(), u, client.DryRunAll)
			} else {
				u := testutil.ReadSample(t, grantPolicySampleFile)
				tc.mutate(t, u)
				err = admin.Create(t.Context(), u, client.DryRunAll)
			}
			for _, substr := range tc.substrs {
				expectDenied(t, err, metav1.StatusReasonInvalid, substr)
			}
		})
	}
}

// setGrantPolicyList sets a GrantPolicy spec list to items, empty when none
// are given. An empty list stays in the request, so the CRD default does not
// fill it.
func setGrantPolicyList(field string, items ...any) mutation {
	return func(t *testing.T, u *unstructured.Unstructured) {
		if err := unstructured.SetNestedSlice(u.Object, append([]any{}, items...), "spec", field); err != nil {
			t.Fatal(err)
		}
	}
}

// canCreateGrants asks the API server's authorizer whether user, in groups,
// may create CapacityGrants in ns.
func canCreateGrants(ctx context.Context, user string, groups []string, ns string) (bool, error) {
	sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User:   user,
		Groups: groups,
		ResourceAttributes: &authorizationv1.ResourceAttributes{
			Namespace: ns,
			Verb:      "create",
			Group:     v1alpha1.GroupVersion.Group,
			Resource:  "capacitygrants",
		},
	}}
	if err := admin.Create(ctx, sar); err != nil {
		return false, err
	}
	return sar.Status.Allowed, nil
}

// TestAuthorRoleAllowsCreateOnlyInBoundNamespace binds config/tenant's
// author RoleBinding to one group and checks that it grants create in that
// namespace only, to that group only.
func TestAuthorRoleAllowsCreateOnlyInBoundNamespace(t *testing.T) {
	const group = "team-a-developers"
	ns, other := namespace(t), namespace(t)

	// The RoleBinding names this ClusterRole, which config/rbac installs
	// and this package otherwise does not.
	roles, err := testutil.ReadObjects(authorRoleFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range roles {
		createOwned(t, role)
	}

	bindings, err := testutil.ReadObjects(authorBindingFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bindings {
		b.SetNamespace(ns)
		subjects, _, err := unstructured.NestedSlice(b.Object, "subjects")
		if err != nil {
			t.Fatal(err)
		}
		replaced := false
		for _, s := range subjects {
			if m, ok := s.(map[string]any); ok && m["name"] == "AUTHOR_GROUP" {
				m["name"] = group
				replaced = true
			}
		}
		if !replaced {
			t.Fatalf("%s %s has no AUTHOR_GROUP subject to replace", b.GetKind(), b.GetName())
		}
		if err := unstructured.SetNestedSlice(b.Object, subjects, "subjects"); err != nil {
			t.Fatal(err)
		}
		createOwned(t, b)
	}

	testutil.Eventually(t, "group "+group+" to be allowed to create grants in "+ns, func(ctx context.Context) (bool, error) {
		return canCreateGrants(ctx, "alice", []string{group}, ns)
	})

	cases := []struct {
		name   string
		user   string
		groups []string
		ns     string
	}{
		{name: "denies a user outside the group", user: "bob", ns: ns},
		{name: "denies the group in another namespace", user: "alice", groups: []string{group}, ns: other},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allowed, err := canCreateGrants(t.Context(), tc.user, tc.groups, tc.ns)
			if err != nil {
				t.Fatalf("SubjectAccessReview: %v", err)
			}
			if allowed {
				t.Fatalf("user %s in groups %q may create capacitygrants in %s, want denied", tc.user, tc.groups, tc.ns)
			}
		})
	}
}

// TestGrantPolicyRecovery walks the documented way to remove GrantPolicy
// default and put it back. It removes shared objects, so its cleanup
// recreates them whatever fails midway and waits until both are enforced
// again.
func TestGrantPolicyRecovery(t *testing.T) {
	sample := readSample(t, namespace(t))
	t.Cleanup(func() {
		restore(t, grantPoliciesBindingFile)
		restore(t, grantPolicySampleFile)
		testutil.Eventually(t, "deleting GrantPolicy "+grantPolicyName+" to be refused again", guardRefusesDelete)
		testutil.Eventually(t, "the sample grant to be admitted under the recreated GrantPolicy", accepted(sample))
	})

	binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: grantPoliciesPolicy}}
	if err := admin.Delete(t.Context(), binding); err != nil {
		t.Fatalf("delete ValidatingAdmissionPolicyBinding %s: %v", grantPoliciesPolicy, err)
	}
	testutil.Eventually(t, "GrantPolicy "+grantPolicyName+" to be deletable without the guard binding", func(ctx context.Context) (bool, error) {
		err := admin.Delete(ctx, grantPolicyRef())
		switch {
		case err == nil, apierrors.IsNotFound(err):
			return true, nil
		case apierrors.IsForbidden(err):
			return false, testutil.NotYet(err)
		default:
			return false, err
		}
	})
	testutil.Eventually(t, "grants to be refused for want of GrantPolicy "+grantPolicyName,
		refused(sample, metav1.StatusReasonInvalid, paramNotFoundMessage))

	restore(t, grantPolicySampleFile)
	testutil.Eventually(t, "the sample grant to be admitted under the recreated GrantPolicy", accepted(sample))

	restore(t, grantPoliciesBindingFile)
	testutil.Eventually(t, "deleting GrantPolicy "+grantPolicyName+" to be refused again", guardRefusesDelete)
}
