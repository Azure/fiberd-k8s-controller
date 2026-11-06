package podtemplate

import (
	"flag"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Azure/fiberd/pkg/fiberd"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

func TestCheckArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr string // empty means accepted
	}{
		{name: "no args"},
		{name: "empty list", args: []string{}},
		{name: "one-dash flag with a value", args: []string{"-delta-registry=example.com/deltas"}},
		{name: "two-dash flag with a value", args: []string{"--delta-registry=example.com/deltas"}},
		{name: "empty value", args: []string{"-delta-registry="}},
		{name: "value with spaces is one item", args: []string{"-engine=/usr/bin/engine --mode fast"}},
		{name: "value holding an equals sign", args: []string{"-delta-registry=example.com/deltas?tag=a=b"}},
		{name: "bare boolean flag", args: []string{"-gvisor-templates-on-disk"}},
		{name: "two-dash boolean with a value", args: []string{"--gvisor-templates-on-disk=false"}},
		{name: "every tenant flag", args: []string{"-delta-registry=r", "-delta-key=/k", "-delta-trust=/t", "-devices=/dev/x", "-engine-memory-max=1", "-grant-ceiling=1", "-hyperlight-guest=/g", "-parity=off"}},
		{name: "controller flag", args: []string{"-issuer=https://evil.example"}, wantErr: "args[0]: Forbidden: -issuer is not a flag a tenant may set"},
		{name: "controller flag in the two-dash form", args: []string{"--verifier=insecure-json"}, wantErr: "-verifier is not a flag a tenant may set"},
		{name: "controller boolean", args: []string{"-insecure-plaintext"}, wantErr: "-insecure-plaintext is not a flag a tenant may set"},
		{name: "controller boolean with a value", args: []string{"--all-caps=true"}, wantErr: "-all-caps is not a flag a tenant may set"},
		{name: "controller flag later in the list", args: []string{"-delta-registry=r", "-runc-rootfs=/"}, wantErr: "args[1]"},
		{name: "zygote registry", args: []string{"-registry=example.com/zygotes"}, wantErr: "-registry is not a flag a tenant may set"},
		{name: "seal key", args: []string{"--delta-seal-key=/k"}, wantErr: "-delta-seal-key is not a flag a tenant may set"},
		{name: "template for the grant's digest", args: []string{"-template=sha256:" + strings.Repeat("ab", 32) + "=/bin/sh"}, wantErr: "-template is not a flag a tenant may set"},
		{name: "template for the default key", args: []string{"--template=default=/bin/sh"}, wantErr: "-template is not a flag a tenant may set"},
		{name: "operator flag", args: []string{"-userns-pool=0:65536"}, wantErr: "-userns-pool is not a flag a tenant may set"},
		{name: "operator key", args: []string{"-audit-key=/k"}, wantErr: "-audit-key is not a flag a tenant may set"},
		{name: "test hook", args: []string{"-admin-unsafe"}, wantErr: "-admin-unsafe is not a flag a tenant may set"},
		{name: "unknown flag", args: []string{"-no-such-flag=1"}, wantErr: "-no-such-flag is not a flag a tenant may set"},
		{name: "binary flag home", args: []string{"-home=standalone"}, wantErr: "args[0]: Forbidden: -home is not a flag a tenant may set"},
		{name: "binary flag home in the two-dash form", args: []string{"--home=kubernetes"}, wantErr: "-home is not a flag a tenant may set"},
		{name: "binary flag home without a value", args: []string{"--home"}, wantErr: "-home is not a flag a tenant may set"},
		{name: "binary flag pod-name", args: []string{"-pod-name=x"}, wantErr: "args[0]: Forbidden: -pod-name is not a flag a tenant may set"},
		{name: "binary flag pod-uid", args: []string{"-pod-uid=x"}, wantErr: "-pod-uid is not a flag a tenant may set"},
		{name: "binary flag pod-namespace", args: []string{"-pod-namespace=x"}, wantErr: "-pod-namespace is not a flag a tenant may set"},
		{name: "binary boolean version", args: []string{"--version"}, wantErr: "-version is not a flag a tenant may set"},
		{name: "non-boolean flag without a value", args: []string{"-delta-registry"}, wantErr: "must carry its value as -delta-registry=value"},
		{name: "flag and value as two items", args: []string{"-delta-registry", "example.com/deltas"}, wantErr: "args[1]"},
		{name: "flag and value split by a space", args: []string{"-registry example.com/zygotes"}, wantErr: "args[0]: Invalid value"},
		{name: "no dash", args: []string{"registry=example.com"}, wantErr: "args[0]: Invalid value"},
		{name: "three dashes", args: []string{"---registry=example.com"}, wantErr: "args[0]: Invalid value"},
		{name: "upper case", args: []string{"-Registry=example.com"}, wantErr: "args[0]: Invalid value"},
		{name: "starts with a digit", args: []string{"-1registry=x"}, wantErr: "args[0]: Invalid value"},
		{name: "empty item", args: []string{""}, wantErr: "args[0]: Invalid value"},
		{name: "lone dash", args: []string{"-"}, wantErr: "args[0]: Invalid value"},
		{name: "double dash terminator", args: []string{"--"}, wantErr: "args[0]: Invalid value"},
		{name: "value with a newline", args: []string{"-registry=a\nb"}, wantErr: "args[0]: Invalid value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckArgs(tc.args)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("CheckArgs(%q) = %v, want nil", tc.args, err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("CheckArgs(%q) = nil, want an error holding %q", tc.args, tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("CheckArgs(%q) = %q, want it to hold %q", tc.args, err, tc.wantErr)
			}
		})
	}
}

func TestTenantFlags(t *testing.T) {
	want := []string{
		"delta-key", "delta-registry", "delta-trust", "devices", "engine",
		"engine-memory-max", "grant-ceiling", "gvisor-templates-on-disk", "hyperlight-guest",
		"parity",
	}
	cases := []struct {
		name string
		got  func() []string
	}{
		{name: "sorted and complete", got: TenantFlags},
		{
			name: "a caller cannot change the set",
			got: func() []string {
				first := TenantFlags()
				first[0] = "changed"
				return TenantFlags()
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(want, tc.got()); diff != "" {
				t.Errorf("TenantFlags mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// fiberdFlags binds fiberd's real flags to a throwaway flag set.
func fiberdFlags() *flag.FlagSet {
	var cfg fiberd.Config
	fs := flag.NewFlagSet("fiberd", flag.ContinueOnError)
	cfg.Bind(fs)
	return fs
}

// TestTenantFlagsAreFiberdFlags keeps the tenant flags honest. Each must
// be a flag fiberd's Config.Bind registers, so a renamed fiberd flag
// fails here instead of letting a tenant arg through to a fiberd that
// refuses it. The check lives in this test rather than at controller
// startup, since importing pkg/fiberd would pull fiberd's whole runtime
// into the controller binary.
func TestTenantFlagsAreFiberdFlags(t *testing.T) {
	fs := fiberdFlags()
	for _, name := range TenantFlags() {
		t.Run(name, func(t *testing.T) {
			if fs.Lookup(name) == nil {
				t.Errorf("fiberd does not register -%s", name)
			}
		})
	}
}

// TestTenantBoolFlagsMatchFiberd keeps tenantBoolFlags equal to the
// boolean flags among the tenant flags, as fiberd registers them.
func TestTenantBoolFlagsMatchFiberd(t *testing.T) {
	fs := fiberdFlags()
	var fromFiberd []string
	fs.VisitAll(func(f *flag.Flag) {
		b, ok := f.Value.(interface{ IsBoolFlag() bool })
		if ok && b.IsBoolFlag() && tenantFlags[f.Name] {
			fromFiberd = append(fromFiberd, f.Name)
		}
	})
	slices.Sort(fromFiberd)

	cases := []struct {
		name      string
		got, want []string
	}{
		{name: "tenant boolean flags", got: TenantBoolFlags(), want: fromFiberd},
		{name: "every boolean flag is a tenant flag", got: TenantBoolFlags(), want: slices.DeleteFunc(TenantFlags(), func(n string) bool { return !tenantBoolFlags[n] })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, tc.got); diff != "" {
				t.Errorf("tenantBoolFlags differ from fiberd's tenant boolean flags (-fiberd +ours):\n%s", diff)
			}
		})
	}
}

// TestControllerArgsAreNotTenantFlags checks that a tenant can pass none
// of the flags the controller sets, the zygote's included.
func TestControllerArgsAreNotTenantFlags(t *testing.T) {
	cases := []struct {
		name    string
		runtime v1alpha1.Runtime
		zygote  []string
		opts    Options
	}{
		{name: "proc", runtime: v1alpha1.RuntimeProc},
		{name: "runc", runtime: v1alpha1.RuntimeRunc},
		{name: "gvisor", runtime: v1alpha1.RuntimeGvisor},
		{name: "hyperlight", runtime: v1alpha1.RuntimeHyperlight},
		{name: "proc with plaintext", runtime: v1alpha1.RuntimeProc, opts: Options{Plaintext: true}},
		{name: "registry zygote with a seal key", runtime: v1alpha1.RuntimeProc, zygote: []string{"-registry=r", "-delta-seal-key=/k", "-fiber-hide=/d"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			zygote := tc.zygote
			if zygote == nil {
				zygote = []string{"-template=d=/z"}
			}
			for _, arg := range controllerArgs(tc.runtime, zygote, tc.opts.withDefaults()) {
				name, _, _ := strings.Cut(strings.TrimPrefix(arg, "-"), "=")
				if tenantFlags[name] {
					t.Errorf("the controller sets -%s, but CheckArgs lets a tenant set it too", name)
				}
			}
		})
	}
}
