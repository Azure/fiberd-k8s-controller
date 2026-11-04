package podtemplate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"sigs.k8s.io/yaml"
)

// grantsPolicyFile is the admission policy that the builder's checks
// repeat, seen from this package's directory.
const grantsPolicyFile = "../../../config/policy/grants_policy.yaml"

// TestPolicyMatchesBuilder keeps the admission policy's lists and arg
// pattern equal to the builder's. The two refuse the same grants, one at
// admission and one at reconcile time, and nothing else ties them.
func TestPolicyMatchesBuilder(t *testing.T) {
	raw, err := os.ReadFile(grantsPolicyFile)
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("run from the repository, config/policy/grants_policy.yaml is missing: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	var vap admissionregistrationv1.ValidatingAdmissionPolicy
	if err := yaml.Unmarshal(raw, &vap); err != nil {
		t.Fatalf("parse %s: %v", grantsPolicyFile, err)
	}
	if len(vap.Spec.Validations) == 0 {
		t.Fatalf("%s has no validations", grantsPolicyFile)
	}

	boolFlags := TenantBoolFlags()
	bareArgs := make([]string, 0, 2*len(boolFlags))
	for _, f := range boolFlags {
		bareArgs = append(bareArgs, "-"+f, "--"+f)
	}
	lists := []struct {
		name     string
		variable string
		want     []string
		// anyOrder is set where the policy's list is not sorted.
		anyOrder bool
	}{
		{name: "tenantFlags equals TenantFlags", variable: "tenantFlags", want: TenantFlags()},
		{name: "bareArgs holds both forms of TenantBoolFlags", variable: "bareArgs", want: bareArgs, anyOrder: true},
		{name: "ownedDirs equals ownedDirs", variable: "ownedDirs", want: ownedDirs, anyOrder: true},
		{name: "trustPaths equals trustPaths", variable: "trustPaths", want: trustPaths, anyOrder: true},
		{name: "ownedVolumes equals ownedVolumes", variable: "ownedVolumes", want: ownedVolumes, anyOrder: true},
		{
			name:     "reservedKeyPrefixes equals reservedKeyPrefixes",
			variable: "reservedKeyPrefixes",
			want:     reservedKeyPrefixes,
			anyOrder: true,
		},
		{
			name:     "ownedEnv holds identityEnv, trustEnv and loaderEnv",
			variable: "ownedEnv",
			want:     slices.Concat(identityEnv, trustEnv, loaderEnv),
			anyOrder: true,
		},
	}
	for _, tc := range lists {
		t.Run(tc.name, func(t *testing.T) {
			i := slices.IndexFunc(vap.Spec.Variables, func(v admissionregistrationv1.Variable) bool { return v.Name == tc.variable })
			if i < 0 {
				t.Fatalf("%s has no variable %s", grantsPolicyFile, tc.variable)
			}
			got, err := celStringList(vap.Spec.Variables[i].Expression)
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
