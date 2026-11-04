//go:build envtest

package envtest_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// podSpecPath is spec.podTemplate.spec in a CapacityGrant.
var podSpecPath = []string{"spec", "podTemplate", "spec"}

// loadSample returns config/samples' CapacityGrant in the default
// namespace.
func loadSample(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	objs, err := readObjects(filepath.Join(repoRoot, "config", "samples", "fiberd_v1alpha1_capacitygrant.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 {
		t.Fatalf("the sample holds %d objects, want 1", len(objs))
	}
	objs[0].SetNamespace("default")
	return objs[0]
}

// setPodField sets one field of spec.podTemplate.spec.
func setPodField(t *testing.T, u *unstructured.Unstructured, field string, value any) {
	t.Helper()
	if err := unstructured.SetNestedField(u.Object, value, slices.Concat(podSpecPath, []string{field})...); err != nil {
		t.Fatal(err)
	}
}

// editContainers hands the pod template's containers to edit and stores
// what it returns.
func editContainers(t *testing.T, u *unstructured.Unstructured, edit func([]any) []any) {
	t.Helper()
	path := slices.Concat(podSpecPath, []string{"containers"})
	cs, _, err := unstructured.NestedSlice(u.Object, path...)
	if err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedSlice(u.Object, edit(cs), path...); err != nil {
		t.Fatal(err)
	}
}

// TestCRDValidation creates each object with a server-side dry run, so
// the API server's schema and CEL rules decide and nothing is stored for
// the manager to see.
func TestCRDValidation(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(t *testing.T, u *unstructured.Unstructured)
		wantErr string
	}{
		{
			name:   "the sample is accepted",
			mutate: func(*testing.T, *unstructured.Unstructured) {},
		},
		{
			name: "more warm fibers than max",
			mutate: func(t *testing.T, u *unstructured.Unstructured) {
				if err := unstructured.SetNestedField(u.Object, int64(33), "spec", "fibers", "warm"); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "fibers.warm must not exceed fibers.max",
		},
		{
			name: "no pod spec",
			mutate: func(_ *testing.T, u *unstructured.Unstructured) {
				unstructured.RemoveNestedField(u.Object, podSpecPath...)
			},
			wantErr: "podTemplate.spec is required",
		},
		{
			name: "a second container",
			mutate: func(t *testing.T, u *unstructured.Unstructured) {
				editContainers(t, u, func(cs []any) []any {
					return append(cs, map[string]any{"name": "sidecar", "image": "registry.k8s.io/pause:3.10"})
				})
			},
			wantErr: "must hold exactly one container named fiberd",
		},
		{
			name: "a container command",
			mutate: func(t *testing.T, u *unstructured.Unstructured) {
				editContainers(t, u, func(cs []any) []any {
					cs[0].(map[string]any)["command"] = []any{"/bin/sh"}
					return cs
				})
			},
			wantErr: "the fiberd container's command is the image entrypoint",
		},
		{
			name: "an init container",
			mutate: func(t *testing.T, u *unstructured.Unstructured) {
				setPodField(t, u, "initContainers", []any{map[string]any{"name": "init", "image": "registry.k8s.io/pause:3.10"}})
			},
			wantErr: "initContainers and ephemeralContainers are not allowed",
		},
		{
			name:    "a service account name",
			mutate:  func(t *testing.T, u *unstructured.Unstructured) { setPodField(t, u, "serviceAccountName", "default") },
			wantErr: "serviceAccountName is set by the controller",
		},
		{
			name:    "a node name",
			mutate:  func(t *testing.T, u *unstructured.Unstructured) { setPodField(t, u, "nodeName", "node-a") },
			wantErr: "nodeName is not allowed",
		},
		{
			name:    "a restart policy other than Always",
			mutate:  func(t *testing.T, u *unstructured.Unstructured) { setPodField(t, u, "restartPolicy", "Never") },
			wantErr: "restartPolicy must be Always",
		},
		{
			name:    "the host network",
			mutate:  func(t *testing.T, u *unstructured.Unstructured) { setPodField(t, u, "hostNetwork", true) },
			wantErr: "hostPID, hostNetwork, hostIPC, shareProcessNamespace and hostUsers are set by the controller",
		},
		{
			name:    "a name longer than 56 characters",
			mutate:  func(_ *testing.T, u *unstructured.Unstructured) { u.SetName(strings.Repeat("a", 57)) },
			wantErr: "the name is at most 56 characters",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := loadSample(t)
			tc.mutate(t, u)
			err := admin.Create(t.Context(), u, client.DryRunAll)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("create: %v", err)
				}
				return
			}
			if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("create error = %v, want Invalid naming %q", err, tc.wantErr)
			}
		})
	}
}
