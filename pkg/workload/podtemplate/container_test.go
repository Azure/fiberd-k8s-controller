package podtemplate

import (
	"flag"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/Azure/fiberd/pkg/endpoint"
	"github.com/Azure/fiberd/pkg/fiberd"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

// wantProcArgs is what the builder emits for testGrant with testOptions.
func wantProcArgs() []string {
	return []string{
		"-home=kubernetes",
		"-verifier=jwks",
		"-issuer=" + testIssuer,
		"-jwks-max-stale=10m0s",
		"-stale-ttl=" + DefaultStaleTTL.String(),
		"-listen=:8484",
		"-runtime=proc",
		"-node-id=$(POD_NAME)",
		"-state=/var/lib/fiberd",
		"-grants-dir=/var/run/fiberd/grants",
		"-grant-token-file=/var/run/secrets/fiberd.io/grant/token",
		"-grant-fetch-timeout=1m0s",
		"-endpoint-family=inet4",
		"-tls-cert=/var/run/fiberd/tls/tls.crt",
		"-tls-key=/var/run/fiberd/tls/tls.key",
		"-client-ca=/var/run/fiberd/tls/ca.crt",
		"-template=" + testDigest + "=/app/zygote --port=8080",
	}
}

// withArg returns args with the item that starts with prefix replaced by
// arg. It returns args unchanged when no item matches.
func withArg(args []string, prefix, arg string) []string {
	out := slices.Clone(args)
	for i := range out {
		if strings.HasPrefix(out[i], prefix) {
			out[i] = arg
		}
	}
	return out
}

func TestBuildArgs(t *testing.T) {
	cases := []struct {
		name string
		mods []func(*v1alpha1.CapacityGrant)
		opts func(*Options)
		want []string
	}{
		{
			name: "proc",
			want: wantProcArgs(),
		},
		{
			name: "unset runtime is proc",
			mods: []func(*v1alpha1.CapacityGrant){func(cg *v1alpha1.CapacityGrant) { cg.Spec.Runtime = "" }},
			want: wantProcArgs(),
		},
		{
			name: "runc adds its rootfs",
			mods: []func(*v1alpha1.CapacityGrant){func(cg *v1alpha1.CapacityGrant) { cg.Spec.Runtime = v1alpha1.RuntimeRunc }},
			want: append(withArg(wantProcArgs(), "-runtime=", "-runtime=runc"), "-runc-rootfs=/usr/share/fiberd/rootfs"),
		},
		{
			name: "gvisor adds its rootfs",
			mods: []func(*v1alpha1.CapacityGrant){func(cg *v1alpha1.CapacityGrant) { cg.Spec.Runtime = v1alpha1.RuntimeGvisor }},
			want: append(withArg(wantProcArgs(), "-runtime=", "-runtime=gvisor"), "-gvisor-rootfs=/usr/share/fiberd/rootfs"),
		},
		{
			name: "hyperlight adds its helper",
			mods: []func(*v1alpha1.CapacityGrant){func(cg *v1alpha1.CapacityGrant) { setRuntime(cg, v1alpha1.RuntimeHyperlight) }},
			want: append(withArg(wantProcArgs(), "-runtime=", "-runtime=hyperlight"), "-hyperlight-helper=/var/run/fiberd/helper/helper"),
		},
		{
			name: "options reach their flags",
			opts: func(o *Options) {
				o.Lease = time.Hour
				o.StaleTTL = 45 * time.Second
				o.EndpointFamily = endpoint.Inet6
			},
			want: withArg(withArg(withArg(wantProcArgs(),
				"-jwks-max-stale=", "-jwks-max-stale=1h0m0s"),
				"-stale-ttl=", "-stale-ttl=45s"),
				"-endpoint-family=", "-endpoint-family=inet6"),
		},
		{
			name: "tenant args come first",
			mods: []func(*v1alpha1.CapacityGrant){func(cg *v1alpha1.CapacityGrant) {
				container(cg).Args = []string{"-delta-registry=example.com/deltas", "--gvisor-templates-on-disk"}
			}},
			want: append([]string{"-delta-registry=example.com/deltas", "--gvisor-templates-on-disk"}, wantProcArgs()...),
		},
		{
			name: "a registry zygote replaces the template",
			mods: []func(*v1alpha1.CapacityGrant){func(cg *v1alpha1.CapacityGrant) {
				cg.Spec.Zygote = v1alpha1.ZygoteSpec{Digest: testDigest, Registry: "ghcr.io/team-a/zygotes"}
			}},
			want: withArg(wantProcArgs(), "-template=", "-registry=ghcr.io/team-a/zygotes"),
		},
		{
			name: "a command zygote may share the seal key for its deltas",
			mods: []func(*v1alpha1.CapacityGrant){func(cg *v1alpha1.CapacityGrant) { cg.Spec.Zygote.SealKeySecretName = "team-a-seal" }},
			want: append(wantProcArgs(),
				"-delta-seal-key=/var/run/secrets/fiberd.io/seal-key/seal-key.json",
				"-fiber-hide=/var/run/secrets/fiberd.io/seal-key"),
		},
		{
			name: "a seal key adds its file and hides its directory",
			mods: []func(*v1alpha1.CapacityGrant){func(cg *v1alpha1.CapacityGrant) {
				cg.Spec.Zygote = v1alpha1.ZygoteSpec{Digest: testDigest, Registry: "ghcr.io/team-a/zygotes", SealKeySecretName: "team-a-seal"}
			}},
			want: append(withArg(wantProcArgs(), "-template=", "-registry=ghcr.io/team-a/zygotes"),
				"-delta-seal-key=/var/run/secrets/fiberd.io/seal-key/seal-key.json",
				"-fiber-hide=/var/run/secrets/fiberd.io/seal-key"),
		},
		{
			name: "a hyperlight registry zygote pulls beside the helper",
			mods: []func(*v1alpha1.CapacityGrant){func(cg *v1alpha1.CapacityGrant) {
				setRuntime(cg, v1alpha1.RuntimeHyperlight)
				cg.Spec.Zygote = v1alpha1.ZygoteSpec{Digest: testDigest, Registry: "ghcr.io/team-a/zygotes", SealKeySecretName: "team-a-seal"}
			}},
			want: append(withArg(withArg(wantProcArgs(),
				"-runtime=", "-runtime=hyperlight"),
				"-template=", "-registry=ghcr.io/team-a/zygotes"),
				"-delta-seal-key=/var/run/secrets/fiberd.io/seal-key/seal-key.json",
				"-fiber-hide=/var/run/secrets/fiberd.io/seal-key",
				"-hyperlight-helper=/var/run/fiberd/helper/helper"),
		},
		{
			name: "dollar signs from the grant and options are escaped",
			mods: []func(*v1alpha1.CapacityGrant){func(cg *v1alpha1.CapacityGrant) {
				cg.Spec.Zygote.Command = []string{"/app/zygote", "$(HOME)"}
			}},
			opts: func(o *Options) { o.IssuerURL = "https://issuer$tenant.example" },
			want: withArg(withArg(wantProcArgs(),
				"-issuer=", "-issuer=https://issuer$$tenant.example"),
				"-template=", "-template="+testDigest+"=/app/zygote $$(HOME)"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := testOptions()
			if tc.opts != nil {
				tc.opts(&opts)
			}
			tmpl := mustBuild(t, testGrant(tc.mods...), opts)
			if diff := cmp.Diff(tc.want, tmpl.Spec.Containers[0].Args); diff != "" {
				t.Errorf("args mismatch (-want +got):\n%s", diff)
			}
			if got := tmpl.Spec.Containers[0].Command; got != nil {
				t.Errorf("command = %q, want unset so the image entrypoint runs", got)
			}
		})
	}
}

// expand does what the kubelet does to a container arg. "$$" becomes "$",
// and "$(NAME)" becomes the value of NAME when env defines it.
func expand(s string, env map[string]string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case '$':
			b.WriteByte('$')
			i++
		case '(':
			end := strings.IndexByte(s[i:], ')')
			if end < 0 {
				b.WriteByte(s[i])
				continue
			}
			name := s[i+2 : i+end]
			if v, ok := env[name]; ok {
				b.WriteString(v)
			} else {
				b.WriteString(s[i : i+end+1])
			}
			i += end
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// TestBuildArgsParseInFiberd hands the built args, expanded as the kubelet
// would, to fiberd's own flag set and checks what fiberd ends up with. The
// flag set also binds -home, as fiberd's cmd/fiberd does beside
// Config.Bind.
func TestBuildArgsParseInFiberd(t *testing.T) {
	cases := []struct {
		name         string
		runtime      v1alpha1.Runtime
		command      []string
		tenantArgs   []string
		wantRuntime  string
		wantRunc     string
		wantGvisor   string
		wantHelper   string
		wantGuest    string
		wantTemplate string
		wantRegistry string
		wantDeltas   string
		// registry and sealKey replace the command with a registry zygote.
		registry    string
		sealKey     string
		wantSealKey string
		wantHide    string
	}{
		{
			name:         "proc",
			runtime:      v1alpha1.RuntimeProc,
			command:      []string{"/app/zygote", "--port=8080"},
			wantRuntime:  "proc",
			wantTemplate: "/app/zygote --port=8080",
		},
		{
			name:         "runc",
			runtime:      v1alpha1.RuntimeRunc,
			command:      []string{"/app/zygote"},
			wantRuntime:  "runc",
			wantRunc:     "/usr/share/fiberd/rootfs",
			wantTemplate: "/app/zygote",
		},
		{
			name:         "gvisor",
			runtime:      v1alpha1.RuntimeGvisor,
			command:      []string{"/app/zygote"},
			wantRuntime:  "gvisor",
			wantGvisor:   "/usr/share/fiberd/rootfs",
			wantTemplate: "/app/zygote",
		},
		{
			name:         "hyperlight with its guest",
			runtime:      v1alpha1.RuntimeHyperlight,
			command:      []string{"/app/zygote"},
			tenantArgs:   []string{"-hyperlight-guest=/var/run/fiberd/helper/guest"},
			wantRuntime:  "hyperlight",
			wantHelper:   "/var/run/fiberd/helper/helper",
			wantGuest:    "/var/run/fiberd/helper/guest",
			wantTemplate: "/app/zygote",
		},
		{
			name:         "tenant flag and a literal dollar survive",
			runtime:      v1alpha1.RuntimeProc,
			command:      []string{"/app/zygote", "$(HOME)"},
			tenantArgs:   []string{"--delta-registry=example.com/deltas"},
			wantRuntime:  "proc",
			wantTemplate: "/app/zygote $(HOME)",
			wantDeltas:   "example.com/deltas",
		},
		{
			name:         "registry zygote",
			runtime:      v1alpha1.RuntimeProc,
			registry:     "http://registry.local:5000/zygotes",
			wantRuntime:  "proc",
			wantRegistry: "http://registry.local:5000/zygotes",
		},
		{
			name:         "registry zygote with a seal key",
			runtime:      v1alpha1.RuntimeGvisor,
			registry:     "ghcr.io/team-a/zygotes",
			sealKey:      "team-a-seal",
			wantRuntime:  "gvisor",
			wantGvisor:   "/usr/share/fiberd/rootfs",
			wantRegistry: "ghcr.io/team-a/zygotes",
			wantSealKey:  "/var/run/secrets/fiberd.io/seal-key/seal-key.json",
			wantHide:     "/var/run/secrets/fiberd.io/seal-key",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cg := testGrant(func(cg *v1alpha1.CapacityGrant) {
				setRuntime(cg, tc.runtime)
				cg.Spec.Zygote.Command = tc.command
				cg.Spec.Zygote.Registry = tc.registry
				cg.Spec.Zygote.SealKeySecretName = tc.sealKey
				container(cg).Args = tc.tenantArgs
			})
			tmpl := mustBuild(t, cg, testOptions())

			env := map[string]string{"POD_NAME": "fiberd-web-7d9f8c6b5-x2k4p", "HOME": "/root"}
			var args []string
			for _, a := range tmpl.Spec.Containers[0].Args {
				args = append(args, expand(a, env))
			}
			var cfg fiberd.Config
			fs := flag.NewFlagSet("fiberd", flag.ContinueOnError)
			cfg.Bind(fs)
			home := fs.String("home", "", "the fiberd home, bound by cmd/fiberd")
			tokenFile := fs.String("grant-token-file", "", "the Pod's token, bound by cmd/fiberd")
			fetchTimeout := fs.Duration("grant-fetch-timeout", 0, "the first fetch's bound, bound by cmd/fiberd")
			if err := fs.Parse(args); err != nil {
				t.Fatalf("fiberd refused %q: %v", args, err)
			}
			if fs.NArg() != 0 {
				t.Fatalf("fiberd left positional args %q", fs.Args())
			}

			checks := []struct {
				what      string
				got, want any
			}{
				{"home", *home, "kubernetes"},
				{"verifier", cfg.Trust.Verifier, "jwks"},
				{"issuer", cfg.Trust.Issuer, testIssuer},
				{"jwks-max-stale", cfg.Trust.JWKSMaxStale, testLease},
				{"stale-ttl", cfg.Node.StaleTTL, DefaultStaleTTL},
				{"listen", cfg.API.Listen, ":8484"},
				{"runtime", cfg.Runtime.Name, tc.wantRuntime},
				{"node-id", cfg.Node.ID, "fiberd-web-7d9f8c6b5-x2k4p"},
				{"state", cfg.Node.StateDir, "/var/lib/fiberd"},
				{"grants-dir", cfg.Node.GrantsDir, "/var/run/fiberd/grants"},
				{"grant-token-file", *tokenFile, "/var/run/secrets/fiberd.io/grant/token"},
				{"grant-fetch-timeout", *fetchTimeout, time.Minute},
				{"endpoint-family", cfg.Endpoint.Family, "inet4"},
				{"template", cfg.Runtime.Templates[testDigest], tc.wantTemplate},
				{"runc-rootfs", cfg.Runtime.Runc.Rootfs, tc.wantRunc},
				{"gvisor-rootfs", cfg.Runtime.Gvisor.Rootfs, tc.wantGvisor},
				{"hyperlight-helper", cfg.Runtime.Hyperlight.Helper, tc.wantHelper},
				{"hyperlight-guest", cfg.Runtime.Hyperlight.Guest, tc.wantGuest},
				{"registry", cfg.Registry.Templates, tc.wantRegistry},
				{"delta-registry", cfg.Registry.Deltas, tc.wantDeltas},
				{"delta-seal-key", cfg.Registry.Keys.Seal, tc.wantSealKey},
				{"fiber-hide", strings.Join(cfg.Runtime.FiberHide, ","), tc.wantHide},
				{"tls-cert", cfg.API.TLS.Cert, "/var/run/fiberd/tls/tls.crt"},
				{"tls-key", cfg.API.TLS.Key, "/var/run/fiberd/tls/tls.key"},
				{"client-ca", cfg.API.TLS.ClientCA, "/var/run/fiberd/tls/ca.crt"},
				{"insecure-plaintext", cfg.API.TLS.InsecurePlaintext, false},
			}
			for _, c := range checks {
				if c.got != c.want {
					t.Errorf("fiberd -%s = %v, want %v", c.what, c.got, c.want)
				}
			}
		})
	}
}

func TestBuildProbes(t *testing.T) {
	// 90 seconds of dials. A probe that gave up at the fetch's own 60 seconds
	// would restart a Pod still waiting on the controller.
	startup := &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8484)}},
		PeriodSeconds:    2,
		FailureThreshold: 45,
	}
	// The exec probes wait longer than the five seconds fiberd healthz and
	// fiberd readyz wait, and readyz replaces a TCP check so that a
	// draining fiberd leaves its Service.
	readiness := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
			Command: []string{"/usr/local/bin/fiberd", "readyz", "-state=/var/lib/fiberd"},
		}},
		TimeoutSeconds:   10,
		PeriodSeconds:    5,
		FailureThreshold: 2,
	}
	liveness := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
			Command: []string{"/usr/local/bin/fiberd", "healthz", "-state=/var/lib/fiberd"},
		}},
		TimeoutSeconds:   10,
		PeriodSeconds:    15,
		FailureThreshold: 3,
	}
	// A tenant probe is refused, see TestBuildErrors, so every grant Pod
	// gets the same three probes whatever its runtime.
	cases := []struct {
		name    string
		runtime v1alpha1.Runtime
	}{
		{name: "proc", runtime: v1alpha1.RuntimeProc},
		{name: "runc", runtime: v1alpha1.RuntimeRunc},
		{name: "gvisor", runtime: v1alpha1.RuntimeGvisor},
		{name: "hyperlight", runtime: v1alpha1.RuntimeHyperlight},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cg := testGrant(func(cg *v1alpha1.CapacityGrant) { setRuntime(cg, tc.runtime) })
			c := mustBuild(t, cg, testOptions()).Spec.Containers[0]
			for _, p := range []struct {
				what      string
				got, want *corev1.Probe
			}{
				{"startupProbe", c.StartupProbe, startup},
				{"readinessProbe", c.ReadinessProbe, readiness},
				{"livenessProbe", c.LivenessProbe, liveness},
			} {
				if diff := cmp.Diff(p.want, p.got); diff != "" {
					t.Errorf("%s mismatch (-want +got):\n%s", p.what, diff)
				}
			}
		})
	}
}
