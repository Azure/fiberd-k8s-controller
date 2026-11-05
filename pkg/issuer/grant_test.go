package issuer_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Azure/fiberd/pkg/core"
	"github.com/Azure/fiberd/pkg/grant"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/pkg/issuer"
)

const (
	testNamespace = "team-a"
	testPodUID    = "3f2a6c1e-8b9d-4e5f-a1b2-c3d4e5f6a7b8"
	testPodName   = "fiberd-web-7d9f8c6b5-x2k4p"

	testIssuerHost = "fiberd-k8s-controller.fiberd-system.svc"
	testIssuerURL  = "https://" + testIssuerHost
)

var (
	testDigest     = "sha256:" + strings.Repeat("ab", 32)
	testThumbprint = strings.Repeat("Q", 43)
	// testNow has no monotonic reading and no sub-second part, so it
	// survives the JWT round trip unchanged.
	testNow = time.Date(2026, time.October, 31, 12, 0, 0, 0, time.UTC)
	testPod = issuer.PodIdentity{UID: testPodUID, Name: testPodName}
)

// validGrant is a CapacityGrant that maps without error.
func validGrant() *v1alpha1.CapacityGrant {
	w := resource.MustParse("512Mi")
	return &v1alpha1.CapacityGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: testNamespace},
		Spec: v1alpha1.CapacityGrantSpec{
			Zygote:           v1alpha1.ZygoteSpec{Digest: testDigest, Command: []string{"/app/server"}},
			Fibers:           v1alpha1.FiberBudget{Max: 8, Warm: 2},
			WriteBudget:      &w,
			CallerThumbprint: testThumbprint,
			Runtime:          v1alpha1.RuntimeGvisor,
		},
	}
}

// validCore is the grant validGrant maps to for testPod, testNow and an
// hour of lease.
func validCore() core.Grant {
	return core.Grant{
		UID:            testPodUID,
		Audience:       testPodName,
		TemplateDigest: testDigest,
		Tenant:         testNamespace,
		FiberMax:       8,
		FiberWarm:      2,
		WBudgetBytes:   512 << 20,
		MinTier:        core.TierBasic,
		LeaseExpiry:    testNow.Add(time.Hour),
		Policy: core.Policy{
			Durability:   core.BestEffort,
			Isolation:    core.Untrusted,
			EndpointMode: core.EndpointDirect,
		},
		CallerThumbprint: testThumbprint,
	}
}

func quantity(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

func TestGrantFromSpec(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*v1alpha1.CapacityGrant)
		pod    issuer.PodIdentity
		// now is the mapping time. Zero means testNow.
		now   time.Time
		lease time.Duration
		want  func(*core.Grant)
	}{
		{
			name: "full spec maps field by field",
		},
		{
			name:   "gvisor is untrusted",
			mutate: func(cg *v1alpha1.CapacityGrant) { cg.Spec.Runtime = v1alpha1.RuntimeGvisor },
			want:   func(g *core.Grant) { g.Policy.Isolation = core.Untrusted },
		},
		{
			name:   "hyperlight is untrusted",
			mutate: func(cg *v1alpha1.CapacityGrant) { cg.Spec.Runtime = v1alpha1.RuntimeHyperlight },
			want:   func(g *core.Grant) { g.Policy.Isolation = core.Untrusted },
		},
		{
			name:   "proc is trusted",
			mutate: func(cg *v1alpha1.CapacityGrant) { cg.Spec.Runtime = v1alpha1.RuntimeProc },
			want:   func(g *core.Grant) { g.Policy.Isolation = core.Trusted },
		},
		{
			name:   "runc is trusted",
			mutate: func(cg *v1alpha1.CapacityGrant) { cg.Spec.Runtime = v1alpha1.RuntimeRunc },
			want:   func(g *core.Grant) { g.Policy.Isolation = core.Trusted },
		},
		{
			name:   "max and warm carried",
			mutate: func(cg *v1alpha1.CapacityGrant) { cg.Spec.Fibers = v1alpha1.FiberBudget{Max: 64, Warm: 64} },
			want:   func(g *core.Grant) { g.FiberMax, g.FiberWarm = 64, 64 },
		},
		{
			name:   "zero warm carried",
			mutate: func(cg *v1alpha1.CapacityGrant) { cg.Spec.Fibers = v1alpha1.FiberBudget{Max: 1} },
			want:   func(g *core.Grant) { g.FiberMax, g.FiberWarm = 1, 0 },
		},
		{
			name:   "thumbprint carried as given",
			mutate: func(cg *v1alpha1.CapacityGrant) { cg.Spec.CallerThumbprint = "abc-_XYZ" + strings.Repeat("0", 35) },
			want:   func(g *core.Grant) { g.CallerThumbprint = "abc-_XYZ" + strings.Repeat("0", 35) },
		},
		{
			name:   "no thumbprint",
			mutate: func(cg *v1alpha1.CapacityGrant) { cg.Spec.CallerThumbprint = "" },
			want:   func(g *core.Grant) { g.CallerThumbprint = "" },
		},
		{
			name:   "nil writeBudget is unlimited",
			mutate: func(cg *v1alpha1.CapacityGrant) { cg.Spec.WriteBudget = nil },
			want:   func(g *core.Grant) { g.WBudgetBytes = 0 },
		},
		{
			name:   "zero writeBudget is unlimited",
			mutate: func(cg *v1alpha1.CapacityGrant) { cg.Spec.WriteBudget = quantity("0") },
			want:   func(g *core.Grant) { g.WBudgetBytes = 0 },
		},
		{
			name:   "fractional suffix that is whole bytes",
			mutate: func(cg *v1alpha1.CapacityGrant) { cg.Spec.WriteBudget = quantity("1.5Ki") },
			want:   func(g *core.Grant) { g.WBudgetBytes = 1536 },
		},
		{
			name:   "decimal suffix",
			mutate: func(cg *v1alpha1.CapacityGrant) { cg.Spec.WriteBudget = quantity("2G") },
			want:   func(g *core.Grant) { g.WBudgetBytes = 2_000_000_000 },
		},
		{
			// 4 * 1024 * 1Ti is 2^52, well inside the 64 bits fiberd
			// multiplies in.
			name: "large but legal max and writeBudget",
			mutate: func(cg *v1alpha1.CapacityGrant) {
				cg.Spec.Fibers = v1alpha1.FiberBudget{Max: 1024, Warm: 2}
				cg.Spec.WriteBudget = quantity("1Ti")
			},
			want: func(g *core.Grant) { g.FiberMax, g.WBudgetBytes = 1024, 1<<40 },
		},
		{
			// 4 * (2^62 - 1) is the largest product below 2^64.
			name: "largest writeBudget for one fiber",
			mutate: func(cg *v1alpha1.CapacityGrant) {
				cg.Spec.Fibers = v1alpha1.FiberBudget{Max: 1}
				cg.Spec.WriteBudget = quantity("4611686018427387903")
			},
			want: func(g *core.Grant) { g.FiberMax, g.FiberWarm, g.WBudgetBytes = 1, 0, 1<<62-1 },
		},
		{
			name:  "whole second lease",
			lease: 2 * time.Second,
			want:  func(g *core.Grant) { g.LeaseExpiry = testNow.Add(2 * time.Second) },
		},
		{
			name:  "lease sets the expiry",
			lease: 90 * time.Second,
			want:  func(g *core.Grant) { g.LeaseExpiry = testNow.Add(90 * time.Second) },
		},
		{
			// The token's exp claim holds whole seconds, so the expiry is
			// cut to one rather than refused by core.Grant.Validate.
			name: "expiry is whole seconds when now has nanoseconds",
			now:  testNow.Add(700*time.Millisecond + 123*time.Nanosecond),
			want: func(g *core.Grant) { g.LeaseExpiry = testNow.Add(time.Hour) },
		},
		{
			name: "pod identity carried",
			pod:  issuer.PodIdentity{UID: "0", Name: "p"},
			want: func(g *core.Grant) { g.UID, g.Audience = "0", "p" },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cg := validGrant()
			if tc.mutate != nil {
				tc.mutate(cg)
			}
			before := cg.DeepCopy()
			pod := tc.pod
			if pod == (issuer.PodIdentity{}) {
				pod = testPod
			}
			now := tc.now
			if now.IsZero() {
				now = testNow
			}
			lease := tc.lease
			if lease == 0 {
				lease = time.Hour
			}
			want := validCore()
			if tc.want != nil {
				tc.want(&want)
			}

			got, err := issuer.GrantFromSpec(cg, pod, now, lease)
			if err != nil {
				t.Fatalf("GrantFromSpec: %v", err)
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("GrantFromSpec mismatch (-want +got):\n%s", diff)
			}
			if got.LeaseExpiry.Nanosecond() != 0 {
				t.Errorf("LeaseExpiry %s is not a whole number of seconds", got.LeaseExpiry.Format(time.RFC3339Nano))
			}
			// reflect.DeepEqual rather than cmp.Diff, because cmp compares
			// a Quantity with its Equal method and would miss the string a
			// Quantity caches inside itself.
			if !reflect.DeepEqual(cg, before) {
				t.Errorf("GrantFromSpec modified the CapacityGrant\n got %+v\nwant %+v", cg, before)
			}
		})
	}
}

// TestGrantFromSpecMappingErrors covers what GrantFromSpec refuses before
// it asks core.Grant.Validate, because the mapping itself needs it.
func TestGrantFromSpecMappingErrors(t *testing.T) {
	// lease is parsed with time.ParseDuration, and empty means an hour.
	cases := []struct {
		name     string
		mutate   func(*v1alpha1.CapacityGrant)
		nilGrant bool
		pod      issuer.PodIdentity
		lease    string
		// wantErr lists substrings of the error, one per field at fault.
		wantErr []string
	}{
		{
			name:     "nil grant",
			nilGrant: true,
			wantErr:  []string{"capacityGrant: Required value"},
		},
		{
			name:    "empty audience",
			pod:     issuer.PodIdentity{UID: testPodUID, Name: ""},
			wantErr: []string{"pod.metadata.name: Required value"},
		},
		{
			name:    "bad audience",
			pod:     issuer.PodIdentity{UID: testPodUID, Name: "Web/0"},
			wantErr: []string{"pod.metadata.name: Invalid value"},
		},
		{
			name:    "empty tenant",
			mutate:  func(cg *v1alpha1.CapacityGrant) { cg.Namespace = "" },
			wantErr: []string{"metadata.namespace: Required value"},
		},
		{
			// core.Grant.Validate allows a zero max and reads it as unlimited.
			name:    "zero max",
			mutate:  func(cg *v1alpha1.CapacityGrant) { cg.Spec.Fibers = v1alpha1.FiberBudget{Max: 0} },
			wantErr: []string{"spec.fibers.max: Invalid value"},
		},
		{
			name:    "negative writeBudget",
			mutate:  func(cg *v1alpha1.CapacityGrant) { cg.Spec.WriteBudget = quantity("-1") },
			wantErr: []string{"spec.writeBudget: Invalid value"},
		},
		{
			name:    "fractional writeBudget",
			mutate:  func(cg *v1alpha1.CapacityGrant) { cg.Spec.WriteBudget = quantity("1.5") },
			wantErr: []string{"spec.writeBudget: Invalid value"},
		},
		{
			name:    "milli writeBudget",
			mutate:  func(cg *v1alpha1.CapacityGrant) { cg.Spec.WriteBudget = quantity("100m") },
			wantErr: []string{"spec.writeBudget: Invalid value"},
		},
		{
			// Binary suffixes are clamped to int64 at parse time, decimal
			// ones are not.
			name:    "writeBudget beyond int64",
			mutate:  func(cg *v1alpha1.CapacityGrant) { cg.Spec.WriteBudget = quantity("12E") },
			wantErr: []string{"spec.writeBudget: Invalid value"},
		},
		{
			// ParseQuantity clamps 16Ei to MaxInt64 without an error. The
			// budget is refused instead of carried as MaxInt64 bytes.
			name:    "writeBudget 16Ei is refused rather than clamped",
			mutate:  func(cg *v1alpha1.CapacityGrant) { cg.Spec.WriteBudget = quantity("16Ei") },
			wantErr: []string{"spec.writeBudget: Invalid value"},
		},
		{
			name:    "unknown runtime",
			mutate:  func(cg *v1alpha1.CapacityGrant) { cg.Spec.Runtime = "kata" },
			wantErr: []string{"spec.runtime: Unsupported value"},
		},
		{
			name:    "unset runtime",
			mutate:  func(cg *v1alpha1.CapacityGrant) { cg.Spec.Runtime = "" },
			wantErr: []string{"spec.runtime: Required value"},
		},
		{
			name:    "zero lease",
			lease:   "0s",
			wantErr: []string{"lease: Invalid value"},
		},
		{
			name:    "negative lease",
			lease:   "-1m",
			wantErr: []string{"lease: Invalid value"},
		},
		{
			// fiberd cuts iat and exp to whole seconds, so 1.5s can read
			// as 2s and exceed a MaxLease of 1.5s.
			name:    "fractional lease",
			lease:   "1.5s",
			wantErr: []string{`lease: Invalid value: "1.5s": must be a whole number of seconds`},
		},
		{
			name: "every mapping fault is reported",
			pod:  issuer.PodIdentity{UID: testPodUID, Name: "Web/0"},
			mutate: func(cg *v1alpha1.CapacityGrant) {
				cg.Namespace = ""
				cg.Spec.Fibers = v1alpha1.FiberBudget{Max: 0}
				cg.Spec.WriteBudget = quantity("-1")
				cg.Spec.Runtime = "kata"
			},
			lease: "0s",
			wantErr: []string{
				"pod.metadata.name: Invalid value",
				"metadata.namespace: Required value",
				"spec.fibers.max: Invalid value",
				"spec.writeBudget: Invalid value",
				"spec.runtime: Unsupported value",
				"lease: Invalid value",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cg *v1alpha1.CapacityGrant
			if !tc.nilGrant {
				cg = validGrant()
				if tc.mutate != nil {
					tc.mutate(cg)
				}
			}
			pod := tc.pod
			if pod == (issuer.PodIdentity{}) {
				pod = testPod
			}
			lease := time.Hour
			if tc.lease != "" {
				var err error
				if lease, err = time.ParseDuration(tc.lease); err != nil {
					t.Fatalf("lease %q: %v", tc.lease, err)
				}
			}

			got, err := issuer.GrantFromSpec(cg, pod, testNow, lease)
			if err == nil {
				t.Fatalf("GrantFromSpec = %+v, want an error", got)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("GrantFromSpec error %q does not contain %q", err, want)
				}
			}
			if cg != nil {
				ref := "CapacityGrant " + cg.Namespace + "/" + cg.Name
				if !strings.Contains(err.Error(), ref) {
					t.Errorf("GrantFromSpec error %q does not name %q", err, ref)
				}
			}
			if diff := cmp.Diff(core.Grant{}, got); diff != "" {
				t.Errorf("GrantFromSpec returned a grant with an error, want the zero grant (-want +got):\n%s", diff)
			}
		})
	}
}

// TestGrantFromSpecCoreRefusal proves that GrantFromSpec leaves the
// grant's own rules to core.Grant.Validate and returns its error. Each
// case says which grant core sees, and the error must carry what core
// says of that grant.
func TestGrantFromSpecCoreRefusal(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*v1alpha1.CapacityGrant)
		pod    issuer.PodIdentity
		// core changes validCore into the grant core.Grant.Validate sees.
		core func(*core.Grant)
	}{
		{
			name: "pod UID that is not a DNS-1123 label",
			pod:  issuer.PodIdentity{UID: "Pod_UID", Name: testPodName},
			core: func(g *core.Grant) { g.UID = "Pod_UID" },
		},
		{
			name: "every fault core owns is reported",
			pod:  issuer.PodIdentity{UID: strings.Repeat("a", 64), Name: testPodName},
			mutate: func(cg *v1alpha1.CapacityGrant) {
				cg.Namespace = "team/a"
				cg.Spec.Zygote.Digest = "sha256:a=b"
				cg.Spec.Fibers = v1alpha1.FiberBudget{Max: 4, Warm: 5}
				cg.Spec.WriteBudget = quantity("4Ei")
				cg.Spec.CallerThumbprint = "short"
			},
			core: func(g *core.Grant) {
				g.UID = strings.Repeat("a", 64)
				g.Tenant = "team/a"
				g.TemplateDigest = "sha256:a=b"
				g.FiberMax, g.FiberWarm = 4, 5
				g.WBudgetBytes = 4 << 60
				g.CallerThumbprint = "short"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cg := validGrant()
			if tc.mutate != nil {
				tc.mutate(cg)
			}
			seen := validCore()
			tc.core(&seen)
			coreErr := seen.Validate()
			if coreErr == nil {
				t.Fatalf("core.Grant.Validate accepts %+v, the case must build a grant it refuses", seen)
			}

			got, err := issuer.GrantFromSpec(cg, tc.pod, testNow, time.Hour)
			if err == nil {
				t.Fatalf("GrantFromSpec = %+v, want core's error %q", got, coreErr)
			}
			if !strings.Contains(err.Error(), coreErr.Error()) {
				t.Errorf("GrantFromSpec error %q does not carry core's error %q", err, coreErr)
			}
			ref := "CapacityGrant " + cg.Namespace + "/" + cg.Name
			if !strings.Contains(err.Error(), ref) {
				t.Errorf("GrantFromSpec error %q does not name %q", err, ref)
			}
			if diff := cmp.Diff(core.Grant{}, got); diff != "" {
				t.Errorf("GrantFromSpec returned a grant with an error, want the zero grant (-want +got):\n%s", diff)
			}
		})
	}
}

// TestGrantRoundTrip proves fiberd accepts what GrantFromSpec builds. The
// grant is minted by fiberd's own Issuer and checked by its own Verify.
func TestGrantRoundTrip(t *testing.T) {
	cases := []struct {
		name   string
		alg    jose.SignatureAlgorithm
		mutate func(*v1alpha1.CapacityGrant)
		lease  time.Duration
	}{
		{
			name:  "gvisor with thumbprint and budget",
			alg:   jose.EdDSA,
			lease: time.Hour,
		},
		{
			name: "proc without thumbprint or budget",
			alg:  jose.EdDSA,
			mutate: func(cg *v1alpha1.CapacityGrant) {
				cg.Spec.Runtime = v1alpha1.RuntimeProc
				cg.Spec.CallerThumbprint = ""
				cg.Spec.WriteBudget = nil
				cg.Spec.Fibers = v1alpha1.FiberBudget{Max: 1}
			},
			lease: 5 * time.Minute,
		},
		{
			name:   "hyperlight signed with ES256",
			alg:    jose.ES256,
			mutate: func(cg *v1alpha1.CapacityGrant) { cg.Spec.Runtime = v1alpha1.RuntimeHyperlight },
			lease:  24 * time.Hour,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cg := validGrant()
			if tc.mutate != nil {
				tc.mutate(cg)
			}
			g, err := issuer.GrantFromSpec(cg, testPod, testNow, tc.lease)
			if err != nil {
				t.Fatalf("GrantFromSpec: %v", err)
			}
			ring, err := grant.NewKeyRing(tc.alg)
			if err != nil {
				t.Fatalf("NewKeyRing: %v", err)
			}
			iss := &grant.Issuer{Ring: ring, URL: testIssuerURL, Now: func() time.Time { return testNow }}
			token, err := iss.Mint(g)
			if err != nil {
				t.Fatalf("Mint: %v", err)
			}

			set := iss.JWKS()
			if len(set.Keys) != 1 {
				t.Fatalf("issuer publishes %d keys, want 1", len(set.Keys))
			}
			got, err := grant.Verify(token, &set.Keys[0], grant.VerifyOptions{
				Audience: testPodName,
				Issuer:   testIssuerURL,
				MaxLease: tc.lease,
			})
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			want := g
			want.Issuer = testIssuerURL
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("Verify mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
