package app

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/Azure/fiberd-k8s-controller/pkg/workload/podtemplate"
)

// configDir is relative to this package, since go test runs in it.
const configDir = "../../../config"

// TestControllerManifests ties the Deployment, the Service and the serving
// Certificate to the issuer URL and the flags. The manifests hold literals,
// and nothing else catches a drift before the controller fails on a cluster.
// Rows that compare a manifest value with a literal pin values the design
// fixes, such as the certificate lifetimes and the probe paths, so a silent
// edit fails.
func TestControllerManifests(t *testing.T) {
	manager := readDir(t, filepath.Join(configDir, "manager"))
	certs := readDir(t, filepath.Join(configDir, "certmanager"))

	deploy := find(t, manager, "Deployment", "fiberd-k8s-controller")
	svc := find(t, manager, "Service", "fiberd-k8s-controller")
	cert := find(t, certs, "Certificate", "fiberd-k8s-controller-tls")

	container := named(t, objectList(t, deploy.Object, "spec", "template", "spec", "containers"), "manager")
	flags := map[string]string{}
	for _, arg := range stringList(t, container, "args") {
		name, value, _ := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		flags[name] = value
	}
	issuerURL, err := url.Parse(flags["issuer-url"])
	if err != nil {
		t.Fatalf("--issuer-url %q: %v", flags["issuer-url"], err)
	}
	host := issuerURL.Hostname()
	svcHTTPS := named(t, objectList(t, svc.Object, "spec", "ports"), "https")
	ports := objectList(t, container, "ports")

	// A Service selector key the Pod lacks matches no Pod, so only the
	// selector's keys are compared.
	selector := stringMap(t, svc.Object, "spec", "selector")
	labels := stringMap(t, deploy.Object, "spec", "template", "metadata", "labels")
	selected := map[string]string{}
	for k := range selector {
		if v, ok := labels[k]; ok {
			selected[k] = v
		}
	}

	// The Pod mounts the Certificate's Secret, found by secretName rather
	// than by volume name, so a renamed volume cannot hide a wrong Secret.
	secretName := get(t, cert.Object, "spec", "secretName")
	var volume string
	for _, v := range objectList(t, deploy.Object, "spec", "template", "spec", "volumes") {
		if s, ok := v["secret"].(map[string]any); ok && s["secretName"] == secretName {
			volume = get(t, v, "name")
		}
	}
	if volume == "" {
		t.Fatalf("the Deployment mounts no volume from Secret %q", secretName)
	}
	mount := named(t, objectList(t, container, "volumeMounts"), volume)
	mountPath := get(t, mount, "mountPath")

	cases := []struct {
		name string
		got  any
		want any
	}{
		{name: "issuer URL host", got: host, want: svc.GetName() + "." + svc.GetNamespace() + ".svc"},
		{name: "issuer URL port", got: issuerURL.Port(), want: get(t, svcHTTPS, "port")},
		{name: "Service targetPort", got: get(t, svcHTTPS, "targetPort"), want: get(t, named(t, ports, "https"), "name")},
		{name: "template labels holding the Service selector", got: selected, want: selector},
		{name: "container https port", got: get(t, named(t, ports, "https"), "containerPort"), want: bindPort(t, flags, "issuer-bind-address", defaultIssuerAddr)},
		{name: "container probes port", got: get(t, named(t, ports, "probes"), "containerPort"), want: bindPort(t, flags, "health-probe-bind-address", defaultProbeAddr)},
		{name: "--tls-cert-file directory", got: path.Dir(flags["tls-cert-file"]), want: mountPath},
		{name: "--tls-private-key-file directory", got: path.Dir(flags["tls-private-key-file"]), want: mountPath},
		{name: "--tls-cert-file base", got: path.Base(flags["tls-cert-file"]), want: "tls.crt"},
		{name: "--tls-private-key-file base", got: path.Base(flags["tls-private-key-file"]), want: "tls.key"},
		{name: "tls mount readOnly", got: mount["readOnly"], want: true},
		{name: "readiness probe path", got: get(t, container, "readinessProbe", "httpGet", "path"), want: "/readyz"},
		{name: "readiness probe port", got: get(t, container, "readinessProbe", "httpGet", "port"), want: "probes"},
		{name: "liveness probe path", got: get(t, container, "livenessProbe", "httpGet", "path"), want: "/healthz"},
		{name: "liveness probe port", got: get(t, container, "livenessProbe", "httpGet", "port"), want: "probes"},
		{name: "Certificate namespace", got: cert.GetNamespace(), want: "fiberd-system"},
		{name: "Certificate duration", got: get(t, cert.Object, "spec", "duration"), want: "24h"},
		{name: "Certificate renewBefore", got: get(t, cert.Object, "spec", "renewBefore"), want: "8h"},
		{name: "Certificate dnsNames", got: stringList(t, cert.Object, "spec", "dnsNames"), want: []string{host, host + ".cluster.local"}},
		{name: "Certificate usages", got: stringList(t, cert.Object, "spec", "usages"), want: []string{"server auth"}},
		{name: "Certificate rotationPolicy", got: get(t, cert.Object, "spec", "privateKey", "rotationPolicy"), want: "Always"},
		{name: "Certificate issuerRef kind", got: get(t, cert.Object, "spec", "issuerRef", "kind"), want: "ClusterIssuer"},
		{name: "Certificate issuerRef name", got: get(t, cert.Object, "spec", "issuerRef", "name"), want: podtemplate.CAIssuerName},
	}
	for _, tc := range cases {
		if !reflect.DeepEqual(tc.got, tc.want) {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

// TestCAManifests checks the CA bootstrap the ClusterIssuer serves from.
func TestCAManifests(t *testing.T) {
	objs := readDir(t, filepath.Join(configDir, "certs"))

	ca := find(t, objs, "Certificate", "fiberd-ca")
	clusterIssuer := find(t, objs, "ClusterIssuer", podtemplate.CAIssuerName)
	selfSigned := find(t, objs, get(t, ca.Object, "spec", "issuerRef", "kind"), get(t, ca.Object, "spec", "issuerRef", "name"))
	_, hasSelfSigned, _ := unstructured.NestedFieldNoCopy(selfSigned.Object, "spec", "selfSigned")

	cases := []struct {
		name string
		got  any
		want any
	}{
		{name: "ClusterIssuer ca.secretName", got: get(t, clusterIssuer.Object, "spec", "ca", "secretName"), want: get(t, ca.Object, "spec", "secretName")},
		{name: "CA namespace", got: ca.GetNamespace(), want: "cert-manager"},
		{name: "CA isCA", got: get(t, ca.Object, "spec", "isCA"), want: "true"},
		{name: "CA key algorithm", got: get(t, ca.Object, "spec", "privateKey", "algorithm"), want: "ECDSA"},
		{name: "CA key size", got: get(t, ca.Object, "spec", "privateKey", "size"), want: "256"},
		{name: "CA key rotationPolicy", got: get(t, ca.Object, "spec", "privateKey", "rotationPolicy"), want: "Never"},
		{name: "CA duration", got: get(t, ca.Object, "spec", "duration"), want: "87600h"},
		{name: "self-signed Issuer kind", got: selfSigned.GetKind(), want: "Issuer"},
		{name: "self-signed Issuer spec.selfSigned set", got: hasSelfSigned, want: true},
		{name: "self-signed Issuer namespace", got: selfSigned.GetNamespace(), want: ca.GetNamespace()},
	}
	for _, tc := range cases {
		if !reflect.DeepEqual(tc.got, tc.want) {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

// TestDefaultKustomization checks the install carries the CA and the
// serving Certificate.
func TestDefaultKustomization(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(configDir, "default", "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var k struct {
		Resources []string `json:"resources"`
	}
	if err := yaml.Unmarshal(raw, &k); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"../certs", "../certmanager"} {
		if !slices.Contains(k.Resources, want) {
			t.Errorf("resources = %q, want it to hold %q", k.Resources, want)
		}
	}
}

// readDir reads the files dir's kustomization.yaml lists, so an object
// dropped from the build fails the test that looks for it. Directory
// entries are skipped, since each test reads its own directory.
func readDir(t *testing.T, dir string) []*unstructured.Unstructured {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var k struct {
		Resources []string `json:"resources"`
	}
	if err := yaml.Unmarshal(raw, &k); err != nil {
		t.Fatalf("parse %s: %v", filepath.Join(dir, "kustomization.yaml"), err)
	}
	var objs []*unstructured.Unstructured
	for _, name := range k.Resources {
		if filepath.Ext(name) != ".yaml" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
		for {
			doc, err := r.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			u := &unstructured.Unstructured{}
			if err := yaml.Unmarshal(doc, &u.Object); err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}
			if len(u.Object) == 0 {
				continue
			}
			objs = append(objs, u)
		}
	}
	return objs
}

// find returns the one object of kind named name.
func find(t *testing.T, objs []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	t.Helper()
	var found *unstructured.Unstructured
	for _, u := range objs {
		if u.GetKind() == kind && u.GetName() == name {
			if found != nil {
				t.Fatalf("two %s objects named %q", kind, name)
			}
			found = u
		}
	}
	if found == nil {
		t.Fatalf("no %s named %q", kind, name)
	}
	return found
}

// get returns the scalar at fields as a string. A YAML number parses to a
// float64, and %v prints a whole one without a fraction.
func get(t *testing.T, obj map[string]any, fields ...string) string {
	t.Helper()
	v, ok, err := unstructured.NestedFieldNoCopy(obj, fields...)
	if err != nil || !ok {
		t.Fatalf("no %s: %v", strings.Join(fields, "."), err)
	}
	return fmt.Sprint(v)
}

// stringList returns the string list at fields.
func stringList(t *testing.T, obj map[string]any, fields ...string) []string {
	t.Helper()
	s, ok, err := unstructured.NestedStringSlice(obj, fields...)
	if err != nil || !ok {
		t.Fatalf("no string list %s: %v", strings.Join(fields, "."), err)
	}
	return s
}

// stringMap returns the string map at fields.
func stringMap(t *testing.T, obj map[string]any, fields ...string) map[string]string {
	t.Helper()
	m, ok, err := unstructured.NestedStringMap(obj, fields...)
	if err != nil || !ok {
		t.Fatalf("no string map %s: %v", strings.Join(fields, "."), err)
	}
	return m
}

// bindPort returns the port of the address flag holds, or of def when the
// args leave the flag out and the binary falls back to its default.
func bindPort(t *testing.T, flags map[string]string, flag, def string) string {
	t.Helper()
	addr, ok := flags[flag]
	if !ok {
		addr = def
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("--%s %q: %v", flag, addr, err)
	}
	return port
}

// objectList returns the list of objects at fields.
func objectList(t *testing.T, obj map[string]any, fields ...string) []map[string]any {
	t.Helper()
	list, ok, err := unstructured.NestedSlice(obj, fields...)
	if err != nil || !ok {
		t.Fatalf("no list %s: %v", strings.Join(fields, "."), err)
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("%s holds a %T, want an object", strings.Join(fields, "."), item)
		}
		out = append(out, m)
	}
	return out
}

// named returns the entry of list whose name is name.
func named(t *testing.T, list []map[string]any, name string) map[string]any {
	t.Helper()
	for _, m := range list {
		if m["name"] == name {
			return m
		}
	}
	t.Fatalf("no entry named %q", name)
	return nil
}
