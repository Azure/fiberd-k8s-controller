package issuer_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/go-cmp/cmp"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"

	"github.com/Azure/fiberd/pkg/grant"

	"github.com/Azure/fiberd-k8s-controller/pkg/issuer"
	"github.com/Azure/fiberd-k8s-controller/pkg/testcert"
)

const (
	// waitTimeout bounds every wait on something local, such as the server
	// reporting ready or certwatcher reading a file. The bound is only
	// reached on failure, so a generous one costs a passing run nothing.
	waitTimeout  = 5 * time.Second
	pollInterval = 10 * time.Millisecond
	// watchInterval is certwatcher's own poll. It catches the changes
	// fsnotify does not report, such as a rename over a symlink.
	watchInterval = 50 * time.Millisecond
	// holdFor is how long a change that must not be served is watched for.
	// A few polls are enough for certwatcher to have tried the files.
	holdFor = 4 * watchInterval
)

// newPair is a self-signed serving certificate for testIssuerHost.
func newPair(t *testing.T) testcert.Pair {
	t.Helper()
	p, err := testcert.New(testIssuerHost)
	if err != nil {
		t.Fatalf("testcert.New(%q): %v", testIssuerHost, err)
	}
	return p
}

// certificate is the pair as a getter serves it.
func certificate(t *testing.T, p testcert.Pair) *tls.Certificate {
	t.Helper()
	cert, err := p.Certificate()
	if err != nil {
		t.Fatalf("Certificate: %v", err)
	}
	return &cert
}

// pairGetter serves the pair on every handshake, as certwatcher does once
// it has read a pair.
func pairGetter(t *testing.T, p testcert.Pair) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	t.Helper()
	cert := certificate(t, p)
	return func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return cert, nil }
}

// writePair puts the pair at certPath and keyPath, as a cert-manager
// Secret lays it out.
func writePair(t *testing.T, p testcert.Pair, certPath, keyPath string) {
	t.Helper()
	if err := p.Write(certPath, keyPath); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func symlink(t *testing.T, target, path string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("symlink %s -> %s: %v", path, target, err)
	}
}

func rename(t *testing.T, from, to string) {
	t.Helper()
	if err := os.Rename(from, to); err != nil {
		t.Fatalf("rename %s to %s: %v", from, to, err)
	}
}

func removeAll(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("remove %s: %v", path, err)
	}
}

// clientTLS trusts each pair and expects testIssuerHost, so a handshake
// here verifies as strictly as a grant Pod's does.
func clientTLS(pairs ...testcert.Pair) *tls.Config {
	return &tls.Config{RootCAs: testcert.Pool(pairs...), ServerName: testIssuerHost}
}

// httpsClient speaks HTTP/2 through cfg. ForceAttemptHTTP2 is needed
// because a Transport with its own TLS config stays on HTTP/1.1 otherwise.
func httpsClient(t *testing.T, cfg *tls.Config) *http.Client {
	t.Helper()
	tr := &http.Transport{TLSClientConfig: cfg, ForceAttemptHTTP2: true}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: waitTimeout}
}

// fetch gets path from s and returns the response with its body read.
func fetch(t *testing.T, c *http.Client, s *issuer.Server, path string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+s.Addr().String()+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read GET %s: %v", path, err)
	}
	return resp, body
}

// handshake dials addr once through cfg and returns the connection after
// the handshake. Every call is a fresh handshake, since cfg has no session
// cache, so the server presents its current certificate each time.
func handshake(ctx context.Context, addr string, cfg *tls.Config) (*tls.Conn, error) {
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: waitTimeout}, Config: cfg}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	return conn.(*tls.Conn), nil
}

// servedSerial is the serial number of the leaf the server at addr
// presents on a fresh handshake.
func servedSerial(ctx context.Context, addr string, cfg *tls.Config) (*big.Int, error) {
	conn, err := handshake(ctx, addr, cfg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	return conn.ConnectionState().PeerCertificates[0].SerialNumber, nil
}

// waitFor polls cond until it holds or waitTimeout passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(pollInterval)
	}
}

// stays polls cond for holdFor and fails as soon as it stops holding. It
// is how a change that must not happen is observed.
func stays(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(holdFor)
	for time.Now().Before(deadline) {
		if !cond() {
			t.Fatalf("%s stopped holding", what)
		}
		time.Sleep(pollInterval)
	}
}

// issuerHandler is core's handler over ring at testIssuerURL.
func issuerHandler(ring grant.KeyRing) http.Handler {
	return (&grant.Issuer{Ring: ring, URL: testIssuerURL}).Handler()
}

// newRing is a ring of two EdDSA keys, one signing and one retired. Core
// publishes both in the JWKS. NewKeyRing makes one key and only a store
// adds more, so the retired key is appended as a KeyRing holds it.
func newRing(t *testing.T) grant.KeyRing {
	t.Helper()
	ring, err := grant.NewKeyRing(jose.EdDSA)
	if err != nil {
		t.Fatalf("NewKeyRing: %v", err)
	}
	retired, err := grant.GenerateKey(jose.EdDSA)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	at := time.Now()
	return append(ring, grant.StoredKey{JWK: *retired, SignedUntil: &at})
}

// newServer binds a loopback port for handler behind getCertificate. Only
// Start closes the listener, so every test that calls newServer also calls
// startServer.
func newServer(t *testing.T, handler http.Handler, getCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)) *issuer.Server {
	t.Helper()
	s, err := issuer.NewServer(t.Context(), "127.0.0.1:0", handler, getCertificate)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

// startServer runs s until the test ends or stop is called, and returns
// once s reports ready. done carries what Start returned. The cleanup
// waits for Start to return, so no server goroutine outlives its test, and
// it waits on returned rather than done so a test may drain done itself.
func startServer(t *testing.T, s *issuer.Server) (stop context.CancelFunc, done <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		errc <- s.Start(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-returned:
		case <-time.After(waitTimeout):
			t.Error("Start did not return after the test ended")
		}
	})
	waitFor(t, "the server to report ready", func() bool { return s.Ready(nil) == nil })
	return cancel, errc
}

// certGetter is a certificate getter whose answer a test replaces while
// the server runs.
type certGetter struct {
	mu   sync.Mutex
	cert *tls.Certificate
	err  error
}

func (g *certGetter) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cert, g.err
}

func (g *certGetter) set(cert *tls.Certificate, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cert, g.err = cert, err
}

// discovery is the part of the OIDC discovery document core's grant.Cache
// checks. The issuer must equal the configured URL and jwks_uri must sit
// under it.
type discovery struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

// The file names of a cert-manager Secret's pair.
const (
	certName = "tls.crt"
	keyName  = "tls.key"
)

// certFiles is where a certwatcher reads the pair from.
type certFiles struct {
	dir  string
	cert string
	key  string
}

// certFilesIn is the pair's paths under dir.
func certFilesIn(dir string) certFiles {
	return certFiles{dir: dir, cert: filepath.Join(dir, certName), key: filepath.Join(dir, keyName)}
}

// plainLayout writes the pair as two files under dir.
func plainLayout(t *testing.T, dir string, first testcert.Pair) certFiles {
	t.Helper()
	f := certFilesIn(dir)
	writePair(t, first, f.cert, f.key)
	return f
}

// kubeletLayout lays dir out as the kubelet does a Secret volume. The pair
// sits in a versioned directory, which the kubelet names by a timestamp,
// ..data points at it, and the file names are symlinks through ..data.
func kubeletLayout(t *testing.T, dir string, first testcert.Pair) certFiles {
	t.Helper()
	f := certFilesIn(dir)
	versioned := certFilesIn(filepath.Join(dir, "..v1"))
	mkdir(t, versioned.dir)
	writePair(t, first, versioned.cert, versioned.key)
	symlink(t, "..v1", filepath.Join(dir, "..data"))
	symlink(t, filepath.Join("..data", certName), f.cert)
	symlink(t, filepath.Join("..data", keyName), f.key)
	return f
}

// kubeletSwap publishes next as the kubelet does an updated Secret. A new
// versioned directory is written, a fresh ..data_tmp symlink is renamed
// over ..data, and the old directory is removed. The watched names never
// change, so only a reread sees the new pair.
func kubeletSwap(t *testing.T, f certFiles, next testcert.Pair) {
	t.Helper()
	versioned := certFilesIn(filepath.Join(f.dir, "..v2"))
	mkdir(t, versioned.dir)
	writePair(t, next, versioned.cert, versioned.key)
	symlink(t, "..v2", filepath.Join(f.dir, "..data_tmp"))
	rename(t, filepath.Join(f.dir, "..data_tmp"), filepath.Join(f.dir, "..data"))
	removeAll(t, filepath.Join(f.dir, "..v1"))
}

func TestServerTLSVersion(t *testing.T) {
	pair := newPair(t)
	s := newServer(t, issuerHandler(newRing(t)), pairGetter(t, pair))
	startServer(t, s)

	cases := []struct {
		name string
		// maxVersion caps what the client offers.
		maxVersion uint16
		// wantRefused means the handshake must fail.
		wantRefused bool
	}{
		{name: "TLS 1.2 is refused", maxVersion: tls.VersionTLS12, wantRefused: true},
		{name: "TLS 1.3 handshakes", maxVersion: tls.VersionTLS13},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := clientTLS(pair)
			cfg.MaxVersion = tc.maxVersion
			conn, err := handshake(t.Context(), s.Addr().String(), cfg)
			if tc.wantRefused {
				if err == nil {
					_ = conn.Close()
					t.Fatal("handshake succeeded, want a refusal")
				}
				if !strings.Contains(err.Error(), "protocol version") {
					t.Fatalf("handshake error %q, want a protocol version refusal", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("handshake: %v", err)
			}
			defer func() { _ = conn.Close() }()
			if v := conn.ConnectionState().Version; v != tls.VersionTLS13 {
				t.Errorf("negotiated version = %#x, want TLS 1.3", v)
			}
		})
	}
}

func TestServerHTTP2(t *testing.T) {
	pair := newPair(t)
	s := newServer(t, issuerHandler(newRing(t)), pairGetter(t, pair))
	startServer(t, s)

	resp, _ := fetch(t, httpsClient(t, clientTLS(pair)), s, issuer.DiscoveryPath)
	if resp.ProtoMajor != 2 {
		t.Errorf("response protocol = %s, want HTTP/2", resp.Proto)
	}
}

// TestServerServesIssuer pins DiscoveryPath and JWKSPath to the paths
// core's handler answers at.
func TestServerServesIssuer(t *testing.T) {
	pair := newPair(t)
	ring := newRing(t)
	s := newServer(t, issuerHandler(ring), pairGetter(t, pair))
	startServer(t, s)
	c := httpsClient(t, clientTLS(pair))

	cases := []struct {
		name string
		path string
		// check reads the body core answered with.
		check func(t *testing.T, body []byte)
	}{
		{
			name: "discovery names the https issuer and the JWKS under it",
			path: issuer.DiscoveryPath,
			check: func(t *testing.T, body []byte) {
				var got discovery
				if err := json.Unmarshal(body, &got); err != nil {
					t.Fatalf("decode discovery %q: %v", body, err)
				}
				want := discovery{Issuer: testIssuerURL, JWKSURI: testIssuerURL + issuer.JWKSPath}
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("discovery mismatch (-want +got):\n%s", diff)
				}
			},
		},
		{
			name: "JWKS lists every ring key by kid",
			path: issuer.JWKSPath,
			check: func(t *testing.T, body []byte) {
				var set jose.JSONWebKeySet
				if err := json.Unmarshal(body, &set); err != nil {
					t.Fatalf("decode JWKS %q: %v", body, err)
				}
				got := make([]string, 0, len(set.Keys))
				for _, k := range set.Keys {
					got = append(got, k.KeyID)
				}
				if diff := cmp.Diff(kids(ring), got); diff != "" {
					t.Errorf("JWKS kids mismatch (-want +got):\n%s", diff)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := fetch(t, c, s, tc.path)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200, body %q", tc.path, resp.StatusCode, body)
			}
			tc.check(t, body)
		})
	}
}

func TestServerReady(t *testing.T) {
	pair := newPair(t)
	cert := certificate(t, pair)
	g := &certGetter{cert: cert}
	s := newServer(t, issuerHandler(newRing(t)), g.get)

	if err := s.Ready(nil); err == nil || !strings.Contains(err.Error(), "not started") {
		t.Errorf("Ready before Start = %v, want an error saying not started", err)
	}
	startServer(t, s)

	cases := []struct {
		name string
		cert *tls.Certificate
		err  error
		// wantErr is a substring of the error. Empty means ready.
		wantErr string
	}{
		{name: "certificate loaded", cert: cert},
		{name: "no certificate loaded", wantErr: "no serving certificate loaded"},
		{name: "getter fails", err: errors.New("unreadable"), wantErr: "unreadable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g.set(tc.cert, tc.err)
			err := s.Ready(nil)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Ready = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Ready = nil, want an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Ready error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestNewServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	taken := ln.Addr().String()
	handler := issuerHandler(newRing(t))
	getter := pairGetter(t, newPair(t))

	cases := []struct {
		name           string
		addr           string
		handler        http.Handler
		getCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)
		// wantInErr is a substring of the error. Empty skips the check.
		wantInErr string
	}{
		{name: "taken port", addr: taken, handler: handler, getCertificate: getter, wantInErr: taken},
		{name: "nil handler", addr: "127.0.0.1:0", getCertificate: getter},
		{name: "nil getter", addr: "127.0.0.1:0", handler: handler},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := issuer.NewServer(t.Context(), tc.addr, tc.handler, tc.getCertificate)
			if err == nil {
				t.Fatalf("NewServer on %s succeeded, want an error", tc.addr)
			}
			if !strings.Contains(err.Error(), tc.wantInErr) {
				t.Errorf("NewServer error %q does not name %s", err, tc.wantInErr)
			}
		})
	}
}

func TestServerStopsWithContext(t *testing.T) {
	pair := newPair(t)
	s := newServer(t, issuerHandler(newRing(t)), pairGetter(t, pair))
	stop, done := startServer(t, s)
	addr := s.Addr().String()

	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start = %v after the context ended, want nil", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Start did not return after the context ended")
	}
	if err := s.Ready(nil); err == nil {
		t.Error("Ready = nil after Start returned, want an error")
	}
	d := net.Dialer{Timeout: waitTimeout}
	conn, err := d.DialContext(t.Context(), "tcp", addr)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("dial %s succeeded after shutdown, want a refusal", addr)
	}
}

func TestServerNeedLeaderElection(t *testing.T) {
	s := newServer(t, issuerHandler(newRing(t)), pairGetter(t, newPair(t)))
	startServer(t, s)
	if s.NeedLeaderElection() {
		t.Error("NeedLeaderElection = true, want false so every replica serves")
	}
}

// TestServerReloadsCertificate drives controller-runtime's certwatcher,
// the getter the manager wiring injects, through the changes a Secret
// volume sees and checks what the server presents on a fresh handshake.
func TestServerReloadsCertificate(t *testing.T) {
	cases := []struct {
		name string
		// layout writes first under dir and returns the paths certwatcher
		// watches.
		layout func(t *testing.T, dir string, first testcert.Pair) certFiles
		// change puts next in place of first, or breaks the files.
		change func(t *testing.T, f certFiles, next testcert.Pair)
		// keepsFirst means the change must not replace what is served.
		keepsFirst bool
	}{
		{
			name:   "a pair written over the same names is served",
			layout: plainLayout,
			change: func(t *testing.T, f certFiles, next testcert.Pair) { writePair(t, next, f.cert, f.key) },
		},
		{
			name:   "the kubelet's atomic swap is served",
			layout: kubeletLayout,
			change: kubeletSwap,
		},
		{
			name:   "a pair that does not parse keeps the old one served",
			layout: plainLayout,
			change: func(t *testing.T, f certFiles, _ testcert.Pair) {
				writeFile(t, f.cert, []byte("not a certificate"))
				writeFile(t, f.key, []byte("not a key"))
			},
			keepsFirst: true,
		},
		{
			name:   "files moved away keep the old one served",
			layout: plainLayout,
			change: func(t *testing.T, f certFiles, _ testcert.Pair) {
				rename(t, f.cert, f.cert+".old")
				rename(t, f.key, f.key+".old")
			},
			keepsFirst: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first, next, last := newPair(t), newPair(t), newPair(t)
			f := tc.layout(t, t.TempDir(), first)

			cw, err := certwatcher.New(f.cert, f.key)
			if err != nil {
				t.Fatalf("certwatcher.New: %v", err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			watchDone := make(chan error, 1)
			go func() { watchDone <- cw.WithWatchInterval(watchInterval).Start(ctx) }()

			s := newServer(t, issuerHandler(newRing(t)), cw.GetCertificate)
			startServer(t, s)
			addr := s.Addr().String()
			cfg := clientTLS(first, next, last)
			serves := func(pair testcert.Pair) bool {
				sn, hsErr := servedSerial(t.Context(), addr, cfg)
				if hsErr != nil {
					t.Logf("handshake with %s: %v", addr, hsErr)
					return false
				}
				return sn.Cmp(pair.Leaf.SerialNumber) == 0
			}
			if !serves(first) {
				t.Fatal("the first pair is not served")
			}

			tc.change(t, f, next)
			if tc.keepsFirst {
				stays(t, "the first pair being served", func() bool { return serves(first) })
				// The watcher must survive the bad read, so a good pair
				// written afterwards is served.
				writePair(t, last, f.cert, f.key)
				waitFor(t, "the last pair to be served", func() bool { return serves(last) })
			} else {
				waitFor(t, "the next pair to be served", func() bool { return serves(next) })
			}

			cancel()
			select {
			case err := <-watchDone:
				if err != nil {
					t.Errorf("certwatcher.Start = %v after the context ended, want nil", err)
				}
			case <-time.After(waitTimeout):
				t.Fatal("certwatcher did not stop after the context ended")
			}
		})
	}
}
