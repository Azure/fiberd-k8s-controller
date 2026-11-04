package app

import (
	"os"
	"regexp"
	"testing"

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
