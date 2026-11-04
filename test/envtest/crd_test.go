//go:build envtest

package envtest_test

import (
	"path/filepath"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/fiberd-k8s-controller/test/envtest/internal/testutil"
)

// loadSample returns config/samples' CapacityGrant in the default
// namespace.
func loadSample(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	u := testutil.ReadSample(t, filepath.Join(repoRoot, "config", "samples", "fiberd_v1alpha1_capacitygrant.yaml"))
	u.SetNamespace("default")
	return u
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
				unstructured.RemoveNestedField(u.Object, testutil.PodSpecPath...)
			},
			wantErr: "podTemplate.spec is required",
		},
		{
			name: "a second container",
			mutate: func(t *testing.T, u *unstructured.Unstructured) {
				testutil.EditContainers(t, u, func(cs []any) []any {
					return append(cs, map[string]any{"name": "sidecar", "image": "registry.k8s.io/pause:3.10"})
				})
			},
			wantErr: "must hold exactly one container named fiberd",
		},
		{
			name: "a container command",
			mutate: func(t *testing.T, u *unstructured.Unstructured) {
				testutil.SetContainerField(t, u, "command", []any{"/bin/sh"})
			},
			wantErr: "the fiberd container's command is the image entrypoint",
		},
		{
			name: "an init container",
			mutate: func(t *testing.T, u *unstructured.Unstructured) {
				testutil.SetPodField(t, u, "initContainers", []any{map[string]any{"name": "init", "image": "registry.k8s.io/pause:3.10"}})
			},
			wantErr: "initContainers and ephemeralContainers are not allowed",
		},
		{
			name: "a service account name",
			mutate: func(t *testing.T, u *unstructured.Unstructured) {
				testutil.SetPodField(t, u, "serviceAccountName", "default")
			},
			wantErr: "serviceAccountName is set by the controller",
		},
		{
			name:    "a node name",
			mutate:  func(t *testing.T, u *unstructured.Unstructured) { testutil.SetPodField(t, u, "nodeName", "node-a") },
			wantErr: "nodeName is not allowed",
		},
		{
			name: "a restart policy other than Always",
			mutate: func(t *testing.T, u *unstructured.Unstructured) {
				testutil.SetPodField(t, u, "restartPolicy", "Never")
			},
			wantErr: "restartPolicy must be Always",
		},
		{
			name:    "the host network",
			mutate:  func(t *testing.T, u *unstructured.Unstructured) { testutil.SetPodField(t, u, "hostNetwork", true) },
			wantErr: "hostPID, hostNetwork, hostIPC, shareProcessNamespace and hostUsers are set by the controller",
		},
		{
			name: "no memory limit",
			mutate: func(t *testing.T, u *unstructured.Unstructured) {
				testutil.EditContainers(t, u, func(cs []any) []any {
					unstructured.RemoveNestedField(cs[0].(map[string]any), "resources", "limits", "memory")
					return cs
				})
			},
			wantErr: "must set resources.limits.memory",
		},
		{
			name: "a startup probe",
			mutate: func(t *testing.T, u *unstructured.Unstructured) {
				testutil.SetContainerField(t, u, "startupProbe", map[string]any{"tcpSocket": map[string]any{"port": int64(1)}})
			},
			wantErr: "startupProbe, readinessProbe and livenessProbe are set by the controller",
		},
		{
			name: "Pod-level resources",
			mutate: func(t *testing.T, u *unstructured.Unstructured) {
				testutil.SetPodField(t, u, "resources", map[string]any{"limits": map[string]any{"memory": "1Gi"}})
			},
			wantErr: "podTemplate.spec.resources is not allowed",
		},
		{
			name: "a postStart hook",
			mutate: func(t *testing.T, u *unstructured.Unstructured) {
				testutil.SetContainerField(t, u, "lifecycle", map[string]any{
					"postStart": map[string]any{"exec": map[string]any{"command": []any{"/bin/sh"}}},
				})
			},
			wantErr: "lifecycle is not allowed",
		},
		{
			name: "host aliases",
			mutate: func(t *testing.T, u *unstructured.Unstructured) {
				testutil.SetPodField(t, u, "hostAliases", []any{map[string]any{"ip": "10.0.0.9", "hostnames": []any{"issuer.example"}}})
			},
			wantErr: "hostAliases, dnsConfig and runtimeClassName are not allowed",
		},
		{
			name: "a DNS config",
			mutate: func(t *testing.T, u *unstructured.Unstructured) {
				testutil.SetPodField(t, u, "dnsConfig", map[string]any{"nameservers": []any{"10.0.0.9"}})
			},
			wantErr: "hostAliases, dnsConfig and runtimeClassName are not allowed",
		},
		{
			name: "a runtime class",
			mutate: func(t *testing.T, u *unstructured.Unstructured) {
				testutil.SetPodField(t, u, "runtimeClassName", "kata")
			},
			wantErr: "hostAliases, dnsConfig and runtimeClassName are not allowed",
		},
		{
			name: "an envFrom source",
			mutate: func(t *testing.T, u *unstructured.Unstructured) {
				testutil.SetContainerField(t, u, "envFrom", []any{map[string]any{"configMapRef": map[string]any{"name": "trust"}}})
			},
			wantErr: "envFrom is not allowed",
		},
		{
			name: "container SELinux options",
			mutate: func(t *testing.T, u *unstructured.Unstructured) {
				testutil.SetContainerField(t, u, "securityContext", map[string]any{"seLinuxOptions": map[string]any{"type": "spc_t"}})
			},
			wantErr: "seLinuxOptions and windowsOptions are not allowed",
		},
		{
			name: "Pod SELinux options",
			mutate: func(t *testing.T, u *unstructured.Unstructured) {
				testutil.SetPodField(t, u, "securityContext", map[string]any{"seLinuxOptions": map[string]any{"type": "spc_t"}})
			},
			wantErr: "seLinuxOptions and windowsOptions are not allowed",
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
