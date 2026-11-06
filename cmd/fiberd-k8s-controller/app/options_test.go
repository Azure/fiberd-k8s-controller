package app

import (
	"flag"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/Azure/fiberd/pkg/endpoint"

	"github.com/Azure/fiberd-k8s-controller/pkg/issuer"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload/podtemplate"
)

const (
	testIssuer   = "https://fiberd-k8s-controller.fiberd-system.svc:8443"
	testCertFile = "/var/run/fiberd/tls/tls.crt"
	testKeyFile  = "/var/run/fiberd/tls/tls.key"
)

// withRequired prepends the flags that have no default to args.
func withRequired(args ...string) []string {
	return append([]string{
		"--issuer-url=" + testIssuer,
		"--tls-cert-file=" + testCertFile,
		"--tls-private-key-file=" + testKeyFile,
	}, args...)
}

// quantityEqual compares quantities by value, since 0.5 and 500m are the
// same CPU.
var quantityEqual = cmp.Comparer(func(a, b resource.Quantity) bool { return a.Cmp(b) == 0 })

// parse registers the flags of NewOptions on a fresh set and parses args.
func parse(args []string) (*Options, error) {
	fs := flag.NewFlagSet("fiberd-k8s-controller", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	o := NewOptions()
	o.AddFlags(fs)
	err := fs.Parse(args)
	return o, err
}

// defaults is what the flags give with only withRequired's flags set and
// $POD_NAMESPACE set to ns.
func defaults(ns string) Options {
	return Options{
		Template: podtemplate.Options{
			IssuerURL:      testIssuer,
			Lease:          time.Hour,
			StaleTTL:       podtemplate.DefaultStaleTTL,
			EndpointFamily: podtemplate.DefaultEndpointFamily,
			DefaultCPU:     resource.MustParse("500m"),
			Plaintext:      true,
			ServiceAccount: podtemplate.DefaultServiceAccount,
		},
		Namespace:   ns,
		LeaderElect: true,
		ProbeAddr:   ":8081",
		IssuerAddr:  ":8443",
		TLSCertFile: testCertFile,
		TLSKeyFile:  testKeyFile,
		Limits:      issuer.RateLimits{PerIP: 50, Global: 100, PodInterval: 10 * time.Second},
	}
}

// unset is defaults(ns) with the flags that have no default left empty, as
// NewOptions returns it.
func unset(ns string) Options {
	o := defaults(ns)
	o.Template.IssuerURL = ""
	o.TLSCertFile = ""
	o.TLSKeyFile = ""
	return o
}

func TestAddFlags(t *testing.T) {
	cases := []struct {
		name         string
		podNamespace string
		args         []string
		want         func() Options
		wantErr      string
	}{
		{
			name:         "defaults take the namespace from POD_NAMESPACE",
			podNamespace: "fiberd-system",
			args:         withRequired(),
			want:         func() Options { return defaults("fiberd-system") },
		},
		{
			name: "no POD_NAMESPACE leaves the namespace empty",
			args: withRequired(),
			want: func() Options { return defaults("") },
		},
		{
			name:         "the namespace flag wins over POD_NAMESPACE",
			podNamespace: "fiberd-system",
			args:         withRequired("--namespace=controllers"),
			want:         func() Options { return defaults("controllers") },
		},
		{
			name: "every flag set",
			args: []string{
				"--issuer-url=https://issuer.example.com",
				"--lease=30m",
				"--stale-ttl=1m",
				"--endpoint-family=inet6",
				"--default-pod-cpu=2",
				"--insecure-plaintext=false",
				"--grant-service-account=grant-pods",
				"--namespace=controllers",
				"--leader-elect=false",
				"--health-probe-bind-address=127.0.0.1:9440",
				"--issuer-bind-address=127.0.0.1:9443",
				"--tls-cert-file=/etc/fiberd/serving.crt",
				"--tls-private-key-file=/etc/fiberd/serving.key",
				"--grant-rate-per-ip=5.5",
				"--grant-rate=20",
				"--grant-pod-interval=1m",
			},
			want: func() Options {
				return Options{
					Template: podtemplate.Options{
						IssuerURL:      "https://issuer.example.com",
						Lease:          30 * time.Minute,
						StaleTTL:       time.Minute,
						EndpointFamily: endpoint.Inet6,
						DefaultCPU:     resource.MustParse("2"),
						ServiceAccount: "grant-pods",
					},
					Namespace:   "controllers",
					ProbeAddr:   "127.0.0.1:9440",
					IssuerAddr:  "127.0.0.1:9443",
					TLSCertFile: "/etc/fiberd/serving.crt",
					TLSKeyFile:  "/etc/fiberd/serving.key",
					Limits:      issuer.RateLimits{PerIP: 5.5, Global: 20, PodInterval: time.Minute},
				}
			},
		},
		{
			name: "the endpoint family is read as fiberd reads it",
			args: withRequired("--endpoint-family=UNIX"),
			want: func() Options {
				o := defaults("")
				o.Template.EndpointFamily = endpoint.Unix
				return o
			},
		},
		{
			name:    "an unknown endpoint family is refused",
			args:    []string{"--endpoint-family=tcp"},
			wantErr: "endpoint-family",
		},
		{
			name:    "an empty endpoint family is refused instead of read as unix",
			args:    []string{"--endpoint-family="},
			wantErr: "endpoint family is empty",
		},
		{
			name:    "a zero CPU is refused instead of kept at the default",
			args:    []string{"--default-pod-cpu=0"},
			wantErr: "not a positive quantity",
		},
		{
			name:    "a negative CPU is refused",
			args:    []string{"--default-pod-cpu=-1"},
			wantErr: "not a positive quantity",
		},
		{
			name:    "a CPU that is not a quantity is refused",
			args:    []string{"--default-pod-cpu=lots"},
			wantErr: "default-pod-cpu",
		},
		{
			name:    "a lease that is not a duration is refused",
			args:    []string{"--lease=hour"},
			wantErr: "lease",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envPodNamespace, tc.podNamespace)
			got, err := parse(tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parse(%q) error = %v, want one naming %q", tc.args, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse(%q): %v", tc.args, err)
			}
			if diff := cmp.Diff(tc.want(), *got, quantityEqual); diff != "" {
				t.Errorf("parse(%q) mismatch (-want +got):\n%s", tc.args, diff)
			}
		})
	}
}

func TestNewOptions(t *testing.T) {
	cases := []struct {
		name         string
		podNamespace string
		want         func() Options
	}{
		{
			name:         "POD_NAMESPACE gives the namespace",
			podNamespace: "fiberd-system",
			want:         func() Options { return unset("fiberd-system") },
		},
		{
			name: "no POD_NAMESPACE leaves the namespace empty",
			want: func() Options { return unset("") },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envPodNamespace, tc.podNamespace)
			if diff := cmp.Diff(tc.want(), *NewOptions(), quantityEqual); diff != "" {
				t.Errorf("NewOptions() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Options)
		wantErr []string
	}{
		{
			name:   "the defaults with an issuer and a namespace are valid",
			mutate: func(*Options) {},
		},
		{
			name:   "an IPv6 probe address is valid",
			mutate: func(o *Options) { o.ProbeAddr = "[::1]:8081" },
		},
		{
			name: "every issuer flag set is valid",
			mutate: func(o *Options) {
				o.Template.IssuerURL = "https://issuer.example.com"
				o.IssuerAddr = "[::1]:9443"
				o.TLSCertFile = "/etc/fiberd/serving.crt"
				o.TLSKeyFile = "/etc/fiberd/serving.key"
			},
		},
		{
			name:    "no issuer URL",
			mutate:  func(o *Options) { o.Template.IssuerURL = "" },
			wantErr: []string{"issuer URL is required"},
		},
		{
			name:    "no namespace",
			mutate:  func(o *Options) { o.Namespace = "" },
			wantErr: []string{"--namespace is required"},
		},
		{
			name:    "a namespace that is not a DNS label",
			mutate:  func(o *Options) { o.Namespace = "Fiberd_System" },
			wantErr: []string{"not a valid namespace name"},
		},
		{
			name:    "a probe address without a port",
			mutate:  func(o *Options) { o.ProbeAddr = "8081" },
			wantErr: []string{"--health-probe-bind-address"},
		},
		{
			name:    "a probe port out of range",
			mutate:  func(o *Options) { o.ProbeAddr = ":70000" },
			wantErr: []string{"--health-probe-bind-address", "not a number from 0 to 65535"},
		},
		{
			name:    "an empty probe address",
			mutate:  func(o *Options) { o.ProbeAddr = "" },
			wantErr: []string{"--health-probe-bind-address"},
		},
		{
			name:    "a zero lease",
			mutate:  func(o *Options) { o.Template.Lease = 0 },
			wantErr: []string{"lease must be positive"},
		},
		{
			name:    "a lease of fractional seconds",
			mutate:  func(o *Options) { o.Template.Lease = 1500 * time.Millisecond },
			wantErr: []string{"lease"},
		},
		{
			name:    "a negative default CPU",
			mutate:  func(o *Options) { o.Template.DefaultCPU = resource.MustParse("-1") },
			wantErr: []string{"default CPU must be positive"},
		},
		{
			name:    "no certificate file",
			mutate:  func(o *Options) { o.TLSCertFile = "" },
			wantErr: []string{"--tls-cert-file is required"},
		},
		{
			name:    "no key file",
			mutate:  func(o *Options) { o.TLSKeyFile = "" },
			wantErr: []string{"--tls-private-key-file is required"},
		},
		{
			name:    "an issuer address without a port",
			mutate:  func(o *Options) { o.IssuerAddr = "8443" },
			wantErr: []string{"--issuer-bind-address"},
		},
		{
			name:    "an issuer port out of range",
			mutate:  func(o *Options) { o.IssuerAddr = ":70000" },
			wantErr: []string{"--issuer-bind-address", "not a number from 0 to 65535"},
		},
		{
			name:    "an empty issuer address",
			mutate:  func(o *Options) { o.IssuerAddr = "" },
			wantErr: []string{"--issuer-bind-address"},
		},
		{
			name:    "an http issuer URL",
			mutate:  func(o *Options) { o.Template.IssuerURL = "http://fiberd-k8s-controller.fiberd-system.svc:8443" },
			wantErr: []string{"absolute https URL"},
		},
		{
			name:    "a zero per-IP rate",
			mutate:  func(o *Options) { o.Limits.PerIP = 0 },
			wantErr: []string{"--grant-rate-per-ip must be a positive number"},
		},
		{
			name:    "a negative global rate",
			mutate:  func(o *Options) { o.Limits.Global = -1 },
			wantErr: []string{"--grant-rate must be a positive number"},
		},
		{
			name:    "a global rate of NaN",
			mutate:  func(o *Options) { o.Limits.Global = math.NaN() },
			wantErr: []string{"--grant-rate must be a positive number"},
		},
		{
			name:    "an infinite per-IP rate",
			mutate:  func(o *Options) { o.Limits.PerIP = math.Inf(1) },
			wantErr: []string{"--grant-rate-per-ip must be a positive number"},
		},
		{
			name:    "a zero Pod interval",
			mutate:  func(o *Options) { o.Limits.PodInterval = 0 },
			wantErr: []string{"--grant-pod-interval must be positive"},
		},
		{
			name: "every problem is reported at once",
			mutate: func(o *Options) {
				o.Template.IssuerURL = ""
				o.Namespace = ""
				o.ProbeAddr = "nowhere"
				o.TLSCertFile = ""
			},
			wantErr: []string{"issuer URL is required", "--namespace is required", "--health-probe-bind-address", "--tls-cert-file is required"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := defaults("fiberd-system")
			tc.mutate(&o)
			err := o.Validate()
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error naming %q", tc.wantErr)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Validate() = %v, want it to name %q", err, want)
				}
			}
		})
	}
}
