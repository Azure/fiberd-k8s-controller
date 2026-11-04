package v1alpha1_test

import (
	"regexp"
	"testing"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

func TestImageRepositoryPattern(t *testing.T) {
	re := regexp.MustCompile(v1alpha1.ImageRepositoryPattern)

	cases := []struct {
		repo string
		want bool
	}{
		{repo: "ghcr.io/azure/fiberd", want: true},
		{repo: "ghcr.io/azure", want: true},
		{repo: "ghcr.io/azure/fiberd-runc", want: true},
		{repo: "localhost/x", want: true},
		{repo: "localhost:5000/x", want: true},
		{repo: "registry:5000/team/x", want: true},
		{repo: "registry.example:5000/x", want: true},
		{repo: "registry.example/fiberd-refzygote", want: true},
		{repo: "ghcr.io/azure/fiberd_v2.images", want: true},

		// A bare name is a Docker Hub shorthand, not a fully qualified repository.
		{repo: "fiberd", want: false},
		{repo: "library/fiberd", want: false},

		// A host alone would admit a whole registry.
		{repo: "ghcr.io", want: false},
		{repo: "localhost", want: false},
		{repo: "localhost:5000", want: false},
		{repo: "registry:5000", want: false},

		// A tag or a digest is not part of a repository.
		{repo: "ghcr.io/azure/fiberd:0.4.0", want: false},
		{repo: "ghcr.io/azure/fiberd@sha256:0123", want: false},

		// Hosts and paths are lowercase, and path components do not start or
		// end in a separator.
		{repo: "GHCR.io/azure/fiberd", want: false},
		{repo: "ghcr.io/azure/fiberd/", want: false},
		{repo: "ghcr.io/azure/.fiberd", want: false},
		{repo: "ghcr.io/azure/fiberd-", want: false},
		{repo: "-ghcr.io/azure/fiberd", want: false},
		{repo: "ghcr.io/azure fiberd", want: false},
		{repo: "", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.repo, func(t *testing.T) {
			if got := re.MatchString(tc.repo); got != tc.want {
				t.Errorf("MatchString(%q) = %v, want %v", tc.repo, got, tc.want)
			}
		})
	}
}
