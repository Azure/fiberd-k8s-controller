package v1alpha1_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	"github.com/Azure/fiberd/pkg/core"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

const crdPath = "../../config/crd/bases/fiberd.io_capacitygrants.yaml"

const crdVersion = "v1alpha1"

func ptr[T any](v T) *T { return &v }

func loadCRD(t *testing.T) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	raw, err := os.ReadFile(crdPath)
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s is missing, run `make manifests` and commit the result", crdPath)
	}
	if err != nil {
		t.Fatalf("read %s: %v", crdPath, err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("parse %s: %v", crdPath, err)
	}
	return &crd
}

func loadVersion(t *testing.T) (*apiextensionsv1.CustomResourceDefinition, *apiextensionsv1.CustomResourceDefinitionVersion) {
	t.Helper()
	crd := loadCRD(t)
	for i := range crd.Spec.Versions {
		if crd.Spec.Versions[i].Name == crdVersion {
			return crd, &crd.Spec.Versions[i]
		}
	}
	t.Fatalf("%s has no version %s", crdPath, crdVersion)
	return nil, nil
}

// schemaAt walks properties from the root of the schema. The segment "[]"
// steps into the items of an array. An empty path is the root itself.
func schemaAt(root *apiextensionsv1.JSONSchemaProps, path string) (*apiextensionsv1.JSONSchemaProps, error) {
	cur := root
	if path == "" {
		return cur, nil
	}
	for _, seg := range strings.Split(path, ".") {
		if seg == "[]" {
			if cur.Items == nil || cur.Items.Schema == nil {
				return nil, fmt.Errorf("path %q, the schema before %q is not an array", path, seg)
			}
			cur = cur.Items.Schema
			continue
		}
		next, ok := cur.Properties[seg]
		if !ok {
			return nil, fmt.Errorf("path %q, no property %q", path, seg)
		}
		cur = &next
	}
	return cur, nil
}

func enumValues(t *testing.T, s *apiextensionsv1.JSONSchemaProps) []string {
	t.Helper()
	out := make([]string, 0, len(s.Enum))
	for _, e := range s.Enum {
		var v string
		if err := json.Unmarshal(e.Raw, &v); err != nil {
			t.Fatalf("enum value %s is not a string: %v", e.Raw, err)
		}
		out = append(out, v)
	}
	return out
}

func sameSet(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

type schemaCase struct {
	name        string
	path        string
	enum        []string
	minimum     *float64
	minItems    *int64
	pattern     string
	required    []string
	defaultJSON string
	listType    string
	listMapKeys []string
	intOrString bool
	properties  []string
	rules       []string

	// rulesNeedFieldPath asks that every rule of the case names a fieldPath.
	rulesNeedFieldPath bool
}

// podTemplateRules are the CEL rules on spec.podTemplate, in marker order.
var podTemplateRules = []string{
	"has(self.spec)",
	"!has(self.spec) || (self.spec.containers.size() == 1 && self.spec.containers[0].name == 'fiberd')",
	"!has(self.spec) || self.spec.containers.size() == 0 || !has(self.spec.containers[0].command)",
	"!has(self.spec) || (!has(self.spec.initContainers) && !has(self.spec.ephemeralContainers))",
	"!has(self.spec) || (!has(self.spec.serviceAccountName) && !has(self.spec.serviceAccount))",
	"!has(self.spec) || !has(self.spec.nodeName)",
	"!has(self.spec) || !has(self.spec.restartPolicy) || self.spec.restartPolicy == 'Always'",
	"!has(self.spec) || ((!has(self.spec.hostPID) || !self.spec.hostPID) && (!has(self.spec.hostNetwork) || !self.spec.hostNetwork) && (!has(self.spec.hostIPC) || !self.spec.hostIPC) && (!has(self.spec.shareProcessNamespace) || !self.spec.shareProcessNamespace) && !has(self.spec.hostUsers))",
}

// warmRule is the CEL rule on spec.fibers.
const warmRule = "!has(self.warm) || self.warm <= self.max"

// nameRule is the CEL rule on the root of the grant.
const nameRule = "self.metadata.name.size() <= 56"

func TestCapacityGrantSchema(t *testing.T) {
	_, version := loadVersion(t)
	if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
		t.Fatalf("version %s of %s has no openAPIV3Schema", crdVersion, crdPath)
	}
	root := version.Schema.OpenAPIV3Schema

	cases := []schemaCase{
		{
			name:     "grant requires spec",
			path:     "",
			required: []string{"spec"},
		},
		{
			name:  "grant carries the name rule",
			path:  "",
			rules: []string{nameRule},
		},
		{
			name:     "spec requires zygote fibers and podTemplate",
			path:     "spec",
			required: []string{"zygote", "fibers", "podTemplate"},
		},
		{
			name:     "zygote requires digest and command",
			path:     "spec.zygote",
			required: []string{"digest", "command"},
		},
		{name: "zygote digest is a sha256 pattern", path: "spec.zygote.digest", pattern: v1alpha1.DigestPattern},
		{
			name:     "zygote command has at least one item",
			path:     "spec.zygote.command",
			minItems: ptr(int64(1)),
		},
		{
			name:     "zygote command is atomic",
			path:     "spec.zygote.command",
			listType: "atomic",
		},
		{
			name:     "fibers requires max",
			path:     "spec.fibers",
			required: []string{"max"},
		},
		{
			name:  "fibers carries the warm rule",
			path:  "spec.fibers",
			rules: []string{warmRule},
		},
		{
			name:    "fibers max is at least one",
			path:    "spec.fibers.max",
			minimum: ptr(float64(1)),
		},
		{
			name:    "fibers warm is not negative",
			path:    "spec.fibers.warm",
			minimum: ptr(float64(0)),
		},
		{
			name:        "writeBudget is an int or string quantity",
			path:        "spec.writeBudget",
			intOrString: true,
		},
		{
			name:    "callerThumbprint is a base64url sha256",
			path:    "spec.callerThumbprint",
			pattern: core.CallerThumbprintPattern,
		},
		{
			name: "runtime is a known backend",
			path: "spec.runtime",
			enum: []string{"proc", "runc", "gvisor", "hyperlight"},
		},
		{
			name:        "runtime defaults to proc",
			path:        "spec.runtime",
			defaultJSON: `"proc"`,
		},
		{
			name:       "podTemplate has metadata and spec",
			path:       "spec.podTemplate",
			properties: []string{"metadata", "spec"},
		},
		{
			name:       "podTemplate metadata keeps labels and annotations",
			path:       "spec.podTemplate.metadata",
			properties: []string{"labels", "annotations"},
		},
		{
			name:               "podTemplate carries the controller's rules",
			path:               "spec.podTemplate",
			rules:              podTemplateRules,
			rulesNeedFieldPath: true,
		},
		{
			name:     "podTemplate spec requires containers",
			path:     "spec.podTemplate.spec",
			required: []string{"containers"},
		},
		{
			name:     "podTemplate container requires name",
			path:     "spec.podTemplate.spec.containers.[]",
			required: []string{"name"},
		},
		{
			name:        "status conditions are a map keyed by type",
			path:        "status.conditions",
			listType:    "map",
			listMapKeys: []string{"type"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := schemaAt(root, tc.path)
			if err != nil {
				t.Fatal(err)
			}
			asserted := 0
			if tc.enum != nil {
				asserted++
				if have := enumValues(t, got); !sameSet(have, tc.enum) {
					t.Errorf("enum = %v, want %v", have, tc.enum)
				}
			}
			if tc.minimum != nil {
				asserted++
				if got.Minimum == nil || *got.Minimum != *tc.minimum {
					t.Errorf("minimum = %v, want %v", got.Minimum, *tc.minimum)
				}
			}
			if tc.minItems != nil {
				asserted++
				if got.MinItems == nil || *got.MinItems != *tc.minItems {
					t.Errorf("minItems = %v, want %v", got.MinItems, *tc.minItems)
				}
			}
			if tc.pattern != "" {
				asserted++
				if got.Pattern != tc.pattern {
					t.Errorf("pattern = %q, want %q", got.Pattern, tc.pattern)
				}
			}
			if tc.required != nil {
				asserted++
				if !sameSet(got.Required, tc.required) {
					t.Errorf("required = %v, want %v", got.Required, tc.required)
				}
			}
			if tc.defaultJSON != "" {
				asserted++
				if got.Default == nil || string(got.Default.Raw) != tc.defaultJSON {
					t.Errorf("default = %v, want %s", got.Default, tc.defaultJSON)
				}
			}
			if tc.listType != "" {
				asserted++
				if got.XListType == nil || *got.XListType != tc.listType {
					t.Errorf("x-kubernetes-list-type = %v, want %s", got.XListType, tc.listType)
				}
				// slices.Equal treats nil and empty as equal, so a case without map keys matches either.
				if !slices.Equal(got.XListMapKeys, tc.listMapKeys) {
					t.Errorf("x-kubernetes-list-map-keys = %v, want %v", got.XListMapKeys, tc.listMapKeys)
				}
			}
			if tc.intOrString {
				asserted++
				if !got.XIntOrString {
					t.Error("x-kubernetes-int-or-string = false, want true")
				}
			}
			if tc.properties != nil {
				asserted++
				for _, prop := range tc.properties {
					if _, ok := got.Properties[prop]; !ok {
						t.Errorf("property %q is missing", prop)
					}
				}
			}
			if tc.rules != nil {
				asserted++
				have := make([]string, 0, len(got.XValidations))
				for _, r := range got.XValidations {
					have = append(have, r.Rule)
					if r.Message == "" {
						t.Errorf("rule %q has no message", r.Rule)
					}
					if tc.rulesNeedFieldPath && r.FieldPath == "" {
						t.Errorf("rule %q has no fieldPath", r.Rule)
					}
				}
				if !slices.Equal(have, tc.rules) {
					t.Errorf("x-kubernetes-validations rules = %q, want %q", have, tc.rules)
				}
			}
			if asserted == 0 {
				t.Fatal("the case asserts nothing")
			}
		})
	}
}

func columnPath(v *apiextensionsv1.CustomResourceDefinitionVersion, name string) string {
	for _, c := range v.AdditionalPrinterColumns {
		if c.Name == name {
			return c.JSONPath
		}
	}
	return ""
}

func columnNames(v *apiextensionsv1.CustomResourceDefinitionVersion) []string {
	names := make([]string, 0, len(v.AdditionalPrinterColumns))
	for _, c := range v.AdditionalPrinterColumns {
		names = append(names, c.Name)
	}
	return names
}

func TestCapacityGrantCRDMetadata(t *testing.T) {
	crd, version := loadVersion(t)

	cases := []struct {
		name string
		got  any
		want any
	}{
		{name: "group", got: crd.Spec.Group, want: "fiberd.io"},
		{name: "kind", got: crd.Spec.Names.Kind, want: "CapacityGrant"},
		{name: "plural", got: crd.Spec.Names.Plural, want: "capacitygrants"},
		{name: "short names", got: crd.Spec.Names.ShortNames, want: []string{"cg"}},
		{name: "scope", got: string(crd.Spec.Scope), want: "Namespaced"},
		{name: "served", got: version.Served, want: true},
		{name: "storage", got: version.Storage, want: true},
		{name: "status subresource", got: version.Subresources != nil && version.Subresources.Status != nil, want: true},
		{name: "printer columns", got: columnNames(version), want: []string{"Ready", "Age"}},
		{name: "ready column path", got: columnPath(version, "Ready"), want: `.status.conditions[?(@.type=="Ready")].status`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !reflect.DeepEqual(tc.got, tc.want) {
				t.Errorf("got %v, want %v", tc.got, tc.want)
			}
		})
	}
}
