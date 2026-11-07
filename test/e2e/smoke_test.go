//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	grantv1 "github.com/Azure/fiberd/api/grant/v1"
	"github.com/Azure/fiberd/pkg/tlsconf"
)

// The harness passes these. A missing one fails the test, since a skip here
// would pass a run that proved nothing.
const (
	envTarget     = "FIBERD_E2E_TARGET"
	envIssuer     = "FIBERD_E2E_ISSUER"
	envIssuerName = "FIBERD_E2E_ISSUER_NAME"
	envCallerDir  = "FIBERD_E2E_CALLER_DIR"
	envNamespace  = "FIBERD_E2E_NAMESPACE"
	envGrant      = "FIBERD_E2E_GRANT"
	envPod        = "FIBERD_E2E_POD"
	envPodUID     = "FIBERD_E2E_POD_UID"
)

const (
	// callTimeout bounds each call. A clone forks a warm template, so this
	// only needs to outlast a loaded CI runner.
	callTimeout = 30 * time.Second

	// grantWait bounds the wait for the Pod to show in the route's answer,
	// which reads the controller's cache.
	grantWait = time.Minute

	// emptyRetry stays above the ten seconds the caller route takes to earn
	// one more request, so asking again is never refused.
	emptyRetry = 11 * time.Second
)

// callerGrant is the part of a caller grant this test reads.
type callerGrant struct {
	Token    string `json:"token"`
	UID      string `json:"uid"`
	Audience string `json:"audience"`
}

// callerAnswer is the body of POST /v1/caller-grants.
type callerAnswer struct {
	ServerName string        `json:"serverName"`
	Grants     []callerGrant `json:"grants"`
}

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is not set, run this through hack/tests/kind/conform.sh smoke", name)
	}
	return v
}

// callerTLS is the caller's mutual TLS, verifying a server under name.
func callerTLS(t *testing.T, name string) *tls.Config {
	t.Helper()
	dir := requireEnv(t, envCallerDir)
	cfg, err := tlsconf.Client(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
	if err != nil {
		t.Fatalf("load the caller's certificate: %v", err)
	}
	cfg.ServerName = name
	return cfg
}

// askIssuer posts once to the caller route. A positive wait means the route
// refused for now and says when to ask again.
func askIssuer(ctx context.Context, client *http.Client, url string, body []byte) (answer callerAnswer, wait time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return answer, 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return answer, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return answer, 0, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return answer, 0, json.Unmarshal(raw, &answer)
	case http.StatusTooManyRequests:
		seconds, err := strconv.Atoi(resp.Header.Get("Retry-After"))
		if err != nil {
			return answer, 0, fmt.Errorf("a 429 with no usable Retry-After: %w", err)
		}
		return answer, time.Duration(seconds) * time.Second, nil
	default:
		return answer, 0, fmt.Errorf("status %d: %s", resp.StatusCode, raw)
	}
}

// fetchGrants asks the caller route for the grant's Pods until it names one.
func fetchGrants(t *testing.T) callerAnswer {
	t.Helper()
	client := &http.Client{
		Timeout:   callTimeout,
		Transport: &http.Transport{TLSClientConfig: callerTLS(t, requireEnv(t, envIssuerName))},
	}
	t.Cleanup(client.CloseIdleConnections)
	body, err := json.Marshal(map[string]string{"namespace": requireEnv(t, envNamespace), "name": requireEnv(t, envGrant)})
	if err != nil {
		t.Fatalf("encode the request: %v", err)
	}
	url := "https://" + requireEnv(t, envIssuer) + "/v1/caller-grants"

	ctx, cancel := context.WithTimeout(t.Context(), grantWait)
	defer cancel()
	for {
		answer, wait, err := askIssuer(ctx, client, url, body)
		if err != nil {
			t.Fatalf("ask the caller route: %v", err)
		}
		if wait == 0 {
			if len(answer.Grants) > 0 {
				return answer
			}
			wait = emptyRetry
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the caller route named no Pod within %s", grantWait)
		case <-time.After(wait):
		}
	}
}

// podGrant checks that the answer names the one Pod and returns its grant.
func podGrant(t *testing.T, answer callerAnswer) string {
	t.Helper()
	if len(answer.Grants) != 1 {
		t.Fatalf("the caller route gave %d grants, want 1: %+v", len(answer.Grants), answer.Grants)
	}
	got := answer.Grants[0]
	for _, c := range []struct{ name, got, want string }{
		{"serverName", answer.ServerName, "fiberd-" + requireEnv(t, envGrant) + "." + requireEnv(t, envNamespace) + ".svc"},
		{"audience", got.Audience, requireEnv(t, envPod)},
		{"uid", got.UID, requireEnv(t, envPodUID)},
	} {
		if c.got != c.want {
			t.Fatalf("the caller route's %s = %q, want %q", c.name, c.got, c.want)
		}
	}
	return got.Token
}

// dial connects to the Pod over mutual TLS. The target is a NodePort, so only
// serverName ties the connection to the Pod. A caller grant's address is a Pod
// IP the host may not reach, so this does not use it.
func dial(t *testing.T, serverName string) grantv1.FibersClient {
	t.Helper()
	conn, err := grpc.NewClient(requireEnv(t, envTarget), grpc.WithTransportCredentials(credentials.NewTLS(callerTLS(t, serverName))))
	if err != nil {
		t.Fatalf("dial the Pod: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return grantv1.NewFibersClient(conn)
}

// TestPodGrant clones on an image with no test hooks, with the grant the caller
// route minted for the Pod. fiberd admits it only because its UID is the Pod's
// and it names the key of this connection, which is the one path conformance
// cannot cover, since its image lifts the UID check and it mints its own grants.
func TestPodGrant(t *testing.T) {
	answer := fetchGrants(t)
	token := podGrant(t, answer)
	api := dial(t, answer.ServerName)
	session := fmt.Sprintf("smoke-%d", time.Now().UnixNano())

	steps := []struct {
		name     string
		release  bool
		wantKind grantv1.CloneKind
		wantCode codes.Code
	}{
		{name: "a first clone creates the fiber", wantKind: grantv1.CloneKind_CREATE},
		{name: "a repeat attaches to it", wantKind: grantv1.CloneKind_ATTACH},
		{name: "a release ends it", release: true, wantCode: codes.OK},
		{name: "a second release finds nothing", release: true, wantCode: codes.NotFound},
	}

	var fiberID string
	for _, s := range steps {
		ok := t.Run(s.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
			defer cancel()

			if s.release {
				_, err := api.Release(ctx, &grantv1.ReleaseRequest{FiberId: fiberID, Discard: true})
				if got := status.Code(err); got != s.wantCode {
					t.Fatalf("release code = %v, want %v (err: %v)", got, s.wantCode, err)
				}
				return
			}

			got, err := api.Clone(ctx, &grantv1.CloneRequest{GrantJwt: token, Session: session})
			if err != nil {
				t.Fatalf("clone: %v", err)
			}
			if got.GetKind() != s.wantKind {
				t.Fatalf("clone kind = %v, want %v", got.GetKind(), s.wantKind)
			}
			if fiberID != "" && got.GetFiberId() != fiberID {
				t.Fatalf("clone answered fiber %q, want %q", got.GetFiberId(), fiberID)
			}
			fiberID = got.GetFiberId()
		})
		// Each step acts on what the one before it left.
		if !ok {
			t.FailNow()
		}
	}
}
