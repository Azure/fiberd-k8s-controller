//go:build chart

package chart

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const (
	// go test runs in this package, and every path below is from the root.
	repoRoot = "../.."

	chartDir          = "helm/fiberd-k8s-controller"
	configDefault     = "config/default"
	tenantOverlay     = "test/chart/testdata/tenant"
	grantPolicySample = "config/samples/fiberd_v1alpha1_grantpolicy.yaml"

	release         = "fiberd-k8s-controller"
	namespace       = "fiberd-system"
	tenantNamespace = "team-a"
	repository      = "ghcr.io/azure/fiberd-k8s-controller"
	certManagerAPI  = "cert-manager.io/v1"

	// The default name, the API group that keeps it whatever the install
	// calls cert-manager, and the names a non-default install uses.
	defaultName       = "cert-manager"
	apiGroup          = "cert-manager.io"
	subjectPrefix     = "system:serviceaccount:"
	namespaceOverride = "tls-system"
	saOverride        = "tls-controller"
	csiOverride       = "tls-driver"
)

var digest = "sha256:" + strings.Repeat("0", 64)

// object is one parsed Kubernetes object. Parsing is the same on both sides
// of a comparison, so types and comments never make a difference.
type object = map[string]any

func TestRender(t *testing.T) {
	tests := []struct {
		name string
		set  []string
		// keep picks the rendered objects the row compares.
		keep func(object) bool
		want func(*testing.T) []object
		// clean also fails the row when a default name survives in any object.
		clean bool
	}{
		{
			name: "default values match config/default",
			keep: isNot("GrantPolicy"),
			want: installed,
		},
		{
			name: "a tenant matches config/tenant",
			set: []string{
				"tenants[0].namespace=" + tenantNamespace,
				"tenants[0].authorGroup=team-a-developers",
			},
			keep: inNamespace(tenantNamespace),
			want: func(t *testing.T) []object {
				return kustomizeBuild(t, tenantOverlay)
			},
		},
		{
			name: "the GrantPolicy matches the sample",
			keep: is("GrantPolicy"),
			want: func(t *testing.T) []object {
				data, err := os.ReadFile(filepath.Join(repoRoot, grantPolicySample))
				if err != nil {
					t.Fatalf("reading the sample: %v", err)
				}
				return decode(t, data)
			},
		},
		{
			name: "cert-manager names leave no default behind",
			set: []string{
				"certManager.namespace=" + namespaceOverride,
				"certManager.serviceAccount=" + saOverride,
				"certManager.csiDriverServiceAccount=" + csiOverride,
			},
			keep: isNot("GrantPolicy"),
			want: func(t *testing.T) []object {
				objects := installed(t)
				for i, obj := range objects {
					objects[i] = mapStrings(obj, rename).(object)
				}
				return objects
			},
			clean: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := helmTemplate(t, renderArgs(tc.set)...)
			if err != nil {
				t.Fatalf("helm template: %v", err)
			}
			rendered := decode(t, out)
			got := slices.DeleteFunc(slices.Clone(rendered), func(obj object) bool { return !tc.keep(obj) })
			if diff := cmp.Diff(index(t, tc.want(t)), index(t, got)); diff != "" {
				t.Errorf("objects differ (-want +got):\n%s", diff)
			}
			if tc.clean {
				for _, id := range survivors(t, rendered) {
					t.Errorf("a default name survives in %s", id)
				}
			}
		})
	}
}

func TestRenderRefuses(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "an empty digest",
			args: []string{"--api-versions", certManagerAPI},
			want: "image.digest is required",
		},
		{
			name: "no cert-manager API",
			args: []string{"--set", "image.digest=" + digest},
			want: certManagerAPI + " is not served",
		},
		{
			name: "a tenant with no authorGroup",
			args: renderArgs([]string{"tenants[0].namespace=" + tenantNamespace}),
			want: "every tenant needs an authorGroup",
		},
		{
			name: "a tenant with no namespace",
			args: renderArgs([]string{"tenants[0].authorGroup=team-a-developers"}),
			want: "every tenant needs a namespace",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := helmTemplate(t, tc.args...)
			if err == nil {
				t.Fatalf("helm template succeeded, want %q. It wrote:\n%s", tc.want, out)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("helm template error = %v, want it to hold %q", err, tc.want)
			}
		})
	}
}

func renderArgs(set []string) []string {
	args := []string{"--api-versions", certManagerAPI, "--set", "image.digest=" + digest}
	for _, value := range set {
		args = append(args, "--set", value)
	}
	return args
}

func helmTemplate(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	base := []string{"template", release, chartDir, "--namespace", namespace, "--include-crds"}
	return output(t, "helm", slices.Concat(base, args))
}

// installed is config/default as the chart should render it. Helm creates the
// Namespace, and the chart names the image by digest.
func installed(t *testing.T) []object {
	t.Helper()
	objects := slices.DeleteFunc(kustomizeBuild(t, configDefault), is("Namespace"))
	image := repository + "@" + digest
	for i, obj := range objects {
		objects[i] = mapStrings(obj, func(value string) string {
			if value == "controller:latest" {
				return image
			}
			return value
		}).(object)
	}
	return objects
}

func kustomizeBuild(t *testing.T, dir string) []object {
	t.Helper()
	kustomize := strings.Fields(os.Getenv("KUSTOMIZE"))
	if len(kustomize) == 0 {
		t.Fatal("KUSTOMIZE is unset, run make test-chart")
	}
	out, err := output(t, kustomize[0], slices.Concat(kustomize[1:], []string{"build", dir}))
	if err != nil {
		t.Fatalf("kustomize build %s: %v", dir, err)
	}
	return decode(t, out)
}

// output runs a command from the root, with its stderr in any error.
func output(t *testing.T, name string, args []string) ([]byte, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), name, args...)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return out, fmt.Errorf("%w: %s", err, exit.Stderr)
	}
	return out, err
}

func decode(t *testing.T, data []byte) []object {
	t.Helper()
	decoder := utilyaml.NewYAMLToJSONDecoder(bytes.NewReader(data))
	var objects []object
	for {
		var obj object
		err := decoder.Decode(&obj)
		if errors.Is(err, io.EOF) {
			return objects
		}
		if err != nil {
			t.Fatalf("parsing objects: %v", err)
		}
		if obj != nil {
			objects = append(objects, obj)
		}
	}
}

// index keys objects by identity, so order never matters and a repeat fails.
func index(t *testing.T, objects []object) map[string]object {
	t.Helper()
	out := make(map[string]object, len(objects))
	for _, obj := range objects {
		id := identity(obj)
		if _, ok := out[id]; ok {
			t.Errorf("%s appears twice", id)
		}
		out[id] = obj
	}
	return out
}

func identity(obj object) string {
	return fmt.Sprintf("%v %v %v/%v", obj["apiVersion"], obj["kind"], metadata(obj)["namespace"], metadata(obj)["name"])
}

func metadata(obj object) object {
	meta, _ := obj["metadata"].(object)
	return meta
}

func is(kind string) func(object) bool {
	return func(obj object) bool { return obj["kind"] == kind }
}

func isNot(kind string) func(object) bool {
	return func(obj object) bool { return obj["kind"] != kind }
}

func inNamespace(name string) func(object) bool {
	return func(obj object) bool { return metadata(obj)["namespace"] == name }
}

// rename does to one config/default value what the non-default values do.
func rename(value string) string {
	if value == defaultName {
		return namespaceOverride
	}
	old := subjectPrefix + defaultName + ":" + defaultName
	value = strings.ReplaceAll(value, old+"-csi-driver", subjectPrefix+namespaceOverride+":"+csiOverride)
	return strings.ReplaceAll(value, old, subjectPrefix+namespaceOverride+":"+saOverride)
}

// survivors lists the objects that still spell a cert-manager name in any key
// or value, however it is spelled. The API group stays, and so do the words in
// CRD descriptions.
func survivors(t *testing.T, objects []object) []string {
	t.Helper()
	var found []string
	for _, obj := range objects {
		if obj["kind"] == "CustomResourceDefinition" {
			continue
		}
		data, err := json.Marshal(obj)
		if err != nil {
			t.Fatalf("writing %s: %v", identity(obj), err)
		}
		if strings.Contains(strings.ReplaceAll(string(data), apiGroup, ""), defaultName) {
			found = append(found, identity(obj))
		}
	}
	return found
}

func mapStrings(value any, visit func(string) string) any {
	switch v := value.(type) {
	case string:
		return visit(v)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = mapStrings(item, visit)
		}
		return out
	case object:
		out := make(object, len(v))
		for key, item := range v {
			out[key] = mapStrings(item, visit)
		}
		return out
	default:
		return value
	}
}
