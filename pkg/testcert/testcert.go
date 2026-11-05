// Package testcert makes self-signed serving certificates for tests. It
// exists for tests only and nothing in a shipped binary imports it.
package testcert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"sync/atomic"
	"time"
)

// serials gives every pair its own serial number, so a handshake tells
// pairs apart.
var serials atomic.Int64

// Pair is a self-signed serving certificate, parsed for a client's trust
// pool and as PEM for the files a server reads.
type Pair struct {
	// Leaf is the parsed certificate.
	Leaf *x509.Certificate

	// CertPEM is the certificate as a cert-manager Secret's tls.crt holds it.
	CertPEM []byte

	// KeyPEM is the private key as a cert-manager Secret's tls.key holds it.
	KeyPEM []byte
}

// New returns an ECDSA P-256 pair for host, valid for an hour. It is
// backdated a minute, so a clock a little behind still accepts it.
func New(host string) (Pair, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Pair{}, fmt.Errorf("testcert: generate the key: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(serials.Add(1)),
		Subject:               pkix.Name{CommonName: host},
		DNSNames:              []string{host},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return Pair{}, fmt.Errorf("testcert: create the certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return Pair{}, fmt.Errorf("testcert: parse the certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return Pair{}, fmt.Errorf("testcert: marshal the key: %w", err)
	}
	return Pair{
		Leaf:    leaf,
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// Write puts the pair at certPath and keyPath. Both are readable by the
// owner only, as a private key file should be.
func (p Pair) Write(certPath, keyPath string) error {
	if err := os.WriteFile(certPath, p.CertPEM, 0o600); err != nil {
		return fmt.Errorf("testcert: write %s: %w", certPath, err)
	}
	if err := os.WriteFile(keyPath, p.KeyPEM, 0o600); err != nil {
		return fmt.Errorf("testcert: write %s: %w", keyPath, err)
	}
	return nil
}

// Certificate is the pair as a server's certificate getter serves it.
func (p Pair) Certificate() (tls.Certificate, error) {
	return tls.X509KeyPair(p.CertPEM, p.KeyPEM)
}

// Pool is a trust pool holding only pairs, so a client trusts nothing else.
func Pool(pairs ...Pair) *x509.CertPool {
	pool := x509.NewCertPool()
	for _, p := range pairs {
		pool.AddCert(p.Leaf)
	}
	return pool
}
