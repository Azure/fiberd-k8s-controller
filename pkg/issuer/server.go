package issuer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// The paths grant.Issuer.Handler serves. fiberd core keeps them as
// literals, so the controller names them once here and a test pins them
// to what core answers at.
const (
	// DiscoveryPath is the path of the OIDC discovery document.
	DiscoveryPath = "/.well-known/openid-configuration"
	// JWKSPath is the path of the public key set.
	JWKSPath = "/openid/v1/jwks"
)

// shutdownTimeout bounds how long a stopping Server waits for open
// connections. A grant request ends within requestTimeout, so a longer wait
// only delays the Pod's exit.
const shutdownTimeout = 10 * time.Second

// tlsHandshakeError starts the line net/http logs for a refused handshake.
const tlsHandshakeError = "http: TLS handshake error"

// Server serves the handler NewServer is given, the issuer's discovery, JWKS
// and grant routes, over TLS 1.3 as a manager Runnable on every replica.
type Server struct {
	inner manager.Server

	getCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	serving        atomic.Bool
}

var (
	_ manager.Runnable               = (*Server)(nil)
	_ manager.LeaderElectionRunnable = (*Server)(nil)
	_ healthz.Checker                = (*Server)(nil).Ready
)

// NewServer binds addr and returns the Server that serves handler on it
// once started. Binding at construction makes a taken port fail before the
// manager starts, as controller-runtime's own probe and metrics listeners
// do. getCertificate answers every handshake, so a certwatcher behind it
// rotates the certificate without a restart. ctx is the base context of
// every request, and its logger takes what net/http reports.
func NewServer(ctx context.Context, addr string, handler http.Handler, getCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)) (*Server, error) {
	if handler == nil || getCertificate == nil {
		return nil, errors.New("issuer: the server needs a handler and a certificate getter")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("issuer: listen on %s: %w", addr, err)
	}
	// controller-runtime keeps these settings for its own servers in
	// pkg/internal/httpserver, which cannot be imported.
	srv := &http.Server{
		Handler:           handler,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		MaxHeaderBytes:    1 << 20,
		IdleTimeout:       90 * time.Second,
		ReadHeaderTimeout: 32 * time.Second,
		ErrorLog:          log.New(serverLog{log: logf.FromContext(ctx)}, "", 0),
	}
	return &Server{
		inner: manager.Server{
			Name:            "issuer",
			Listener:        tls.NewListener(ln, serverTLS(getCertificate)),
			Server:          srv,
			ShutdownTimeout: ptr.To(shutdownTimeout),
		},
		getCertificate: getCertificate,
	}, nil
}

// serverTLS is the listener's TLS configuration. TLS 1.3 is the floor, and
// Go ignores cipher suite settings for it. NextProtos is set by hand
// because a pre-bound tls.Listener served with Serve never passes through
// ServeTLS, which is where Go would otherwise advertise HTTP/2.
func serverTLS(getCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)) *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS13,
		GetCertificate: getCertificate,
		NextProtos:     []string{"h2", "http/1.1"},
	}
}

// serverLog takes the lines net/http writes to its ErrorLog. Any client can
// fail a handshake, and no limit runs before one, so those lines stay below
// the default verbosity or a flood of them becomes a flood of log lines.
type serverLog struct {
	log logr.Logger
}

// Write logs one line of net/http's.
func (w serverLog) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")
	if strings.HasPrefix(line, tlsHandshakeError) {
		w.log.V(quietVerbosity).Info(line)
	} else {
		w.log.Error(nil, line)
	}
	return len(p), nil
}

// NeedLeaderElection is false because every replica must serve discovery
// and the key set, so a grant Pod reaches a follower as well as the
// leader.
func (s *Server) NeedLeaderElection() bool { return false }

// Start serves until ctx ends and returns nil after a clean shutdown.
// Ready passes only while it runs.
func (s *Server) Start(ctx context.Context) error {
	s.serving.Store(true)
	defer s.serving.Store(false)
	// The listener closes when ctx ends, but the drain may take up to the
	// shutdown timeout. A replica still ready then would stay in the
	// Service's endpoints.
	stop := context.AfterFunc(ctx, func() { s.serving.Store(false) })
	defer stop()
	return s.inner.Start(ctx)
}

// Ready is the readyz check of the listener. It passes only while Start
// runs and a serving certificate is loaded, so a replica whose certificate
// files never parsed does not report ready. The getter is asked without a
// hello, as certwatcher answers without one.
func (s *Server) Ready(*http.Request) error {
	if !s.serving.Load() {
		return errors.New("issuer: server not started")
	}
	cert, err := s.getCertificate(nil)
	if err != nil {
		return fmt.Errorf("issuer: get serving certificate: %w", err)
	}
	if cert == nil {
		return errors.New("issuer: no serving certificate loaded")
	}
	return nil
}

// Addr is the bound address. With a port of 0 it carries the port the
// kernel chose.
func (s *Server) Addr() net.Addr {
	return s.inner.Listener.Addr()
}
