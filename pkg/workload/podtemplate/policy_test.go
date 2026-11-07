package podtemplate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/yaml"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

// grantsPolicyFile is the admission policy that the builder's checks repeat,
// seen from this package's directory.
const grantsPolicyFile = "../../../config/policy/grants_policy.yaml"

// hasSource finds each volume source the policy's has() chain names.
var hasSource = regexp.MustCompile(`has\(v\.(\w+)\)`)

// TestPolicyMatchesBuilder keeps the admission policy's lists and arg
// pattern equal to the builder's. The two refuse the same grants, one at
// admission and one at reconcile time, and nothing else ties them.
func TestPolicyMatchesBuilder(t *testing.T) {
	vap := readPolicy(t)

	bareArgs := make([]string, 0, 2*len(tenantBoolFlags))
	for f := range tenantBoolFlags {
		bareArgs = append(bareArgs, "-"+f, "--"+f)
	}
	lists := []struct {
		name     string
		variable string
		want     []string
		// anyOrder is set where the policy's list is not sorted.
		anyOrder bool
	}{
		{name: "tenantFlags equals tenantFlags", variable: "tenantFlags", want: sortedNames(tenantFlags)},
		{name: "bareArgs holds both forms of tenantBoolFlags", variable: "bareArgs", want: bareArgs, anyOrder: true},
		{name: "mountRoots equals mountRoots", variable: "mountRoots", want: mountRoots},
		{name: "ownedVolumes equals ownedVolumes", variable: "ownedVolumes", want: ownedVolumes, anyOrder: true},
		{
			name:     "reservedKeyPrefixes equals reservedKeyPrefixes",
			variable: "reservedKeyPrefixes",
			want:     reservedKeyPrefixes,
			anyOrder: true,
		},
	}
	for _, tc := range lists {
		t.Run(tc.name, func(t *testing.T) {
			got, err := celStringList(policyVariable(t, vap, tc.variable))
			if err != nil {
				t.Fatalf("variable %s: %v", tc.variable, err)
			}
			want := tc.want
			if tc.anyOrder {
				got, want = slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))
			}
			if !slices.Equal(got, want) {
				t.Errorf("variable %s = %q, want %q", tc.variable, got, want)
			}
		})
	}

	quoted := "'" + ArgPattern + "'"
	first := vap.Spec.Validations[0]
	patterns := []struct {
		name string
		expr string
	}{
		{name: "the first expression holds ArgPattern", expr: first.Expression},
		{name: "the first messageExpression holds ArgPattern", expr: first.MessageExpression},
	}
	for _, tc := range patterns {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.expr, quoted) {
				t.Errorf("%q does not hold %s", tc.expr, quoted)
			}
		})
	}

	t.Run("envNamePattern equals EnvNamePattern", func(t *testing.T) {
		if got, want := policyVariable(t, vap, "envNamePattern"), "'"+EnvNamePattern+"'"; got != want {
			t.Errorf("variable envNamePattern = %s, want %s", got, want)
		}
	})
	t.Run("the env rule's message holds EnvNamePattern", func(t *testing.T) {
		if msg := policyValidation(t, vap, "variables.outsideEnv").Message; !strings.Contains(msg, EnvNamePattern) {
			t.Errorf("%q does not hold %s", msg, EnvNamePattern)
		}
	})
	t.Run("guestArgPattern equals GuestArgPattern", func(t *testing.T) {
		if got, want := policyVariable(t, vap, "guestArgPattern"), "'"+GuestArgPattern+"'"; got != want {
			t.Errorf("variable guestArgPattern = %s, want %s", got, want)
		}
	})
	t.Run("runtimeLabelPrefix equals LabelRuntimePrefix", func(t *testing.T) {
		if got, want := policyVariable(t, vap, "runtimeLabelPrefix"), "'"+v1alpha1.LabelRuntimePrefix+"'"; got != want {
			t.Errorf("variable runtimeLabelPrefix = %s, want %s", got, want)
		}
	})
	t.Run("runtimeLabelValue equals LabelRuntimeValue", func(t *testing.T) {
		if got, want := policyVariable(t, vap, "runtimeLabelValue"), "'"+v1alpha1.LabelRuntimeValue+"'"; got != want {
			t.Errorf("variable runtimeLabelValue = %s, want %s", got, want)
		}
	})
}

// TestPolicyVolumeSources keeps the sources the admission policy allows
// equal to allowedSource, over every field of VolumeSource, so a source
// Kubernetes adds is judged by this test the day the dependency moves. The
// policy's message lists the sources for the tenant, so it is checked too.
func TestPolicyVolumeSources(t *testing.T) {
	vap := readPolicy(t)
	allowed := sets.New[string]()
	for _, m := range hasSource.FindAllStringSubmatch(policyVariable(t, vap, "deniedVolumes"), -1) {
		allowed.Insert(m[1])
	}
	if allowed.Len() == 0 {
		t.Fatalf("variable deniedVolumes of %s names no source", grantsPolicyFile)
	}

	known := sets.New[string]()
	source := reflect.TypeFor[corev1.VolumeSource]()
	for i := range source.NumField() {
		f := source.Field(i)
		if f.Type.Kind() != reflect.Pointer {
			t.Fatalf("VolumeSource field %s is a %s, want a pointer", f.Name, f.Type)
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		known.Insert(name)
		t.Run(name, func(t *testing.T) {
			var s corev1.VolumeSource
			reflect.ValueOf(&s).Elem().Field(i).Set(reflect.New(f.Type.Elem()))
			if got, want := allowedSource(&s), allowed.Has(name); got != want {
				t.Errorf("allowedSource = %t, the policy allows it = %t", got, want)
			}
		})
	}

	message := policyValidation(t, vap, "variables.deniedVolumes").MessageExpression
	for _, name := range sets.List(allowed) {
		t.Run(name+" in the policy", func(t *testing.T) {
			if !known.Has(name) {
				t.Errorf("the policy allows %s, which VolumeSource does not have", name)
			}
			if !strings.Contains(message, name) {
				t.Errorf("message %q does not name %s", message, name)
			}
		})
	}
}

// readPolicy parses the admission policy.
func readPolicy(t *testing.T) *admissionregistrationv1.ValidatingAdmissionPolicy {
	t.Helper()
	raw, err := os.ReadFile(grantsPolicyFile)
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("run from the repository, config/policy/grants_policy.yaml is missing: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	vap := &admissionregistrationv1.ValidatingAdmissionPolicy{}
	if err := yaml.Unmarshal(raw, vap); err != nil {
		t.Fatalf("parse %s: %v", grantsPolicyFile, err)
	}
	if len(vap.Spec.Validations) == 0 {
		t.Fatalf("%s has no validations", grantsPolicyFile)
	}
	return vap
}

// policyVariable returns the expression of the policy's variable name.
func policyVariable(t *testing.T, vap *admissionregistrationv1.ValidatingAdmissionPolicy, name string) string {
	t.Helper()
	i := slices.IndexFunc(vap.Spec.Variables, func(v admissionregistrationv1.Variable) bool { return v.Name == name })
	if i < 0 {
		t.Fatalf("%s has no variable %s", grantsPolicyFile, name)
	}
	return vap.Spec.Variables[i].Expression
}

// policyValidation returns the validation whose expression reads variable.
func policyValidation(t *testing.T, vap *admissionregistrationv1.ValidatingAdmissionPolicy, variable string) admissionregistrationv1.Validation {
	t.Helper()
	i := slices.IndexFunc(vap.Spec.Validations, func(v admissionregistrationv1.Validation) bool {
		return strings.Contains(v.Expression, variable)
	})
	if i < 0 {
		t.Fatalf("%s has no validation of %s", grantsPolicyFile, variable)
	}
	return vap.Spec.Validations[i]
}

// celStringList parses a CEL list literal of single-quoted strings, the
// only shape the policy's list variables take. A trailing comma is allowed,
// as CEL allows it. A CEL library would be a dependency for this one test.
func celStringList(expr string) ([]string, error) {
	body, ok := strings.CutPrefix(strings.TrimSpace(expr), "[")
	if ok {
		body, ok = strings.CutSuffix(body, "]")
	}
	if !ok {
		return nil, fmt.Errorf("%q is not a CEL list literal", expr)
	}
	items := strings.Split(body, ",")
	if last := len(items) - 1; strings.TrimSpace(items[last]) == "" {
		items = items[:last]
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if len(item) < 2 || item[0] != '\'' || item[len(item)-1] != '\'' {
			return nil, fmt.Errorf("item %q of %q is not a single-quoted string", item, expr)
		}
		out = append(out, item[1:len(item)-1])
	}
	return out, nil
}
