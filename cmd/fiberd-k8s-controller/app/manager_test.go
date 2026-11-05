package app

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

// TestCacheOptionsFilterGrantObjects pins what the grant route can read from
// the cache. A filter that matched nothing would send every read live, and
// one that matched everything would cache every Pod of the cluster.
func TestCacheOptionsFilterGrantObjects(t *testing.T) {
	opts, err := cacheOptions()
	if err != nil {
		t.Fatalf("cacheOptions: %v", err)
	}
	if opts.DefaultTransform == nil {
		t.Error("the cache keeps managed fields, want them dropped")
	}
	if got := len(opts.ByObject); got != 2 {
		t.Fatalf("the cache filters %d kinds, want 2", got)
	}

	cases := []struct {
		name   string
		labels labels.Set
		want   bool
	}{
		{name: "an object of a grant", labels: labels.Set{v1alpha1.LabelGrant: "web"}, want: true},
		{name: "a label with no value still counts", labels: labels.Set{v1alpha1.LabelGrant: ""}, want: true},
		{name: "an object of no grant", labels: labels.Set{"app": "web"}},
		{name: "an object with no labels"},
	}
	for _, obj := range []client.Object{&corev1.Pod{}, &appsv1.ReplicaSet{}} {
		selector := cacheSelector(t, opts, obj)
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%T %s", obj, tc.name), func(t *testing.T) {
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
