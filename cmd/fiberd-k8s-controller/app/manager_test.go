package app

import (
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/fiberd/pkg/tlsconf/tlsconftest"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/pkg/issuer"
)

// secretGetMarker captures the resource name of the RBAC marker that lets
// the controller read the key Secret.
var secretGetMarker = regexp.MustCompile(`(?m)^// \+kubebuilder:rbac:.*resources=secrets,resourceNames=([^,]+),verbs=get$`)

// TestKeySecretMarker pins the RBAC marker's resource name to
// issuer.KeySecretName, since a marker holds a literal and a rename of the
// Secret would otherwise leave the controller forbidden at startup.
func TestKeySecretMarker(t *testing.T) {
	src, err := os.ReadFile("manager.go")
	if err != nil {
		t.Fatal(err)
	}
	matches := secretGetMarker.FindAllSubmatch(src, -1)
	if len(matches) != 1 {
		t.Fatalf("manager.go holds %d secrets get markers with a resource name, want 1", len(matches))
	}
	if got := string(matches[0][1]); got != issuer.KeySecretName {
		t.Errorf("the secrets get marker names %q, want issuer.KeySecretName %q", got, issuer.KeySecretName)
	}
}

// TestCacheOptionsFilterGrantObjects pins what the reconciler and the grant
// route can read from the cache. A filter that matched nothing would send
// every read live, and one that matched everything would cache every Pod,
// Deployment and Service of the cluster.
func TestCacheOptionsFilterGrantObjects(t *testing.T) {
	opts, err := cacheOptions()
	if err != nil {
		t.Fatalf("cacheOptions: %v", err)
	}
	if opts.DefaultTransform == nil {
		t.Error("the cache keeps managed fields, want them dropped")
	}
	if got := len(opts.ByObject); got != 4 {
		t.Fatalf("the cache filters %d kinds, want 4", got)
	}

	type labelCase struct {
		name   string
		labels labels.Set
		want   bool
	}
	ofGrant := []labelCase{
		{name: "an object of a grant", labels: labels.Set{v1alpha1.LabelGrant: "web"}, want: true},
		{name: "a label with no value still counts", labels: labels.Set{v1alpha1.LabelGrant: ""}, want: true},
		{name: "an object of no grant", labels: labels.Set{"app": "web"}},
		{name: "an object with no labels"},
	}
	managed := []labelCase{
		{name: "an object the controller manages", labels: labels.Set{v1alpha1.LabelManagedBy: v1alpha1.ManagedBy}, want: true},
		{name: "an object another tool manages", labels: labels.Set{v1alpha1.LabelManagedBy: "helm"}},
		{name: "an object of a grant's name with no label", labels: labels.Set{v1alpha1.LabelGrant: "web"}},
		{name: "an object with no labels"},
	}
	kinds := []struct {
		obj   client.Object
		cases []labelCase
	}{
		{obj: &corev1.Pod{}, cases: ofGrant},
		{obj: &appsv1.ReplicaSet{}, cases: ofGrant},
		{obj: &appsv1.Deployment{}, cases: managed},
		{obj: &corev1.Service{}, cases: managed},
	}
	for _, kind := range kinds {
		selector := cacheSelector(t, opts, kind.obj)
		for _, tc := range kind.cases {
			t.Run(fmt.Sprintf("%T %s", kind.obj, tc.name), func(t *testing.T) {
				if got := selector.Matches(tc.labels); got != tc.want {
					t.Errorf("the selector %q matches %v = %t, want %t", selector, tc.labels, got, tc.want)
				}
			})
		}
	}
}

// cacheSelector is the label selector opts holds for obj's kind. ByObject is
// keyed by pointer, so a lookup by value cannot find it.
func cacheSelector(t *testing.T, opts cache.Options, obj client.Object) labels.Selector {
	t.Helper()
	for key, byObject := range opts.ByObject {
		if reflect.TypeOf(key) == reflect.TypeOf(obj) {
			return byObject.Label
		}
	}
	t.Fatalf("the cache does not filter %T", obj)
	return nil
}

func TestLoadPool(t *testing.T) {
	dir := t.TempDir()
	ca := tlsconftest.NewCA(t, "fiberd-ca")
	caFile := filepath.Join(dir, "ca.crt")
	empty := filepath.Join(dir, "empty.crt")
	for path, data := range map[string][]byte{caFile: ca.CertPEM(), empty: []byte("no certificate here\n")} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	pool, err := loadPool(caFile)
	if err != nil {
		t.Fatalf("loadPool: %v", err)
	}
	if _, err := ca.Client(t, "caller").Cert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("a client certificate from the CA does not verify against the pool: %v", err)
	}
	for _, path := range []string{empty, filepath.Join(dir, "missing.crt")} {
		if _, err := loadPool(path); err == nil {
			t.Errorf("loadPool(%s) = nil error, want one", filepath.Base(path))
		}
	}
}
