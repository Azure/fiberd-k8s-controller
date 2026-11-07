package podtemplate

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

var hashPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// manyLabels gives the template enough labels that Go's map order would
// show if the hash depended on it.
func manyLabels(cg *v1alpha1.CapacityGrant) {
	cg.Spec.PodTemplate.Labels = map[string]string{}
	for _, k := range strings.Split("a b c d e f g h i j k l m n o p", " ") {
		cg.Spec.PodTemplate.Labels[k] = k + "-value"
	}
	cg.Spec.PodTemplate.Spec.NodeSelector = map[string]string{"zone": "a", "pool": "fibers", "arch": "amd64"}
}

// TestHashStable checks that equal grants hash the same, however they were
// built, which is what keeps the hash across controller restarts.
func TestHashStable(t *testing.T) {
	cases := []struct {
		name  string
		clone func(t *testing.T, cg *v1alpha1.CapacityGrant) *v1alpha1.CapacityGrant
	}{
		{
			name:  "same object twice",
			clone: func(_ *testing.T, cg *v1alpha1.CapacityGrant) *v1alpha1.CapacityGrant { return cg },
		},
		{
			name:  "deep copy",
			clone: func(_ *testing.T, cg *v1alpha1.CapacityGrant) *v1alpha1.CapacityGrant { return cg.DeepCopy() },
		},
		{
			name: "read back from JSON as after a restart",
			clone: func(t *testing.T, cg *v1alpha1.CapacityGrant) *v1alpha1.CapacityGrant {
				raw, err := json.Marshal(cg)
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				var out v1alpha1.CapacityGrant
				if err := json.Unmarshal(raw, &out); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				return &out
			},
		},
		{
			name: "maps filled in another order",
			clone: func(_ *testing.T, cg *v1alpha1.CapacityGrant) *v1alpha1.CapacityGrant {
				out := cg.DeepCopy()
				out.Spec.PodTemplate.Labels = map[string]string{}
				keys := strings.Split("a b c d e f g h i j k l m n o p", " ")
				for i := len(keys) - 1; i >= 0; i-- {
					out.Spec.PodTemplate.Labels[keys[i]] = keys[i] + "-value"
				}
				out.Spec.PodTemplate.Spec.NodeSelector = map[string]string{"arch": "amd64", "pool": "fibers", "zone": "a"}
				return out
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cg := testGrant(manyLabels, func(cg *v1alpha1.CapacityGrant) {
				cg.Spec.WriteBudget = ptr.To(resource.MustParse("512Mi"))
			})
			want := mustHash(t, cg, testImage)
			if !hashPattern.MatchString(want) {
				t.Fatalf("Hash = %q, want 16 lower-case hex characters", want)
			}
			if got := mustHash(t, tc.clone(t, cg), testImage); got != want {
				t.Errorf("Hash = %q, want %q", got, want)
			}
		})
	}
}

// TestHashSensitive checks that every input Hash covers changes it, and
// that the grant fields it leaves out do not.
func TestHashSensitive(t *testing.T) {
	cases := []struct {
		name       string
		mod        func(*v1alpha1.CapacityGrant)
		image      string
		wantChange bool
	}{
		{name: "namespace", mod: func(cg *v1alpha1.CapacityGrant) { cg.Namespace = "team-b" }, wantChange: true},
		{name: "zygote digest", mod: func(cg *v1alpha1.CapacityGrant) { cg.Spec.Zygote.Digest = "sha256:" + strings.Repeat("cd", 32) }, wantChange: true},
		{name: "zygote command", mod: func(cg *v1alpha1.CapacityGrant) { cg.Spec.Zygote.Command = []string{"/app/other"} }, wantChange: true},
		{
			name: "zygote registry",
			mod: func(cg *v1alpha1.CapacityGrant) {
				cg.Spec.Zygote = v1alpha1.ZygoteSpec{Digest: testDigest, Registry: "ghcr.io/a/z"}
			},
			wantChange: true,
		},
		{
			name:       "zygote seal key Secret",
			mod:        func(cg *v1alpha1.CapacityGrant) { cg.Spec.Zygote.SealKeySecretName = "k" },
			wantChange: true,
		},
		{name: "fibers max", mod: func(cg *v1alpha1.CapacityGrant) { cg.Spec.Fibers.Max = 8 }, wantChange: true},
		{name: "fibers warm", mod: func(cg *v1alpha1.CapacityGrant) { cg.Spec.Fibers.Warm = 2 }, wantChange: true},
		{
			name:       "writeBudget set",
			mod:        func(cg *v1alpha1.CapacityGrant) { cg.Spec.WriteBudget = ptr.To(resource.MustParse("256Mi")) },
			wantChange: true,
		},
		{
			name:       "writeBudget zero differs from unset",
			mod:        func(cg *v1alpha1.CapacityGrant) { cg.Spec.WriteBudget = ptr.To(resource.MustParse("0")) },
			wantChange: true,
		},
		{name: "caller thumbprint", mod: func(cg *v1alpha1.CapacityGrant) { cg.Spec.CallerThumbprint = strings.Repeat("Q", 43) }, wantChange: true},
		{name: "runtime", mod: func(cg *v1alpha1.CapacityGrant) { cg.Spec.Runtime = v1alpha1.RuntimeRunc }, wantChange: true},
		{name: "image", image: "ghcr.io/azure/fiberd:v0.5.0", wantChange: true},
		{name: "helper image", mod: func(cg *v1alpha1.CapacityGrant) { cg.Spec.HelperImage = testHelperImage }, wantChange: true},
		{name: "template label", mod: func(cg *v1alpha1.CapacityGrant) { cg.Spec.PodTemplate.Labels["tier"] = "edge" }, wantChange: true},
		{
			name:       "template annotation",
			mod:        func(cg *v1alpha1.CapacityGrant) { cg.Spec.PodTemplate.Annotations = map[string]string{"team": "a"} },
			wantChange: true,
		},
		{name: "container args", mod: func(cg *v1alpha1.CapacityGrant) { container(cg).Args = []string{"-delta-registry=r"} }, wantChange: true},
		{
			name: "memory limit",
			mod: func(cg *v1alpha1.CapacityGrant) {
				container(cg).Resources.Limits[corev1.ResourceMemory] = resource.MustParse("2Gi")
			},
			wantChange: true,
		},
		{
			name: "node selector",
			mod: func(cg *v1alpha1.CapacityGrant) {
				cg.Spec.PodTemplate.Spec.NodeSelector = map[string]string{"pool": "b"}
			},
			wantChange: true,
		},
		{name: "grant labels", mod: func(cg *v1alpha1.CapacityGrant) { cg.Labels = map[string]string{"owner": "b"} }},
		{name: "resource version", mod: func(cg *v1alpha1.CapacityGrant) { cg.ResourceVersion = "42" }},
		{name: "generation", mod: func(cg *v1alpha1.CapacityGrant) { cg.Generation = 7 }},
		{name: "status", mod: func(cg *v1alpha1.CapacityGrant) { cg.Status.ResolvedImage = "elsewhere" }},
	}
	base := mustHash(t, testGrant(), testImage)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cg := testGrant()
			if tc.mod != nil {
				tc.mod(cg)
			}
			image := testImage
			if tc.image != "" {
				image = tc.image
			}
			got := mustHash(t, cg, image)
			if changed := got != base; changed != tc.wantChange {
				t.Errorf("Hash = %q against base %q, changed = %v, want %v", got, base, changed, tc.wantChange)
			}
		})
	}
}

// TestHashForm checks the shape of Hash's result. A nil grant is an error
// with no hash.
func TestHashForm(t *testing.T) {
	cases := []struct {
		name    string
		cg      *v1alpha1.CapacityGrant
		wantErr string
	}{
		{name: "nil grant", wantErr: "nil CapacityGrant"},
		{name: "no writeBudget", cg: testGrant()},
		{name: "writeBudget set", cg: testGrant(func(cg *v1alpha1.CapacityGrant) { cg.Spec.WriteBudget = ptr.To(resource.MustParse("512Mi")) })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Hash(tc.cg, testImage)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("Hash = %q, %v, want an error holding %q", got, err, tc.wantErr)
				}
				if got != "" {
					t.Errorf("Hash = %q with an error, want empty", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Hash: %v", err)
			}
			if !hashPattern.MatchString(got) {
				t.Errorf("Hash = %q, want 16 lower-case hex characters", got)
			}
		})
	}
}

// TestHashLeavesGrantAlone checks that Hash writes nothing into the grant,
// which is the informer's cached copy. 512Mi is not in canonical form, so
// ParseQuantity leaves its string uncached and Quantity.String would cache
// it. reflect.DeepEqual sees that cache where cmp.Diff and
// equality.Semantic, which compare a Quantity by its value, do not.
func TestHashLeavesGrantAlone(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*v1alpha1.CapacityGrant)
	}{
		{name: "no writeBudget"},
		{name: "writeBudget set", mod: func(cg *v1alpha1.CapacityGrant) { cg.Spec.WriteBudget = ptr.To(resource.MustParse("512Mi")) }},
		{
			name: "writeBudget and a CPU limit set",
			mod: func(cg *v1alpha1.CapacityGrant) {
				cg.Spec.WriteBudget = ptr.To(resource.MustParse("1536Mi"))
				container(cg).Resources.Limits[corev1.ResourceCPU] = resource.MustParse("1500m")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cg := testGrant()
			if tc.mod != nil {
				tc.mod(cg)
			}
			before := cg.DeepCopy()
			first := mustHash(t, cg, testImage)
			if !reflect.DeepEqual(cg, before) {
				t.Errorf("Hash changed the grant\n got %#v\nwant %#v", cg, before)
			}
			if second := mustHash(t, cg, testImage); second != first {
				t.Errorf("Hash = %q on the second call, want %q", second, first)
			}
		})
	}
}

func TestImageOf(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*v1alpha1.CapacityGrant)
		want string
	}{
		{
			name: "the fiberd container's image",
			mod:  func(cg *v1alpha1.CapacityGrant) { container(cg).Image = testImage },
			want: testImage,
		},
		{
			name: "no image set",
			mod:  func(cg *v1alpha1.CapacityGrant) { container(cg).Image = "" },
		},
		{
			name: "no fiberd container",
			mod: func(cg *v1alpha1.CapacityGrant) {
				cg.Spec.PodTemplate.Spec.Containers = []corev1.Container{{Name: "other", Image: testImage}}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cg := testGrant()
			tc.mod(cg)
			if got := ImageOf(cg); got != tc.want {
				t.Errorf("ImageOf = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestHashPinned pins the hash of the test grant to the value it had
// before helperImage existed. Fields added later are omitted while unset,
// so an upgrade does not roll the Pods of every grant.
func TestHashPinned(t *testing.T) {
	const want = "572197dce76da108"
	if got := mustHash(t, testGrant(), testImage); got != want {
		t.Fatalf("Hash = %q, want %q", got, want)
	}
}
