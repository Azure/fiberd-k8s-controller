package testcert_test

import (
	"crypto/tls"
	"crypto/x509"
	"path/filepath"
	"testing"

	"github.com/Azure/fiberd-k8s-controller/pkg/testcert"
)

const testHost = "fiberd-k8s-controller.fiberd-system.svc"

func newPair(t *testing.T) testcert.Pair {
	t.Helper()
	p, err := testcert.New(testHost)
	if err != nil {
		t.Fatalf("New(%q): %v", testHost, err)
	}
	return p
}

func TestPairVerify(t *testing.T) {
	p := newPair(t)
	cases := []struct {
		name    string
		host    string
		wantErr bool
	}{
		{name: "its own host verifies", host: testHost},
		{name: "another host is refused", host: "other.fiberd-system.svc", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.Leaf.Verify(x509.VerifyOptions{
				DNSName:   tc.host,
				Roots:     testcert.Pool(p),
				KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			})
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Errorf("Verify(%q) error = %v, want error %t", tc.host, err, tc.wantErr)
			}
		})
	}
}

func TestPairsDiffer(t *testing.T) {
	a, b := newPair(t), newPair(t)
	if a.Leaf.SerialNumber.Cmp(b.Leaf.SerialNumber) == 0 {
		t.Errorf("two pairs share serial %s, want distinct ones", a.Leaf.SerialNumber)
	}
}

// TestPairWrite reads the written files back as a server would, so the
// files and Certificate hold the same pair.
func TestPairWrite(t *testing.T) {
	p := newPair(t)
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := p.Write(certPath, keyPath); err != nil {
		t.Fatalf("Write: %v", err)
	}
	loaded, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}
	want, err := p.Certificate()
	if err != nil {
		t.Fatalf("Certificate: %v", err)
	}
	if !loaded.Leaf.Equal(want.Leaf) {
		t.Error("the written pair differs from Certificate")
	}
}
