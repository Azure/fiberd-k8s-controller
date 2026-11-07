package podtemplate

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/api/resource"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"

	"github.com/Azure/fiberd/pkg/endpoint"
)

func TestOptionsWithDefaults(t *testing.T) {
	cases := []struct {
		name string
		in   Options
		want Options
	}{
		{
			name: "everything unset",
			in:   Options{},
			want: Options{
				StaleTTL:       DefaultStaleTTL,
				EndpointFamily: endpoint.Inet4,
				DefaultCPU:     resource.MustParse("500m"),
				ServiceAccount: "fiberd-grant",
			},
		},
		{
			name: "set values stay",
			in: Options{
				IssuerURL:      testIssuer,
				Lease:          time.Hour,
				StaleTTL:       time.Minute,
				EndpointFamily: endpoint.Inet6,
				DefaultCPU:     resource.MustParse("2"),
				ServiceAccount: "runner",
			},
			want: Options{
				IssuerURL:      testIssuer,
				Lease:          time.Hour,
				StaleTTL:       time.Minute,
				EndpointFamily: endpoint.Inet6,
				DefaultCPU:     resource.MustParse("2"),
				ServiceAccount: "runner",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, tc.in.withDefaults()); diff != "" {
				t.Errorf("withDefaults mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestOptionsValidate checks that Validate reports every problem. Each
// case's wantErrs holds one substring for each problem, in order, and an
// empty wantErrs means the options are valid.
func TestOptionsValidate(t *testing.T) {
	cases := []struct {
		name     string
		opts     func(*Options)
		wantErrs []string
	}{
		{name: "unset fields take valid defaults"},
		{
			name: "every field set",
			opts: func(o *Options) {
				o.StaleTTL = time.Minute
				o.EndpointFamily = endpoint.Unix
				o.DefaultCPU = resource.MustParse("2")
				o.ServiceAccount = "runner"
			},
		},
		{name: "no issuer", opts: func(o *Options) { o.IssuerURL = "" }, wantErrs: []string{"issuer URL is required"}},
		{name: "relative issuer", opts: func(o *Options) { o.IssuerURL = "/issuer" }, wantErrs: []string{"absolute https URL"}},
		{name: "issuer with no scheme", opts: func(o *Options) { o.IssuerURL = "fiberd-k8s-controller.fiberd-system.svc" }, wantErrs: []string{"absolute https URL"}},
		{name: "http issuer", opts: func(o *Options) { o.IssuerURL = "http://fiberd-k8s-controller.fiberd-system.svc" }, wantErrs: []string{"absolute https URL"}},
		{name: "issuer with a space", opts: func(o *Options) { o.IssuerURL = "https://issuer.example/a b" }, wantErrs: []string{"must not contain whitespace"}},
		{name: "issuer with a port", opts: func(o *Options) { o.IssuerURL = testIssuer + ":8443" }},
		{name: "issuer with a path", opts: func(o *Options) { o.IssuerURL = testIssuer + "/issuer" }, wantErrs: []string{"must have no path, query or fragment"}},
		{name: "issuer with a trailing slash", opts: func(o *Options) { o.IssuerURL = testIssuer + "/" }, wantErrs: []string{"must have no path, query or fragment"}},
		{name: "issuer with a query", opts: func(o *Options) { o.IssuerURL = testIssuer + "?tenant=a" }, wantErrs: []string{"must have no path, query or fragment"}},
		{name: "issuer with a fragment", opts: func(o *Options) { o.IssuerURL = testIssuer + "#a" }, wantErrs: []string{"must have no path, query or fragment"}},
		{name: "issuer with a bare question mark", opts: func(o *Options) { o.IssuerURL = testIssuer + "?" }, wantErrs: []string{"must have no path, query or fragment"}},
		{name: "zero lease", opts: func(o *Options) { o.Lease = 0 }, wantErrs: []string{"lease must be positive"}},
		{name: "lease of whole seconds", opts: func(o *Options) { o.Lease = 90 * time.Second }},
		{name: "lease with a fraction of a second", opts: func(o *Options) { o.Lease = time.Minute + 500*time.Millisecond }, wantErrs: []string{"whole number of seconds"}},
		{name: "negative stale TTL", opts: func(o *Options) { o.StaleTTL = -time.Second }, wantErrs: []string{"stale TTL must be positive"}},
		{name: "unknown endpoint family", opts: func(o *Options) { o.EndpointFamily = "inet5" }, wantErrs: []string{"endpoint family"}},
		{name: "negative default CPU", opts: func(o *Options) { o.DefaultCPU = resource.MustParse("-1") }, wantErrs: []string{"default CPU must be positive"}},
		{name: "bad service account", opts: func(o *Options) { o.ServiceAccount = "Not_A_Name" }, wantErrs: []string{"service account"}},
		{
			name: "every problem is reported",
			opts: func(o *Options) {
				*o = Options{
					IssuerURL:      "/issuer",
					Lease:          -time.Minute,
					StaleTTL:       -time.Second,
					EndpointFamily: "inet5",
					DefaultCPU:     resource.MustParse("-1"),
					ServiceAccount: "Not_A_Name",
				}
			},
			wantErrs: []string{
				"absolute https URL",
				"lease must be positive",
				"stale TTL must be positive",
				"endpoint family",
				"default CPU must be positive",
				"service account",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := testOptions()
			if tc.opts != nil {
				tc.opts(&opts)
			}
			err := opts.Validate()
			if len(tc.wantErrs) == 0 {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			var agg utilerrors.Aggregate
			if !errors.As(err, &agg) {
				t.Fatalf("Validate = %v (%T), want a utilerrors.Aggregate", err, err)
			}
			got := agg.Errors()
			if len(got) != len(tc.wantErrs) {
				t.Fatalf("Validate reported %d problems, want %d: %v", len(got), len(tc.wantErrs), err)
			}
			for i, want := range tc.wantErrs {
				if !strings.Contains(got[i].Error(), want) {
					t.Errorf("problem %d = %q, want it to hold %q", i, got[i], want)
				}
			}
		})
	}
}
