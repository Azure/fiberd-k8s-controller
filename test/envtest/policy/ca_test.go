//go:build envtest

package policy_test

import (
	"maps"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
	"github.com/Azure/fiberd-k8s-controller/pkg/testutil"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload"
	"github.com/Azure/fiberd-k8s-controller/pkg/workload/podtemplate"
)

// The users of a default install that may ask fiberd-ca for a certificate.
const (
	issuingController = "system:serviceaccount:cert-manager:cert-manager"
	csiDriver         = "system:serviceaccount:cert-manager:cert-manager-csi-driver"
)

// The messages below are cut from config/certs, so a reworded rule fails
// here rather than passing on another rule's refusal.
const (
	requesterMessage         = "only the issuing controller's and the csi-driver's ServiceAccounts may create a CertificateRequest for fiberd-ca"
	requestIsCAMessage       = "a CertificateRequest for fiberd-ca must not set isCA"
	requestUsagesMessage     = "a CertificateRequest for fiberd-ca must have usages of exactly server auth or exactly client auth"
	requestServerLifeMessage = "a server auth CertificateRequest for fiberd-ca must set a duration of at most 24h"
	requestClientLifeMessage = "a client auth CertificateRequest for fiberd-ca must leave duration unset or set it to at most 2160h"

	fieldsMessage         = "is not allowed in a Certificate for fiberd-ca, which may set only issuerRef, secretName"
	certUsagesMessage     = "a Certificate for fiberd-ca must have usages of exactly client auth with no dnsNames, or exactly server auth"
	certNamespaceMessage  = "a server auth Certificate for fiberd-ca is allowed only in fiberd-system"
	certNamesMessage      = "a server auth Certificate for fiberd-ca must have dnsNames of exactly fiberd-k8s-controller.fiberd-system.svc and fiberd-k8s-controller.fiberd-system.svc.cluster.local"
	durationFormMessage   = "the duration of a Certificate for fiberd-ca must match ^([0-9]+[hms])+$"
	certWildcardMessage   = "the commonName of a Certificate for fiberd-ca must not hold a wildcard *"
	certServerLifeMessage = "a server auth Certificate for fiberd-ca must set a duration of at most 24h"
	certClientLifeMessage = "a client auth Certificate for fiberd-ca must leave duration unset or set it to at most 2160h"

	attrKeysMessage      = "a csi volume for fiberd-ca may set only the csi.cert-manager.io/ attributes"
	issuerKindMessage    = "a csi volume for fiberd-ca must set issuer-kind to ClusterIssuer"
	dollarMessage        = "no attribute of a csi volume for fiberd-ca may hold $, except a common-name of exactly ${POD_NAME}"
	keyUsagesMessage     = "a csi volume for fiberd-ca must set key-usages to exactly server auth or exactly client auth"
	podNamespaceMessage  = "a server auth csi volume for fiberd-ca is not allowed in fiberd-system"
	dnsNamesMessage      = "a server auth csi volume for fiberd-ca must set dns-names to exactly fiberd-<grant>.<namespace>.svc"
	podWildcardMessage   = "the common-name of a csi volume for fiberd-ca must not hold a wildcard *"
	podDurationMessage   = "the duration of a csi volume for fiberd-ca must match ^([0-9]+[hms])+$"
	podServerLifeMessage = "a server auth csi volume for fiberd-ca must set a duration of at most 24h"
	clientDNSMessage     = "a client auth csi volume for fiberd-ca must not set dns-names"
	podClientLifeMessage = "a client auth csi volume for fiberd-ca must leave duration unset or set it to at most 2160h"
)

// The csi-driver attributes the cases set.
const (
	attrPrefix      = "csi.cert-manager.io/"
	attrIssuerName  = attrPrefix + "issuer-name"
	attrIssuerKind  = attrPrefix + "issuer-kind"
	attrCommonName  = attrPrefix + "common-name"
	attrDNSNames    = attrPrefix + "dns-names"
	attrKeyUsages   = attrPrefix + "key-usages"
	attrDuration    = attrPrefix + "duration"
	attrRenewBefore = attrPrefix + "renew-before"
)

// csrBytes stands in for a CSR. The CRD only checks it is base64, and the
// policy cannot read it.
const csrBytes = "cmVxdWVzdA=="

// asUser is a client that the API server sees as username. It keeps
// cluster-admin rights, so only the policy can refuse it.
func asUser(t *testing.T, username string) client.Client {
	t.Helper()
	cfg := rest.CopyConfig(restConfig)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: username, Groups: []string{"system:masters"}}
	c, err := client.New(cfg, client.Options{Scheme: admin.Scheme()})
	if err != nil {
		t.Fatalf("create a client for %s: %v", username, err)
	}
	return c
}

// expectAdmitted fails t unless err is nil.
func expectAdmitted(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("the request was refused, want it admitted: %v", err)
	}
}

// expect fails t unless err admits, for an empty substr, or refuses as
// Forbidden with a message holding substr.
func expect(t *testing.T, err error, substr string) {
	t.Helper()
	if substr == "" {
		expectAdmitted(t, err)
		return
	}
	expectDenied(t, err, metav1.StatusReasonForbidden, substr)
}

// fiberdCARef names ClusterIssuer fiberd-ca.
func fiberdCARef() map[string]any {
	return map[string]any{"name": "fiberd-ca", "kind": "ClusterIssuer", "group": "cert-manager.io"}
}

func certificateRequest(ns string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "CertificateRequest",
		"metadata":   map[string]any{"namespace": ns, "generateName": "request-"},
		"spec":       spec,
	}}
}

// clientRequestSpec is what the issuing controller writes for a caller's
// Certificate that leaves duration unset.
func clientRequestSpec() map[string]any {
	return map[string]any{"issuerRef": fiberdCARef(), "request": csrBytes, "usages": []any{"client auth"}}
}

// serverRequestSpec is what the csi-driver writes for a grant Pod's volume.
func serverRequestSpec() map[string]any {
	return map[string]any{"issuerRef": fiberdCARef(), "request": csrBytes, "usages": []any{"server auth"}, "duration": "24h0m0s"}
}

func certificate(ns string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata":   map[string]any{"namespace": ns, "generateName": "caller-"},
		"spec":       spec,
	}}
}

// clientCertificateSpec is a caller's Certificate as the router that uses
// grants writes it: client auth, a common name and a key that never
// rotates, since callerThumbprint names the key. Its duration is unset.
func clientCertificateSpec() map[string]any {
	return map[string]any{
		"issuerRef":  fiberdCARef(),
		"secretName": "router-tls",
		"commonName": "router",
		"usages":     []any{"client auth"},
		"privateKey": map[string]any{"algorithm": "ECDSA", "size": int64(256), "rotationPolicy": "Never"},
	}
}

// servingCertificate is the controller's own Certificate, as config
// installs it.
func servingCertificate(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	u, err := testutil.ReadObject(servingCertificateFile)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// withSpec returns a copy of u with spec.key set to value, or removed for
// a nil value.
func withSpec(u *unstructured.Unstructured, key string, value any) *unstructured.Unstructured {
	u = u.DeepCopy()
	if value == nil {
		unstructured.RemoveNestedField(u.Object, "spec", key)
		return u
	}
	u.Object["spec"].(map[string]any)[key] = value
	return u
}

// inNamespace returns a copy of u in ns.
func inNamespace(u *unstructured.Unstructured, ns string) *unstructured.Unstructured {
	u = u.DeepCopy()
	u.SetNamespace(ns)
	return u
}

// csiPod is a Pod in ns with one csi volume holding attrs.
func csiPod(ns string, attrs map[string]any) *unstructured.Unstructured {
	return podWithVolumes(ns, csiVolume("tls", attrs))
}

// csiVolume is a csi-driver volume holding attrs.
func csiVolume(name string, attrs map[string]any) map[string]any {
	return map[string]any{
		"name": name,
		"csi":  map[string]any{"driver": "csi.cert-manager.io", "readOnly": true, "volumeAttributes": attrs},
	}
}

func podWithVolumes(ns string, volumes ...any) *unstructured.Unstructured {
	spec := map[string]any{
		"containers":                   []any{map[string]any{"name": "app", "image": "registry.example/app:1"}},
		"automountServiceAccountToken": false,
	}
	if len(volumes) > 0 {
		spec["volumes"] = volumes
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"namespace": ns, "generateName": "pod-"},
		"spec":       spec,
	}}
}

// serverAttributes are the attributes the builder writes for grant web in
// ns, as literals so a builder change does not move the case with it.
func serverAttributes(ns string) map[string]any {
	host := "fiberd-web." + ns + ".svc"
	return map[string]any{
		attrIssuerName:                   "fiberd-ca",
		attrIssuerKind:                   "ClusterIssuer",
		attrCommonName:                   "${POD_NAME}",
		attrDNSNames:                     host + "," + host + ".cluster.local",
		attrKeyUsages:                    "server auth",
		attrDuration:                     "24h",
		attrRenewBefore:                  "8h",
		attrPrefix + "reuse-private-key": "false",
	}
}

// clientAttributes ask for a caller's client leaf.
func clientAttributes() map[string]any {
	return map[string]any{
		attrIssuerName: "fiberd-ca",
		attrIssuerKind: "ClusterIssuer",
		attrCommonName: "router",
		attrKeyUsages:  "client auth",
	}
}

// withAttributes returns a copy of attrs with extra added.
func withAttributes(attrs, extra map[string]any) map[string]any {
	attrs = maps.Clone(attrs)
	maps.Copy(attrs, extra)
	return attrs
}

// withAttribute returns a copy of attrs with key set to value, or removed
// for an empty value.
func withAttribute(attrs map[string]any, key, value string) map[string]any {
	attrs = maps.Clone(attrs)
	if value == "" {
		delete(attrs, key)
		return attrs
	}
	attrs[key] = value
	return attrs
}

// TestCARequests runs fiberd-ca-requests. Each case asks as one user, and
// the issuing controller and the csi-driver are the only users admitted.
func TestCARequests(t *testing.T) {
	ns := namespace(t)
	tests := []struct {
		name   string
		user   string
		obj    *unstructured.Unstructured
		substr string
	}{
		{name: "the issuing controller asks for a caller's client leaf with no duration", user: issuingController, obj: certificateRequest(ns, clientRequestSpec())},
		{name: "the issuing controller asks for the controller's serving leaf", user: issuingController, obj: certificateRequest(controllerNamespace, serverRequestSpec())},
		{name: "the csi-driver asks for a grant Pod's serving leaf", user: csiDriver, obj: certificateRequest(ns, serverRequestSpec())},
		{name: "the csi-driver asks for a client leaf of 2160h", user: csiDriver, obj: withSpec(certificateRequest(ns, clientRequestSpec()), "duration", "2160h0m0s")},
		{name: "a cluster admin", obj: certificateRequest(ns, clientRequestSpec()), substr: requesterMessage},
		{name: "a tenant ServiceAccount", user: "system:serviceaccount:" + ns + ":default", obj: certificateRequest(ns, clientRequestSpec()), substr: requesterMessage},
		{name: "a ServiceAccount named like the controller in another namespace", user: "system:serviceaccount:" + ns + ":cert-manager", obj: certificateRequest(ns, clientRequestSpec()), substr: requesterMessage},
		{name: "isCA true", user: issuingController, obj: withSpec(certificateRequest(ns, clientRequestSpec()), "isCA", true), substr: requestIsCAMessage},
		{name: "isCA false", user: issuingController, obj: withSpec(certificateRequest(ns, clientRequestSpec()), "isCA", false)},
		{name: "no usages", user: issuingController, obj: withSpec(certificateRequest(ns, clientRequestSpec()), "usages", nil), substr: requestUsagesMessage},
		{name: "both usages", user: csiDriver, obj: withSpec(certificateRequest(ns, serverRequestSpec()), "usages", []any{"server auth", "client auth"}), substr: requestUsagesMessage},
		{name: "server auth beside the default usages", user: csiDriver, obj: withSpec(certificateRequest(ns, serverRequestSpec()), "usages", []any{"digital signature", "key encipherment", "server auth"}), substr: requestUsagesMessage},
		{name: "code signing", user: issuingController, obj: withSpec(certificateRequest(ns, clientRequestSpec()), "usages", []any{"code signing"}), substr: requestUsagesMessage},
		{name: "a serving leaf with no duration", user: csiDriver, obj: withSpec(certificateRequest(ns, serverRequestSpec()), "duration", nil), substr: requestServerLifeMessage},
		{name: "a serving leaf of 24h1s", user: csiDriver, obj: withSpec(certificateRequest(ns, serverRequestSpec()), "duration", "24h0m1s"), substr: requestServerLifeMessage},
		{name: "a client leaf of 2161h", user: issuingController, obj: withSpec(certificateRequest(ns, clientRequestSpec()), "duration", "2161h0m0s"), substr: requestClientLifeMessage},
		{name: "an Issuer named fiberd-ca is held to the same rules", obj: withSpec(certificateRequest(ns, clientRequestSpec()), "issuerRef", map[string]any{"name": "fiberd-ca", "kind": "Issuer"}), substr: requesterMessage},
		{name: "an issuer of another group named fiberd-ca is held to the same rules", obj: withSpec(certificateRequest(ns, clientRequestSpec()), "issuerRef", map[string]any{"name": "fiberd-ca", "kind": "Signer", "group": "example.com"}), substr: requesterMessage},
		{name: "another issuer is not matched", obj: withSpec(withSpec(certificateRequest(ns, clientRequestSpec()), "issuerRef", map[string]any{"name": "fiberd-selfsigned", "kind": "Issuer"}), "isCA", true)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := admin
			if tc.user != "" {
				c = asUser(t, tc.user)
			}
			expect(t, c.Create(t.Context(), tc.obj.DeepCopy(), client.DryRunAll), tc.substr)
		})
	}
}

// TestCACertificates runs fiberd-ca-certificates on creates.
func TestCACertificates(t *testing.T) {
	ns := namespace(t)
	caller := certificate(ns, clientCertificateSpec())
	serving := servingCertificate(t)
	ca, err := testutil.ReadObject(caCertificateFile)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		obj    *unstructured.Unstructured
		substr string
	}{
		{name: "a caller's client leaf with commonName only and duration unset", obj: caller},
		{name: "a caller's client leaf in fiberd-system", obj: inNamespace(caller, controllerNamespace)},
		{name: "a client leaf of 2160h", obj: withSpec(caller, "duration", "2160h")},
		{name: "a client leaf of 90m", obj: withSpec(caller, "duration", "90m")},
		{name: "a client leaf with no commonName", obj: withSpec(caller, "commonName", nil)},
		{name: "a client leaf with every other allowed field", obj: withSpec(withSpec(withSpec(withSpec(caller, "renewBeforePercentage", int64(30)), "revisionHistoryLimit", int64(1)), "secretTemplate", map[string]any{"labels": map[string]any{"app": "router"}}), "renewBefore", "1h")},
		{name: "the controller's serving leaf", obj: serving},
		{name: "the controller's serving leaf with its names in the other order", obj: withSpec(serving, "dnsNames", []any{"fiberd-k8s-controller.fiberd-system.svc.cluster.local", "fiberd-k8s-controller.fiberd-system.svc"})},
		{name: "the CA itself is not matched", obj: inNamespace(ca, ns)},

		{name: "isCA", obj: withSpec(caller, "isCA", true), substr: "spec.isCA " + fieldsMessage},
		{name: "isCA false", obj: withSpec(caller, "isCA", false), substr: "spec.isCA " + fieldsMessage},
		{name: "a subject", obj: withSpec(caller, "subject", map[string]any{"organizations": []any{"team-a"}}), substr: "spec.subject " + fieldsMessage},
		{name: "a literal subject", obj: withSpec(caller, "literalSubject", "CN=router"), substr: "spec.literalSubject " + fieldsMessage},
		{name: "an IP address", obj: withSpec(caller, "ipAddresses", []any{"10.0.0.1"}), substr: "spec.ipAddresses " + fieldsMessage},
		{name: "a URI", obj: withSpec(caller, "uris", []any{"spiffe://cluster.local/ns/a/sa/b"}), substr: "spec.uris " + fieldsMessage},
		{name: "an email address", obj: withSpec(caller, "emailAddresses", []any{"a@example.com"}), substr: "spec.emailAddresses " + fieldsMessage},
		{name: "another name", obj: withSpec(caller, "otherNames", []any{map[string]any{"oid": "1.3.6.1.4.1.311.20.2.3", "utf8Value": "a@example.com"}}), substr: "spec.otherNames " + fieldsMessage},
		{name: "a keystore", obj: withSpec(caller, "keystores", map[string]any{"pkcs12": map[string]any{"create": true, "passwordSecretRef": map[string]any{"name": "p", "key": "k"}}}), substr: "spec.keystores " + fieldsMessage},

		{name: "a client leaf with dnsNames", obj: withSpec(caller, "dnsNames", []any{"router.example.com"}), substr: certUsagesMessage},
		{name: "no usages", obj: withSpec(caller, "usages", nil), substr: certUsagesMessage},
		{name: "both usages", obj: withSpec(caller, "usages", []any{"client auth", "server auth"}), substr: certUsagesMessage},
		{name: "client auth beside digital signature", obj: withSpec(caller, "usages", []any{"digital signature", "client auth"}), substr: certUsagesMessage},

		{name: "a serving leaf in a tenant namespace", obj: inNamespace(serving, ns), substr: certNamespaceMessage},
		{name: "a serving leaf with one of the names", obj: withSpec(serving, "dnsNames", []any{"fiberd-k8s-controller.fiberd-system.svc"}), substr: certNamesMessage},
		{name: "a serving leaf with a third name", obj: withSpec(serving, "dnsNames", []any{"fiberd-k8s-controller.fiberd-system.svc", "fiberd-k8s-controller.fiberd-system.svc.cluster.local", "fiberd-k8s-controller"}), substr: certNamesMessage},
		{name: "a serving leaf with a wildcard", obj: withSpec(serving, "dnsNames", []any{"fiberd-k8s-controller.fiberd-system.svc", "*.fiberd-system.svc.cluster.local"}), substr: certNamesMessage},
		{name: "a serving leaf with no names", obj: withSpec(serving, "dnsNames", nil), substr: certNamesMessage},
		{name: "a serving leaf with no duration", obj: withSpec(serving, "duration", nil), substr: certServerLifeMessage},
		{name: "a serving leaf of 25h", obj: withSpec(serving, "duration", "25h"), substr: certServerLifeMessage},
		{name: "a serving leaf of 24h1s", obj: withSpec(serving, "duration", "24h1s"), substr: certServerLifeMessage},
		{name: "a serving leaf of 1d", obj: withSpec(serving, "duration", "1d"), substr: durationFormMessage},
		{name: "a serving leaf of 1.5h", obj: withSpec(serving, "duration", "1.5h"), substr: durationFormMessage},
		{name: "a client leaf of 2161h", obj: withSpec(caller, "duration", "2161h"), substr: certClientLifeMessage},
		{name: "a client leaf of 90d", obj: withSpec(caller, "duration", "90d"), substr: durationFormMessage},
		{name: "a client leaf of free text", obj: withSpec(caller, "duration", "forever"), substr: durationFormMessage},
		{name: "a client leaf with a wildcard common name", obj: withSpec(caller, "commonName", "*.example.com"), substr: certWildcardMessage},
		{name: "the controller's serving leaf with a wildcard common name", obj: withSpec(serving, "commonName", "*"), substr: certWildcardMessage},
		{name: "the controller's serving leaf with a common name", obj: withSpec(serving, "commonName", "fiberd-k8s-controller")},

		{name: "an Issuer named fiberd-ca is held to the same rules", obj: withSpec(withSpec(caller, "issuerRef", map[string]any{"name": "fiberd-ca", "kind": "Issuer"}), "isCA", true), substr: "spec.isCA " + fieldsMessage},
		{name: "an issuer of another group named fiberd-ca is held to the same rules", obj: withSpec(withSpec(caller, "issuerRef", map[string]any{"name": "fiberd-ca", "kind": "Signer", "group": "example.com"}), "dnsNames", []any{"a.example.com"}), substr: certUsagesMessage},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			expect(t, dryRunCreate(t.Context(), tc.obj), tc.substr)
		})
	}
}

// TestCACertificateUpdates runs fiberd-ca-certificates on updates. One
// Certificate is stored while the binding is gone, as one stored before
// the policy would be, so a metadata-only update must still pass it.
func TestCACertificateUpdates(t *testing.T) {
	ns := namespace(t)
	good := certificate(ns, clientCertificateSpec())
	createOwned(t, good)

	bad := withSpec(certificate(ns, clientCertificateSpec()), "dnsNames", []any{"router.example.com"})
	t.Cleanup(func() {
		restore(t, caCertificatesBindingFile)
		testutil.Eventually(t, caCertsPolicy+" to refuse again", refused(bad, metav1.StatusReasonForbidden, certUsagesMessage))
	})
	binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: caCertsPolicy}}
	if err := admin.Delete(t.Context(), binding); err != nil {
		t.Fatalf("delete ValidatingAdmissionPolicyBinding %s: %v", caCertsPolicy, err)
	}
	testutil.Eventually(t, "a Certificate with dnsNames to be admitted without the binding", accepted(bad))
	createOwned(t, bad)
	restore(t, caCertificatesBindingFile)
	testutil.Eventually(t, caCertsPolicy+" to refuse again", refused(bad, metav1.StatusReasonForbidden, certUsagesMessage))

	tests := []struct {
		name   string
		obj    *unstructured.Unstructured
		edit   func(u *unstructured.Unstructured)
		substr string
	}{
		{name: "a label on a stored Certificate that breaks a rule", obj: bad, edit: func(u *unstructured.Unstructured) { u.SetLabels(map[string]string{"touched": "yes"}) }},
		{name: "a spec change on a stored Certificate that breaks a rule", obj: bad, edit: func(u *unstructured.Unstructured) { u.Object["spec"].(map[string]any)["commonName"] = "router-2" }, substr: certUsagesMessage},
		{name: "a label on a caller's Certificate", obj: good, edit: func(u *unstructured.Unstructured) { u.SetLabels(map[string]string{"touched": "yes"}) }},
		{name: "a new duration on a caller's Certificate", obj: good, edit: func(u *unstructured.Unstructured) { u.Object["spec"].(map[string]any)["duration"] = "720h" }},
		{name: "isCA added to a caller's Certificate", obj: good, edit: func(u *unstructured.Unstructured) { u.Object["spec"].(map[string]any)["isCA"] = true }, substr: "spec.isCA " + fieldsMessage},
		{name: "dnsNames added to a caller's Certificate", obj: good, edit: func(u *unstructured.Unstructured) {
			u.Object["spec"].(map[string]any)["dnsNames"] = []any{"router.example.com"}
		}, substr: certUsagesMessage},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cur := &unstructured.Unstructured{}
			cur.SetGroupVersionKind(tc.obj.GroupVersionKind())
			if err := admin.Get(t.Context(), client.ObjectKeyFromObject(tc.obj), cur); err != nil {
				t.Fatalf("get Certificate %s: %v", tc.obj.GetName(), err)
			}
			tc.edit(cur)
			expect(t, admin.Update(t.Context(), cur, client.DryRunAll), tc.substr)
		})
	}
}

// TestCAPods runs fiberd-ca-pods on the csi volumes a Pod may hold.
func TestCAPods(t *testing.T) {
	ns := namespace(t)
	server := serverAttributes(ns)
	caller := clientAttributes()
	tests := []struct {
		name   string
		obj    *unstructured.Unstructured
		substr string
	}{
		{name: "a grant Pod's serving leaf", obj: csiPod(ns, server)},
		{name: "a caller's client leaf", obj: csiPod(ns, caller)},
		{name: "a caller's client leaf in fiberd-system", obj: csiPod(controllerNamespace, caller)},
		{name: "a client leaf of 2160h", obj: csiPod(ns, withAttribute(caller, attrDuration, "2160h"))},
		{name: "a client leaf named after its Pod", obj: csiPod(ns, withAttribute(caller, attrCommonName, "${POD_NAME}"))},
		{name: "every other allowed attribute", obj: csiPod(ns, withAttributes(server, map[string]any{
			attrPrefix + "key-algorithm":    "ECDSA",
			attrPrefix + "key-size":         "256",
			attrPrefix + "key-encoding":     "PKCS8",
			attrPrefix + "fs-group":         "2000",
			attrPrefix + "certificate-file": "c.pem",
			attrPrefix + "privatekey-file":  "k.pem",
			attrPrefix + "ca-file":          "ca.pem",
		}))},
		{name: "a grant name of one character", obj: csiPod(ns, withAttribute(server, attrDNSNames, "fiberd-a."+ns+".svc,fiberd-a."+ns+".svc.cluster.local"))},
		{name: "a Pod with no volumes", obj: podWithVolumes(ns)},
		{name: "a volume of another issuer", obj: csiPod(ns, withAttribute(withAttribute(server, attrIssuerName, "team-a-ca"), attrDNSNames, "${POD_NAMESPACE}.example.com"))},
		{name: "an emptyDir beside a serving leaf", obj: podWithVolumes(ns, map[string]any{"name": "data", "emptyDir": map[string]any{}}, csiVolume("tls", server))},

		{name: "an attribute outside the list", obj: csiPod(ns, withAttribute(caller, attrPrefix+"uri-sans", "spiffe://a/b")), substr: attrKeysMessage},
		{name: "an IP address", obj: csiPod(ns, withAttribute(server, attrPrefix+"ip-sans", "10.0.0.1")), substr: attrKeysMessage},
		{name: "isCA", obj: csiPod(ns, withAttribute(caller, attrPrefix+"is-ca", "true")), substr: attrKeysMessage},
		{name: "an attribute of another driver", obj: csiPod(ns, withAttribute(caller, "example.com/color", "blue")), substr: attrKeysMessage},
		{name: "a namespaced Issuer", obj: csiPod(ns, withAttribute(caller, attrIssuerKind, "Issuer")), substr: issuerKindMessage},
		{name: "no issuer-kind", obj: csiPod(ns, withAttribute(caller, attrIssuerKind, "")), substr: issuerKindMessage},
		{name: "a common name from the namespace", obj: csiPod(ns, withAttribute(caller, attrCommonName, "${POD_NAMESPACE}")), substr: dollarMessage},
		{name: "a common name that only starts with the Pod name", obj: csiPod(ns, withAttribute(caller, attrCommonName, "${POD_NAME}.x")), substr: dollarMessage},
		{name: "dns-names with the namespace expanded", obj: csiPod(ns, withAttribute(server, attrDNSNames, "fiberd-web.${POD_NAMESPACE}.svc,fiberd-web.${POD_NAMESPACE}.svc.cluster.local")), substr: dollarMessage},
		{name: "no key-usages", obj: csiPod(ns, withAttribute(caller, attrKeyUsages, "")), substr: keyUsagesMessage},
		{name: "both usages", obj: csiPod(ns, withAttribute(server, attrKeyUsages, "server auth,client auth")), substr: keyUsagesMessage},
		{name: "the driver's default usages", obj: csiPod(ns, withAttribute(caller, attrKeyUsages, "digital signature,key encipherment")), substr: keyUsagesMessage},

		{name: "a serving leaf in fiberd-system", obj: csiPod(controllerNamespace, serverAttributes(controllerNamespace)), substr: podNamespaceMessage},
		{name: "a serving leaf with one name", obj: csiPod(ns, withAttribute(server, attrDNSNames, "fiberd-web."+ns+".svc")), substr: dnsNamesMessage},
		{name: "a serving leaf with a third name", obj: csiPod(ns, withAttribute(server, attrDNSNames, "fiberd-web."+ns+".svc,fiberd-web."+ns+".svc.cluster.local,fiberd-web")), substr: dnsNamesMessage},
		{name: "a serving leaf with no names", obj: csiPod(ns, withAttribute(server, attrDNSNames, "")), substr: dnsNamesMessage},
		{name: "a serving leaf for another namespace", obj: csiPod(ns, withAttribute(server, attrDNSNames, "fiberd-web.other.svc,fiberd-web.other.svc.cluster.local")), substr: dnsNamesMessage},
		{name: "a serving leaf for the controller", obj: csiPod(ns, withAttribute(server, attrDNSNames, "fiberd-k8s-controller.fiberd-system.svc,fiberd-k8s-controller.fiberd-system.svc.cluster.local")), substr: dnsNamesMessage},
		{name: "a serving leaf for a Service not named by a grant", obj: csiPod(ns, withAttribute(server, attrDNSNames, "web."+ns+".svc,web."+ns+".svc.cluster.local")), substr: dnsNamesMessage},
		{name: "a serving leaf with a wildcard", obj: csiPod(ns, withAttribute(server, attrDNSNames, "*."+ns+".svc,*."+ns+".svc.cluster.local")), substr: dnsNamesMessage},
		{name: "a serving leaf whose dot is any character", obj: csiPod(ns, withAttribute(server, attrDNSNames, "fiberd-webx"+ns+"xsvc,fiberd-webx"+ns+"xsvc.cluster.local")), substr: dnsNamesMessage},
		{name: "a serving leaf whose second name is another Service", obj: csiPod(ns, withAttribute(server, attrDNSNames, "fiberd-web."+ns+".svc,fiberd-api."+ns+".svc.cluster.local")), substr: dnsNamesMessage},
		{name: "a serving leaf with the names swapped", obj: csiPod(ns, withAttribute(server, attrDNSNames, "fiberd-web."+ns+".svc.cluster.local,fiberd-web."+ns+".svc")), substr: dnsNamesMessage},
		{name: "a serving leaf with no duration", obj: csiPod(ns, withAttribute(server, attrDuration, "")), substr: podServerLifeMessage},
		{name: "a serving leaf of 25h", obj: csiPod(ns, withAttribute(server, attrDuration, "25h")), substr: podServerLifeMessage},
		{name: "a client leaf with dns-names", obj: csiPod(ns, withAttribute(caller, attrDNSNames, "router.example.com")), substr: clientDNSMessage},
		{name: "a client leaf of 2161h", obj: csiPod(ns, withAttribute(caller, attrDuration, "2161h")), substr: podClientLifeMessage},
		{name: "a client leaf of 1d", obj: csiPod(ns, withAttribute(caller, attrDuration, "1d")), substr: podDurationMessage},
		{name: "a serving leaf of 1d", obj: csiPod(ns, withAttribute(server, attrDuration, "1d")), substr: podDurationMessage},
		{name: "a serving leaf of 1.5h", obj: csiPod(ns, withAttribute(server, attrDuration, "1.5h")), substr: podDurationMessage},
		{name: "a serving leaf of 23h59m", obj: csiPod(ns, withAttribute(server, attrDuration, "23h59m"))},
		{name: "a client leaf with a wildcard common name", obj: csiPod(ns, withAttribute(caller, attrCommonName, "*.example.com")), substr: podWildcardMessage},
		{name: "a serving leaf with a wildcard common name", obj: csiPod(ns, withAttribute(server, attrCommonName, "*")), substr: podWildcardMessage},
		{name: "a volume of another driver naming fiberd-ca", obj: podWithVolumes(ns, map[string]any{
			"name": "tls",
			"csi":  map[string]any{"driver": "example.com", "volumeAttributes": withAttribute(caller, attrIssuerKind, "Issuer")},
		}), substr: issuerKindMessage},
		{name: "a bad volume beside a good one", obj: podWithVolumes(ns, csiVolume("tls", server), csiVolume("client", withAttribute(caller, attrDNSNames, "router.example.com"))), substr: clientDNSMessage},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			expect(t, dryRunCreate(t.Context(), tc.obj), tc.substr)
		})
	}
}

// TestCABuiltPods creates the Pod the builder writes for the sample grant
// on every runtime, so fiberd-ca-pods admits what the controller asks for.
func TestCABuiltPods(t *testing.T) {
	ns := namespace(t)
	for _, rt := range []v1alpha1.Runtime{v1alpha1.RuntimeProc, v1alpha1.RuntimeRunc, v1alpha1.RuntimeGvisor, v1alpha1.RuntimeHyperlight} {
		t.Run(string(rt), func(t *testing.T) {
			cg := &v1alpha1.CapacityGrant{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(readSample(t, ns).Object, cg); err != nil {
				t.Fatal(err)
			}
			cg.Name = "web"
			cg.Spec.Runtime = rt
			if rt == v1alpha1.RuntimeHyperlight {
				cg.Spec.HelperImage = "ghcr.io/azure/fiberd-helper:0.4.0@" + testDigest
			}
			tmpl, err := podtemplate.Build(cg, cg.Spec.PodTemplate.Spec.Containers[0].Image, workload.ObjectName(cg), podtemplate.Options{
				IssuerURL: "https://fiberd-k8s-controller.fiberd-system.svc",
				Lease:     10 * time.Minute,
			})
			if err != nil {
				t.Fatalf("podtemplate.Build: %v", err)
			}
			pod := &corev1.Pod{ObjectMeta: *tmpl.ObjectMeta.DeepCopy(), Spec: tmpl.Spec}
			pod.Namespace = ns
			pod.GenerateName = workload.ObjectName(cg) + "-"
			expectAdmitted(t, admin.Create(t.Context(), pod, client.DryRunAll))
		})
	}
}
