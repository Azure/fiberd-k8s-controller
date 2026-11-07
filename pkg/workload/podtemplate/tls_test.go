package podtemplate

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"flag"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/fiberd/pkg/fiberd"
	"github.com/Azure/fiberd/pkg/tlsconf"
	"github.com/Azure/fiberd/pkg/tlsconf/tlsconftest"

	"github.com/Azure/fiberd-k8s-controller/api/v1alpha1"
)

// TestBuildServesMutualTLSOnly hands the TLS flags of every runtime's grant
// Pod to fiberd's own TLS setup, with the files the csi-driver would write
// into the tls volume, and dials the result. Only a client certificate
// from the fiberd CA gets through, over the Service name the Pod's
// certificate carries. A registry zygote's flags sit beside the TLS flags
// and change none of them.
func TestBuildServesMutualTLSOnly(t *testing.T) {
	type grantCase struct {
		name   string
		mutate func(*v1alpha1.CapacityGrant)
	}
	runtimes := []v1alpha1.Runtime{v1alpha1.RuntimeProc, v1alpha1.RuntimeRunc, v1alpha1.RuntimeGvisor, v1alpha1.RuntimeHyperlight}
	grants := make([]grantCase, 0, len(runtimes)+1)
	for _, rt := range runtimes {
		grants = append(grants, grantCase{name: string(rt), mutate: func(cg *v1alpha1.CapacityGrant) { setRuntime(cg, rt) }})
	}
	grants = append(grants, grantCase{name: "registry zygote with a seal key", mutate: func(cg *v1alpha1.CapacityGrant) {
		cg.Spec.Zygote.Command = nil
		cg.Spec.Zygote.Registry = "ghcr.io/team-a/zygotes"
		cg.Spec.Zygote.SealKeySecretName = "team-a-seal"
	}})
	for _, g := range grants {
		t.Run(g.name, func(t *testing.T) {
			tmpl := mustBuild(t, testGrant(g.mutate), testOptions())
			var cfg fiberd.Config
			fs := flag.NewFlagSet("fiberd", flag.ContinueOnError)
			cfg.Bind(fs)
			for _, name := range []string{"home", "grant-token-file", "grant-fetch-timeout"} {
				fs.String(name, "", "bound by cmd/fiberd")
			}
			if err := fs.Parse(tmpl.Spec.Containers[0].Args); err != nil {
				t.Fatalf("fiberd refused the args: %v", err)
			}
			if cfg.API.TLS.InsecurePlaintext {
				t.Fatal("the grant Pod serves plaintext")
			}

			var dnsNames []string
			for _, v := range tmpl.Spec.Volumes {
				if v.Name == tlsVolume && v.CSI != nil {
					dnsNames = strings.Split(v.CSI.VolumeAttributes[attrDNSNames], ",")
				}
			}
			if len(dnsNames) == 0 {
				t.Fatal("the grant Pod has no csi tls volume with DNS names")
			}

			// The files the csi-driver writes, at the paths the flags name.
			dir := t.TempDir()
			ca := tlsconftest.NewCA(t, CAIssuerName)
			serving := ca.Issue(t, &x509.Certificate{Subject: pkix.Name{CommonName: "fiberd-web-0"}, DNSNames: dnsNames,
				ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
			certPath, keyPath := serving.Write(t, dir, "tls")
			caPath := filepath.Join(dir, "ca.crt")
			if err := os.WriteFile(caPath, ca.CertPEM(), 0o600); err != nil {
				t.Fatal(err)
			}
			for flagValue, file := range map[string]string{cfg.API.TLS.Cert: certPath, cfg.API.TLS.Key: keyPath, cfg.API.TLS.ClientCA: caPath} {
				if filepath.Join(tlsDir, filepath.Base(file)) != flagValue {
					t.Fatalf("fiberd reads %s, want %s in the tls volume", flagValue, filepath.Base(file))
				}
			}
			serverTLS, err := tlsconf.Server(certPath, keyPath, caPath, cfg.API.TLS.InsecurePlaintext)
			if err != nil || serverTLS == nil {
				t.Fatalf("tlsconf.Server = %v, %v, want a mutual TLS configuration", serverTLS, err)
			}

			serviceName := testServiceName + ".team-a.svc"
			other := tlsconftest.NewCA(t, "another-ca")
			cases := []struct {
				name   string
				client *tls.Config
				wantOK bool
			}{
				{name: "a client certificate from the fiberd CA", wantOK: true,
					client: &tls.Config{RootCAs: ca.Pool(), ServerName: serviceName, Certificates: []tls.Certificate{ca.Client(t, "caller").TLS()}}},
				{name: "no client certificate",
					client: &tls.Config{RootCAs: ca.Pool(), ServerName: serviceName}},
				{name: "a client certificate from another CA",
					client: &tls.Config{RootCAs: ca.Pool(), ServerName: serviceName, Certificates: []tls.Certificate{other.Client(t, "caller").TLS()}}},
				{name: "plaintext"},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					err := handshake(t, serverTLS, tc.client)
					if tc.wantOK && err != nil {
						t.Fatalf("handshake: %v", err)
					}
					if !tc.wantOK && err == nil {
						t.Fatal("the grant Pod accepted the connection")
					}
				})
			}
		})
	}
}

// handshake serves one TLS handshake with server and returns the server's
// error. A nil client dials in plaintext and sends an HTTP request.
func handshake(t *testing.T, server, client *tls.Config) error {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	result := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			result <- err
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		// The server checks the client certificate within its handshake.
		result <- tls.Server(conn, server).Handshake()
	}()

	conn, err := net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if client == nil {
		_, _ = conn.Write([]byte("GET / HTTP/1.1\r\nHost: fiberd\r\n\r\n"))
	} else {
		_ = tls.Client(conn, client).Handshake()
	}
	select {
	case err := <-result:
		return err
	case <-time.After(10 * time.Second):
		return errors.New("the server did not finish the handshake")
	}
}
