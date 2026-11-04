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
		{name: "one-dash flag with a value", args: []string{"-registry=example.com/zygotes"}},
		{name: "two-dash flag with a value", args: []string{"--registry=example.com/zygotes"}},
		{name: "empty value", args: []string{"-registry="}},
		{name: "value with spaces is one item", args: []string{"-engine=/usr/bin/engine --mode fast"}},
		{name: "value holding an equals sign", args: []string{"-registry=example.com/zygotes?tag=a=b"}},
		{name: "bare boolean flag", args: []string{"-gvisor-templates-on-disk"}},
		{name: "two-dash boolean with a value", args: []string{"--gvisor-templates-on-disk=false"}},
		{name: "owned flag", args: []string{"-issuer=https://evil.example"}, wantErr: "args[0]: Forbidden: -issuer is set by the controller"},
		{name: "owned flag in the two-dash form", args: []string{"--verifier=insecure-json"}, wantErr: "-verifier is set by the controller"},
		{name: "owned boolean", args: []string{"-insecure-plaintext"}, wantErr: "-insecure-plaintext is set by the controller"},
		{name: "owned boolean with a value", args: []string{"--all-caps=true"}, wantErr: "-all-caps is set by the controller"},
		{name: "owned flag later in the list", args: []string{"-registry=r", "-runc-rootfs=/"}, wantErr: "args[1]"},
		{name: "template for the grant's digest", args: []string{"-template=sha256:" + strings.Repeat("ab", 32) + "=/bin/sh"}, wantErr: "-template is set by the controller"},
		{name: "template for the default key", args: []string{"--template=default=/bin/sh"}, wantErr: "-template is set by the controller"},
		{name: "binary flag home", args: []string{"-home=standalone"}, wantErr: "args[0]: Forbidden: -home is set by the controller"},
		{name: "binary flag home in the two-dash form", args: []string{"--home=kubernetes"}, wantErr: "-home is set by the controller"},
		{name: "binary flag home without a value", args: []string{"--home"}, wantErr: "-home is set by the controller"},
		{name: "binary flag pod-name", args: []string{"-pod-name=x"}, wantErr: "args[0]: Forbidden: -pod-name is set by the controller"},
		{name: "binary flag pod-uid", args: []string{"-pod-uid=x"}, wantErr: "-pod-uid is set by the controller"},
		{name: "binary flag pod-namespace", args: []string{"-pod-namespace=x"}, wantErr: "-pod-namespace is set by the controller"},
		{name: "binary boolean version", args: []string{"--version"}, wantErr: "-version is set by the controller"},
		{name: "non-boolean flag without a value", args: []string{"-registry"}, wantErr: "must carry its value as -registry=value"},
		{name: "flag and value as two items", args: []string{"-registry", "example.com/zygotes"}, wantErr: "args[1]"},
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

func TestOwnedFlags(t *testing.T) {
	want := []string{
		"admin-unsafe", "all-caps", "cgroup-root", "client-ca", "endpoint-family", "endpoint-host",
		"engine-uid", "fiber-hide", "grants-dir", "gvisor-rootfs", "handoff-key", "handoff-listen",
		"home", "http", "insecure-plaintext", "issuer", "jwks-max-stale", "listen", "max-lease", "node-id",
		"pod-name", "pod-namespace", "pod-uid", "runc-rootfs", "runtime", "stale-ttl", "state",
		"template", "tls-cert", "tls-key", "verifier", "version",
	}
	cases := []struct {
		name string
		got  func() []string
	}{
		{name: "sorted and complete", got: OwnedFlags},
		{
			name: "a caller cannot change the set",
			got: func() []string {
				first := OwnedFlags()
				first[0] = "changed"
				return OwnedFlags()
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(want, tc.got()); diff != "" {
				t.Errorf("OwnedFlags mismatch (-want +got):\n%s", diff)
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

// testHooksBuilt reports whether fiberd was built with the
// fiberd_testhooks tag, which registers admin-unsafe.
func testHooksBuilt(fs *flag.FlagSet) bool { return fs.Lookup("status-interval") != nil }

// TestOwnedFlagsAreFiberdFlags keeps the owned fiberd flags honest. Each
// must be a flag fiberd's Config.Bind registers, so a renamed fiberd flag
// fails here instead of slipping past CheckArgs. Each binary flag, -home
// and the Kubernetes home's flags, must not be one, or fiberd's cmd/fiberd
// would bind the same name twice. The check lives in
// this test rather than at controller startup, since importing pkg/fiberd
// would pull fiberd's whole runtime into the controller binary.
func TestOwnedFlagsAreFiberdFlags(t *testing.T) {
	fs := fiberdFlags()
	type flagCase struct {
		name       string
		wantFiberd bool
	}
	cases := make([]flagCase, 0, len(ownedFiberdFlags)+len(binaryFlags))
	for _, name := range ownedFiberdFlags {
		cases = append(cases, flagCase{name: name, wantFiberd: true})
	}
	for _, name := range binaryFlags {
		cases = append(cases, flagCase{name: name})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "admin-unsafe" && !testHooksBuilt(fs) {
				t.Skip("fiberd registers -admin-unsafe only with the fiberd_testhooks tag")
			}
			if got := fs.Lookup(tc.name) != nil; got != tc.wantFiberd {
				t.Errorf("fiberd registers -%s = %v, want %v", tc.name, got, tc.wantFiberd)
			}
		})
	}
}

// TestTenantBoolFlagsMatchFiberd keeps tenantBoolFlags equal to the
// boolean flags fiberd registers that a tenant may set.
func TestTenantBoolFlagsMatchFiberd(t *testing.T) {
	fs := fiberdFlags()
	var fromFiberd []string
	fs.VisitAll(func(f *flag.Flag) {
		b, ok := f.Value.(interface{ IsBoolFlag() bool })
		if ok && b.IsBoolFlag() && !ownedFlags[f.Name] {
			fromFiberd = append(fromFiberd, f.Name)
		}
	})
	ours := make([]string, 0, len(tenantBoolFlags))
	for name := range tenantBoolFlags {
		ours = append(ours, name)
	}
	slices.Sort(fromFiberd)
	slices.Sort(ours)

	cases := []struct {
		name      string
		got, want []string
	}{
		{name: "tenant boolean flags", got: ours, want: fromFiberd},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, tc.got); diff != "" {
				t.Errorf("tenantBoolFlags differ from fiberd's tenant boolean flags (-fiberd +ours):\n%s", diff)
			}
		})
	}
}

// TestControllerArgsAreOwned checks that a tenant can pass none of the
// flags the controller sets, -template included.
func TestControllerArgsAreOwned(t *testing.T) {
	cases := []struct {
		name    string
		runtime v1alpha1.Runtime
		opts    Options
	}{
		{name: "proc", runtime: v1alpha1.RuntimeProc},
		{name: "runc", runtime: v1alpha1.RuntimeRunc},
		{name: "gvisor", runtime: v1alpha1.RuntimeGvisor},
		{name: "hyperlight", runtime: v1alpha1.RuntimeHyperlight},
		{name: "proc with plaintext", runtime: v1alpha1.RuntimeProc, opts: Options{Plaintext: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, arg := range controllerArgs(tc.runtime, "d=/z", tc.opts.withDefaults()) {
				name, _, _ := strings.Cut(strings.TrimPrefix(arg, "-"), "=")
				if !ownedFlags[name] {
					t.Errorf("the controller sets -%s, but CheckArgs lets a tenant set it too", name)
				}
			}
		})
	}
}
