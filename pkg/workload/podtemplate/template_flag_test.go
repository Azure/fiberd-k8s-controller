package podtemplate

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Azure/fiberd/pkg/runtime/host"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

func TestTemplateFlag(t *testing.T) {
	cases := []struct {
		name   string
		zygote v1alpha1.ZygoteSpec
		want   string
		// refused says fiberd's host.FormatTemplateFlag refuses the
		// zygote, and TemplateFlag must report its reason on spec.zygote.
		refused bool
	}{
		{
			name:   "path only",
			zygote: v1alpha1.ZygoteSpec{Digest: testDigest, Command: []string{"/app/zygote"}},
			want:   testDigest + "=/app/zygote",
		},
		{
			name:   "path and arguments joined by single spaces",
			zygote: v1alpha1.ZygoteSpec{Digest: testDigest, Command: []string{"/usr/bin/python3", "-m", "app.zygote", "--port=8080"}},
			want:   testDigest + "=/usr/bin/python3 -m app.zygote --port=8080",
		},
		{
			name:    "nil command",
			zygote:  v1alpha1.ZygoteSpec{Digest: testDigest},
			refused: true,
		},
		{
			name:    "empty command",
			zygote:  v1alpha1.ZygoteSpec{Digest: testDigest, Command: []string{}},
			refused: true,
		},
		{
			name:    "empty element",
			zygote:  v1alpha1.ZygoteSpec{Digest: testDigest, Command: []string{"/app/zygote", ""}},
			refused: true,
		},
		{
			name:    "element with a space",
			zygote:  v1alpha1.ZygoteSpec{Digest: testDigest, Command: []string{"/app/zygote", "--name=a b"}},
			refused: true,
		},
		{
			name:    "element with a tab",
			zygote:  v1alpha1.ZygoteSpec{Digest: testDigest, Command: []string{"/app/zy\tgote"}},
			refused: true,
		},
		{
			name:    "element with a newline",
			zygote:  v1alpha1.ZygoteSpec{Digest: testDigest, Command: []string{"/app/zygote\n"}},
			refused: true,
		},
		{
			name:    "element with a no-break space",
			zygote:  v1alpha1.ZygoteSpec{Digest: testDigest, Command: []string{"/app/zy gote"}},
			refused: true,
		},
		{
			name:    "digest without its algorithm",
			zygote:  v1alpha1.ZygoteSpec{Digest: strings.Repeat("ab", 32), Command: []string{"/app/zygote"}},
			refused: true,
		},
		{
			name:    "upper-case digest",
			zygote:  v1alpha1.ZygoteSpec{Digest: "sha256:" + strings.Repeat("AB", 32), Command: []string{"/app/zygote"}},
			refused: true,
		},
		{
			name:    "default key is not a digest",
			zygote:  v1alpha1.ZygoteSpec{Digest: "default", Command: []string{"/app/zygote"}},
			refused: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := TemplateFlag(tc.zygote)
			if tc.refused {
				_, hostErr := host.FormatTemplateFlag(tc.zygote.Digest, tc.zygote.Command)
				if hostErr == nil {
					t.Fatalf("host.FormatTemplateFlag accepts %+v, the case must use a zygote fiberd refuses", tc.zygote)
				}
				if err == nil {
					t.Fatalf("TemplateFlag = %q, want fiberd's error %q", got, hostErr)
				}
				for _, want := range []string{"spec.zygote: Invalid value", hostErr.Error()} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("TemplateFlag error %q does not contain %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("TemplateFlag: %v", err)
			}
			if got != tc.want {
				t.Errorf("TemplateFlag = %q, want %q", got, tc.want)
			}

			// fiberd must read the value back into the same digest and command.
			templates := map[string]string{}
			if err := host.ParseTemplateFlag(templates, got); err != nil {
				t.Fatalf("fiberd refused -template=%q: %v", got, err)
			}
			cmd, ok := host.Config{Templates: templates}.Command(tc.zygote.Digest)
			if !ok {
				t.Fatalf("fiberd has no command for %s", tc.zygote.Digest)
			}
			if diff := cmp.Diff(tc.zygote.Command, cmd); diff != "" {
				t.Errorf("fiberd read command mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
