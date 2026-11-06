package main

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"sigs.k8s.io/yaml"
)

const (
	manager = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: manager
spec:
  image: controller:latest
`
	issuer = `
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: ca
  namespace: cert-manager
spec:
  selfSigned: {}
`
	base = manager + "---" + issuer

	managerFile = "deployment-manager.yaml"
	issuerFile  = "issuer-ca.yaml"

	wantManager = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: manager
spec:
  image: '{{include "fiberd-k8s-controller.image" .}}'
`
	wantIssuer = `
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: ca
  namespace: '{{.Values.certManager.namespace}}'
spec:
  selfSigned: {}
`
)

func TestRender(t *testing.T) {
	tests := []struct {
		name    string
		build   string
		want    map[string]string
		wantErr string
	}{
		{
			name:  "the image and the cert-manager namespace become templates",
			build: base,
			want:  map[string]string{managerFile: wantManager, issuerFile: wantIssuer},
		},
		{
			name: "the Namespace and the CRDs are left out",
			build: base + `
---
apiVersion: v1
kind: Namespace
metadata:
  name: fiberd-system
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: grants.fiberd.io
`,
			want: map[string]string{managerFile: wantManager, issuerFile: wantIssuer},
		},
		{
			name: "a value that only holds the namespace name stays",
			build: base + `
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: names
data:
  group: cert-manager.io
  note: cert-manager serves this
`,
			want: map[string]string{managerFile: wantManager, issuerFile: wantIssuer, "configmap-names.yaml": `
apiVersion: v1
kind: ConfigMap
metadata:
  name: names
data:
  group: cert-manager.io
  note: cert-manager serves this
`},
		},
		{
			name: "both ServiceAccount names become templates inside longer values",
			build: base + `
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: subjects
data:
  csi: "user == 'system:serviceaccount:cert-manager:cert-manager-csi-driver'"
  plain: system:serviceaccount:cert-manager:cert-manager
`,
			want: map[string]string{managerFile: wantManager, issuerFile: wantIssuer, "configmap-subjects.yaml": `
apiVersion: v1
kind: ConfigMap
metadata:
  name: subjects
data:
  csi: "user == 'system:serviceaccount:{{.Values.certManager.namespace}}:{{.Values.certManager.csiDriverServiceAccount}}'"
  plain: "system:serviceaccount:{{.Values.certManager.namespace}}:{{.Values.certManager.serviceAccount}}"
`},
		},
		{
			name: "a template in a source value is refused",
			build: base + `
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: braces
data:
  key: "{{ .Values.x }}"
`,
			wantErr: "configmap-braces.yaml holds {{",
		},
		{
			name: "a template in a source key is refused",
			build: base + `
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: braces
data:
  "{{ .Values.x }}": value
`,
			wantErr: "configmap-braces.yaml holds {{",
		},
		{
			name:    "two objects with one file name are refused",
			build:   base + "---" + issuer,
			wantErr: "two objects would share the file issuer-ca.yaml",
		},
		{
			name:    "an object with no name is refused",
			build:   base + "---\nkind: ConfigMap\n",
			wantErr: "no kind or name",
		},
		{
			name:    "a build with no default image is refused",
			build:   issuer,
			wantErr: `no object holds "controller:latest"`,
		},
		{
			name:    "a build with no cert-manager namespace is refused",
			build:   manager,
			wantErr: `no object holds "cert-manager"`,
		},
		{
			name:    "a build that is not YAML is refused",
			build:   "kind: [",
			wantErr: "parsing the build",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			files, err := render([]byte(tc.build), substitutions)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("render error = %v, want it to hold %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			got := make(map[string]any, len(files))
			for name, data := range files {
				if !strings.HasPrefix(string(data), header) {
					t.Errorf("%s does not start with the header", name)
				}
				got[name] = parse(t, data)
			}
			want := make(map[string]any, len(tc.want))
			for name, data := range tc.want {
				want[name] = parse(t, []byte(data))
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("files differ (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRenderIsStable(t *testing.T) {
	first, err := render([]byte(base), substitutions)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	tests := []struct {
		name  string
		build string
	}{
		{name: "the same input", build: base},
		{name: "a new comment", build: "# A comment.\n" + base},
		{name: "another key order", build: `
kind: Deployment
apiVersion: apps/v1
spec:
  image: controller:latest
metadata:
  name: manager
---` + issuer},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := render([]byte(tc.build), substitutions)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if diff := cmp.Diff(first, got); diff != "" {
				t.Errorf("bytes differ (-first +got):\n%s", diff)
			}
		})
	}
}

func parse(t *testing.T, data []byte) any {
	t.Helper()
	var obj any
	if err := yaml.Unmarshal(data, &obj); err != nil {
		t.Fatalf("parsing %s: %v", data, err)
	}
	return obj
}
